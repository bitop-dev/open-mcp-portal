package providers

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ncecere/open-mcp-portal/internal/registry"
	"github.com/ncecere/open-mcp-portal/internal/store/dbgen"
)

// A narrowed refresh answered with an empty access token fails closed
// (issue #107): no credential, nothing cached, and the rotated refresh token
// is still stored. Before, the empty token went to r.out and the cache, and
// serviceIssueLocked then sent the stored, un-narrowed token in its place.
// (parseToken refuses such an answer first; this pins the layer below it.)
func TestServiceNarrowedEmptyAccessTokenFailsClosed(t *testing.T) {
	s := testService(t, nil)
	q := dbgen.New(rowsDB{rowsKey("RotateServiceConnectionRefresh", "exec"): true})
	p := dbgen.AuthProvider{ID: uuid.New(), Slug: "github", Kind: "oauth2", Enabled: true, NarrowOnRefresh: true}
	c := dbgen.ServiceConnection{ID: uuid.New(), RefreshTokenCipher: []byte("sealed")}
	r := &serviceIssue{req: &registry.Requirement{Provider: registry.ProviderOf(p), Scopes: []string{"repo"}}}
	err := s.serviceKeepNarrowed(context.Background(), q, p, c, tokenSet{Access: "", Refresh: "rotated", ExpiresIn: 3600}, []byte("sealed-rotated"), r)
	if err != nil {
		t.Fatal(err)
	}
	var down *UnavailableError
	if !errors.As(r.outcome, &down) || down.Code != "invalid_response" {
		t.Fatalf("outcome = %v, want an UnavailableError (invalid_response)", r.outcome)
	}
	if r.pending != nil || r.out.AccessToken != "" || r.out.Narrowed {
		t.Fatalf("an empty narrowed token was kept: pending %+v, out %+v", r.pending, r.out)
	}
}
