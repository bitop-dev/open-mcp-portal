package store_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/ncecere/open-mcp-portal/internal/store"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
)

// The serving pool is sized by its caller (config.DatabasePoolSize, issue
// #107); 0 keeps the URL's own pool_max_conns, else pgx's default.
func TestOpenWaitPoolSize(t *testing.T) {
	_, dsn := testutil.NewDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(dsn string, maxConns int) int32 {
		t.Helper()
		pool, err := store.OpenWait(ctx, dsn, maxConns, time.Second, testutil.Logger())
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		return pool.Config().MaxConns
	}
	if got := open(dsn, 23); got != 23 {
		t.Fatalf("pool size = %d, want 23", got)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "5")
	u.RawQuery = q.Encode()
	if got := open(u.String(), 0); got != 5 {
		t.Fatalf("pool size = %d, want the URL's pool_max_conns 5", got)
	}
	if got := open(u.String(), 9); got != 9 {
		t.Fatalf("pool size = %d, want 9 over the URL's pool_max_conns", got)
	}
}
