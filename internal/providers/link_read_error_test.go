package providers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ncecere/open-mcp-portal/internal/auth"
	"github.com/ncecere/open-mcp-portal/internal/authz"
	"github.com/ncecere/open-mcp-portal/internal/registry"
	"github.com/ncecere/open-mcp-portal/internal/store/dbgen"
)

// errInjected is a database failure: reading the linked platform provider
// failed, which says nothing about whether it is turned off.
var errInjected = errors.New("injected: connection reset while reading auth_providers")

// rowsDB answers sqlc's single-row queries from fixtures: rows[name] is a
// struct whose fields are scanned in order, or an error, keyed by the query's
// name and first argument; anything else answers pgx.ErrNoRows. A statement
// runs (Exec) only when rowsKey(name, "exec") is present.
type rowsDB map[string]any

func rowsKey(name string, arg any) string { return fmt.Sprintf("%s %v", name, arg) }

func queryName(sql string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(sql, "-- name: "), " ")
	return name
}

func (db rowsDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	name := queryName(sql)
	var arg any
	if len(args) > 0 {
		arg = args[0]
	}
	v, ok := db[rowsKey(name, arg)]
	if !ok {
		return fixtureRow{err: pgx.ErrNoRows}
	}
	if err, isErr := v.(error); isErr {
		return fixtureRow{err: err}
	}
	return fixtureRow{v: reflect.ValueOf(v)}
}

func (db rowsDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if _, ok := db[rowsKey(queryName(sql), "exec")]; ok {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.CommandTag{}, errors.New("rowsDB: unexpected Exec " + queryName(sql))
}

func (rowsDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("rowsDB: unexpected Query")
}

type fixtureRow struct {
	v   reflect.Value
	err error
}

func (r fixtureRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != r.v.NumField() {
		return fmt.Errorf("fixture %s has %d fields, the query scans %d", r.v.Type(), r.v.NumField(), len(dest))
	}
	for i, d := range dest {
		to := reflect.ValueOf(d).Elem()
		if to.Type() != r.v.Field(i).Type() {
			return fmt.Errorf("fixture %s field %d is %s, the query scans %s", r.v.Type(), i, r.v.Field(i).Type(), to.Type())
		}
		to.Set(r.v.Field(i))
	}
	return nil
}

// A server's own provider signs in with a platform provider's client
// (registration platform_provider). When reading that platform provider
// fails, the caller must get the database error, not ErrProviderDisabled or
// another "turned off" refusal (issue #107); a link that is really off still
// refuses as before.
func TestLinkReadErrorIsNotProviderDisabled(t *testing.T) {
	ctx := context.Background()
	s := testService(t, nil)
	linked, server := uuid.New(), uuid.New()
	p := dbgen.AuthProvider{ID: server, Slug: "server-own", Name: "Server", Kind: KindMCPServer, Enabled: true,
		LinkProviderID: uuid.NullUUID{UUID: linked, Valid: true}, ServerID: uuid.NullUUID{UUID: uuid.New(), Valid: true}}
	user := dbgen.User{ID: uuid.New(), Status: auth.StatusActive}
	session := "browser-session"
	state := "callback-state"
	db := rowsDB{
		rowsKey("GetAuthProvider", server): p,
		rowsKey("GetAuthProvider", linked): errInjected,
		rowsKey("TakeProviderAuthState", auth.Digest(state)): dbgen.ProviderAuthState{StateDigest: auth.Digest(state),
			UserID: user.ID, SessionDigest: auth.Digest(session), ProviderID: server, ExpiresAt: time.Now().Add(time.Minute)},
	}
	s.q = dbgen.New(db)
	req := &registry.Requirement{Provider: registry.Provider{ID: server}, Scopes: []string{"read"}}
	calls := map[string]func() error{
		"Credential": func() error {
			_, err := s.Credential(ctx, req, CallContext{User: user})
			return err
		},
		"ServiceCredential": func() error {
			_, err := s.ServiceCredential(ctx, req, ServiceCallContext{})
			return err
		},
		"Start": func() error {
			_, err := s.Start(ctx, authz.Actor{UserID: user.ID}, StartInput{ProviderID: server, SessionID: session, ServerBoundAllowed: true})
			return err
		},
		"Callback": func() error {
			_, err := s.Callback(ctx, CallbackInput{State: state, Code: "code", SessionID: session, User: user})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, errInjected) || errors.Is(err, ErrProviderDisabled) || errors.Is(err, errNoProvider) {
			t.Errorf("%s with a failed read of the platform provider: err = %v, want the database error", name, err)
		}
	}

	// The platform provider turned off: the refusals are unchanged.
	db[rowsKey("GetAuthProvider", linked)] = dbgen.AuthProvider{ID: linked, Slug: "platform", Enabled: false}
	if _, err := s.Credential(ctx, req, CallContext{User: user}); !errors.Is(err, ErrProviderDisabled) {
		t.Errorf("Credential with the platform provider off: err = %v, want ErrProviderDisabled", err)
	}
	if _, err := s.ServiceCredential(ctx, req, ServiceCallContext{}); !errors.Is(err, ErrProviderDisabled) {
		t.Errorf("ServiceCredential with the platform provider off: err = %v, want ErrProviderDisabled", err)
	}
	if _, err := s.Start(ctx, authz.Actor{UserID: user.ID}, StartInput{ProviderID: server, SessionID: session, ServerBoundAllowed: true}); !errors.Is(err, errNoProvider) {
		t.Errorf("Start with the platform provider off: err = %v, want errNoProvider", err)
	}
	if to, err := s.Callback(ctx, CallbackInput{State: state, Code: "code", SessionID: session, User: user}); err != nil || !strings.Contains(to, "result="+ResultError) {
		t.Errorf("Callback with the platform provider off: %q, %v, want the error result", to, err)
	}
	// The platform provider deleted: also a refusal, not an error.
	delete(db, rowsKey("GetAuthProvider", linked))
	if _, err := s.ServiceCredential(ctx, req, ServiceCallContext{}); !errors.Is(err, ErrProviderDisabled) {
		t.Errorf("ServiceCredential with the platform provider gone: err = %v, want ErrProviderDisabled", err)
	}
}
