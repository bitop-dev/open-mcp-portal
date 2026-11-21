package httpapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ncecere/open-mcp-portal/internal/httpapi"
	"github.com/ncecere/open-mcp-portal/internal/httpapi/apitypes"
	"github.com/ncecere/open-mcp-portal/internal/providers"
	"github.com/ncecere/open-mcp-portal/internal/registry"
	"github.com/ncecere/open-mcp-portal/internal/store/dbgen"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
)

func TestServiceConnectionInvalidScopeNeverFallsBack(t *testing.T) {
	e := newProvEnv(t)
	sa := createSA(t, e.owner, map[string]any{"name": "no-fallback-ci"})
	key := createSAKey(t, e.owner, sa.Id.String(), map[string]any{"name": "ci"}).Secret
	c := connectShared(t, e.owner, e.provider.Id, "no-fallback")
	code, ec, _ := assignShared(t, e.owner, c, []uuid.UUID{sa.Id}, []apitypes.ServiceConnectionGrant{{ServerId: e.server.Id, Origin: registry.OriginOf(e.server.Url), Scopes: []string{"repo"}}})
	mustCode(t, "assign", code, ec, 200, "")
	e.scalar(`UPDATE auth_providers SET narrow_on_refresh=true WHERE id=$1 RETURNING 'ok'`, e.provider.Id)
	e.oauth.Set(func(f *testutil.FakeOAuth) { f.FailRefresh = "invalid_scope" })
	serviceDenied(t, e.callTool(key, f3Tool), "provider_unavailable")
	if n := e.toolCalls("provider_token"); n != 0 {
		t.Fatal("invalid_scope fell back to stored full token")
	}
	if value := e.scalar(`SELECT (status='active' AND access_token_cipher IS NOT NULL AND refresh_token_cipher IS NOT NULL)::text FROM service_connections WHERE id=$1`, c.Id); value != "true" {
		t.Fatal("narrowing refusal destroyed base consent")
	}
	e.scalar(`UPDATE auth_providers SET narrow_on_refresh=false WHERE id=$1 RETURNING 'ok'`, e.provider.Id)
	if !e.providerReport(e.callTool(key, f3Tool)).Received {
		t.Fatal("explicit admin disabling narrowing did not restore ordinary token delivery")
	}
}
func TestServiceConnectionNarrowingUsesScopeSeparator(t *testing.T) {
	e := newProvEnv(t)
	e.setRequirement(map[string]any{"providerId": e.provider.Id, "scopes": []string{"repo", "read:user"}})
	e.scalar(`UPDATE auth_providers SET narrow_on_refresh=true,scope_separator=',' WHERE id=$1 RETURNING 'ok'`, e.provider.Id)
	e.oauth.Set(func(f *testutil.FakeOAuth) { f.HonourRefreshScope = true })
	sa := createSA(t, e.owner, map[string]any{"name": "comma-ci"})
	key := createSAKey(t, e.owner, sa.Id.String(), map[string]any{"name": "ci"}).Secret
	c := connectShared(t, e.owner, e.provider.Id, "comma")
	code, ec, _ := assignShared(t, e.owner, c, []uuid.UUID{sa.Id}, []apitypes.ServiceConnectionGrant{{ServerId: e.server.Id, Origin: registry.OriginOf(e.server.Url), Scopes: []string{"repo", "read:user"}}})
	mustCode(t, "assign", code, ec, 200, "")
	if !e.providerReport(e.callTool(key, f3Tool)).Received {
		t.Fatal("comma-separated scoped refresh failed")
	}
	scopes := e.oauth.RefreshScopes()
	if len(scopes) != 1 || !strings.Contains(scopes[0], ",") || strings.Contains(scopes[0], " ") {
		t.Fatalf("scoped refresh ignored separator: %v", scopes)
	}
}

// A narrowed refresh answered with an empty access token (issue #107) fails
// closed: the call is refused, the stored full token is never sent, nothing
// is cached (the next call asks again) and the consent is kept.
func TestServiceConnectionEmptyNarrowedTokenNeverFallsBack(t *testing.T) {
	e := newProvEnv(t)
	sa := createSA(t, e.owner, map[string]any{"name": "empty-narrow-ci"})
	key := createSAKey(t, e.owner, sa.Id.String(), map[string]any{"name": "ci"}).Secret
	c := connectShared(t, e.owner, e.provider.Id, "empty-narrow")
	code, ec, _ := assignShared(t, e.owner, c, []uuid.UUID{sa.Id}, []apitypes.ServiceConnectionGrant{{ServerId: e.server.Id, Origin: registry.OriginOf(e.server.Url), Scopes: []string{"repo"}}})
	mustCode(t, "assign", code, ec, 200, "")
	e.scalar(`UPDATE auth_providers SET narrow_on_refresh=true WHERE id=$1 RETURNING 'ok'`, e.provider.Id)
	e.oauth.Set(func(f *testutil.FakeOAuth) { f.FailRefresh = "empty_access" })
	for range 2 {
		before := e.oauth.RefreshRequests()
		serviceDenied(t, e.callTool(key, f3Tool), "provider_unavailable")
		if e.oauth.RefreshRequests() != before+1 {
			t.Fatal("an empty narrowed token was cached")
		}
	}
	if n := e.toolCalls("provider_token"); n != 0 {
		t.Fatal("an empty narrowed token fell back to the stored full token")
	}
	if value := e.scalar(`SELECT (status='active' AND access_token_cipher IS NOT NULL AND refresh_token_cipher IS NOT NULL)::text FROM service_connections WHERE id=$1`, c.Id); value != "true" {
		t.Fatal("an empty narrowed token destroyed base consent")
	}
	e.oauth.Set(func(f *testutil.FakeOAuth) { f.FailRefresh = "" })
	if !e.providerReport(e.callTool(key, f3Tool)).Received {
		t.Fatal("a working provider did not deliver again")
	}
}

// serviceIssueLocked's last step reads the stored token when nothing was
// issued yet. A narrowed call must never get there: a retry after an upstream
// rejected the narrowed token is refused, not answered with the stored token
// holding every granted scope (issue #107).
func TestServiceNarrowedRetryNeverGetsTheStoredToken(t *testing.T) {
	var prov *providers.Service
	e := newProvEnvHook(t, func(d *httpapi.Deps) { prov = d.Providers })
	e.oauth.Set(func(f *testutil.FakeOAuth) { f.HonourRefreshScope = true })
	e.setRequirement(map[string]any{"providerId": e.provider.Id, "scopes": []string{"repo"}})
	sa := createSA(t, e.owner, map[string]any{"name": "narrow-retry-ci"})
	c := connectShared(t, e.owner, e.provider.Id, "narrow-retry")
	code, ec, _ := assignShared(t, e.owner, c, []uuid.UUID{sa.Id}, []apitypes.ServiceConnectionGrant{{ServerId: e.server.Id, Origin: registry.OriginOf(e.server.Url), Scopes: []string{"repo"}}})
	mustCode(t, "assign", code, ec, 200, "")
	e.scalar(`UPDATE auth_providers SET narrow_on_refresh=true WHERE id=$1 RETURNING 'ok'`, e.provider.Id)
	team := uuid.MustParse(e.scalar(`SELECT team_id::text FROM service_accounts WHERE id=$1`, sa.Id))
	cc := providers.ServiceCallContext{Account: dbgen.ServiceAccount{ID: sa.Id, TeamID: team}, Team: dbgen.Team{ID: team},
		ServerID: e.server.Id, ServerOrigin: registry.OriginOf(e.server.Url), ServerURL: e.server.Url}
	req := &registry.Requirement{Provider: registry.Provider{ID: e.provider.Id}, Scopes: []string{"repo"}}
	ctx := context.Background()
	cred, err := prov.ServiceCredential(ctx, req, cc)
	if err != nil || !cred.Narrowed {
		t.Fatalf("narrowed credential = %+v, %v", cred, err)
	}
	stored := e.scalar(`SELECT encode(access_token_cipher, 'hex') FROM service_connections WHERE id=$1`, c.Id)
	next, err := prov.ServiceUpstreamRejected(ctx, req, cc, cred, true)
	var down *providers.UnavailableError
	if !errors.As(err, &down) || next.AccessToken != "" {
		t.Fatalf("retry after a rejected narrowed token: got a token (narrowed %v), err %v; want it refused", next.Narrowed, err)
	}
	if e.scalar(`SELECT encode(access_token_cipher, 'hex') FROM service_connections WHERE id=$1`, c.Id) != stored {
		t.Fatal("the refused retry changed the stored token")
	}
}
