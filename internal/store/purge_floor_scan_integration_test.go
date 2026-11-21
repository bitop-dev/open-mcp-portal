package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ncecere/open-mcp-portal/internal/store"
	"github.com/ncecere/open-mcp-portal/internal/store/dbgen"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
	"github.com/ncecere/open-mcp-portal/migrations"
)

// A batch that finds nothing to delete read the whole call log and payload
// table (#106). With the floor it reads only the rows older than the shortest
// possible period, under a custom and a generic cached plan alike.
func TestPurgeFloorBoundsThePartialBatchScan(t *testing.T) {
	pool, _ := testutil.NewDB(t)
	ctx := context.Background()
	roleExec(t, pool, `INSERT INTO teams(slug,name,retention_overrides) VALUES ('scan','Scan','{"call_metadata":7,"captured_payloads":7}')`)
	roleExec(t, pool, `INSERT INTO mcp_calls(occurred_at,method,outcome,gateway_id,gateway_slug,gateway_name,gateway_team_id,principal_kind,service_account_id)
 SELECT now()-random()*interval '6 days','tools/call','ok',gen_random_uuid(),'scan','Scan',(SELECT id FROM teams WHERE slug='scan'),'service_account',gen_random_uuid()
 FROM generate_series(1,20000)`)
	roleExec(t, pool, `INSERT INTO mcp_call_payloads(call_id,occurred_at,gateway_team_id,by_gateway,by_server,arguments_cipher,arguments_bytes)
 SELECT id,occurred_at,gateway_team_id,true,false,'\x00',1 FROM mcp_calls`)
	roleExec(t, pool, `UPDATE usage_rollup_state SET rolled_up_to=now()`)
	roleExec(t, testutil.OwnerPool(t, pool), `ANALYZE teams; ANALYZE mcp_calls; ANALYZE mcp_call_payloads`)
	for _, mode := range []string{"auto", "force_generic_plan"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		readTuples(t, tx, mode)
		_ = tx.Rollback(ctx)
	}
}

func readTuples(t *testing.T, tx pgx.Tx, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `SELECT set_config('plan_cache_mode',$1,true)`, mode); err != nil {
		t.Fatal(err)
	}
	// The counters include this backend's not yet flushed earlier work (the
	// fixture's inserts), and are not flushed inside a transaction: compare.
	before := tableReads(t, tx)
	q := dbgen.New(tx)
	// More than five runs, so the plan cache may switch to a generic plan.
	for range 8 {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT public.omp_purge_mcp_calls(now(),90,1000)`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("calls purge: %d %v", n, err)
		}
		if n, err := q.PurgeMCPCallPayloads(ctx, dbgen.PurgeMCPCallPayloadsParams{PlatformDays: 30, BatchSize: 1000}); err != nil || n != 0 {
			t.Fatalf("payload purge: %d %v", n, err)
		}
	}
	for name, read := range tableReads(t, tx) {
		if read -= before[name]; read > 100 {
			t.Errorf("%s: %d tuples read by 8 empty purge batches (plan_cache_mode=%s)", name, read, mode)
		}
	}
}

// tableReads returns the heap tuples read by sequential and index scans.
func tableReads(t *testing.T, tx pgx.Tx) map[string]int64 {
	t.Helper()
	rows, err := tx.Query(context.Background(), `SELECT relname, coalesce(seq_tup_read,0)+coalesce(idx_tup_fetch,0)
 FROM pg_stat_xact_user_tables WHERE schemaname='public' AND relname IN ('mcp_calls','mcp_call_payloads')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var read int64
		if err := rows.Scan(&name, &read); err != nil {
			t.Fatal(err)
		}
		out[name] = read
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("read counters for %v", out)
	}
	return out
}

const purgeCallsDefiner = `SELECT format('%s|%s|%s|%s|%s', pg_get_userbyid(p.proowner), p.prosecdef, p.proconfig, p.proacl,
 pg_get_function_result(p.oid)), p.prosrc LIKE '%WITH floor AS%'
 FROM pg_proc p WHERE p.oid='public.omp_purge_mcp_calls(timestamptz,integer,integer)'::regprocedure`

type definerState struct {
	catalog string
	floor   bool
}

// 00066 restates omp_purge_mcp_calls: its owner, ACL (including the runtime
// role's EXECUTE from the grants reconciliation), SECURITY DEFINER, settings
// and result type survive, and the payload index is valid.
func TestPurgeFloorMigrationKeepsDefinerBoundary(t *testing.T) {
	var before, after definerState
	var runtimeRole string
	pool, _ := testutil.NewDBWithMigration(t, func(ctx context.Context, owner *pgxpool.Pool, role string) error {
		runtimeRole = role
		db := stdlib.OpenDBFromPool(owner)
		defer db.Close()
		provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
		if err != nil {
			return err
		}
		read := func(s *definerState) error { return owner.QueryRow(ctx, purgeCallsDefiner).Scan(&s.catalog, &s.floor) }
		if _, err = provider.UpTo(ctx, 65); err == nil {
			err = reconcileFixtureGrants(ctx, owner, role)
		}
		if err == nil {
			err = read(&before)
		}
		if err == nil {
			_, err = provider.UpTo(ctx, 66)
		}
		if err == nil {
			err = read(&after)
		}
		if err == nil {
			_, err = provider.Up(ctx)
		}
		if err != nil {
			return err
		}
		return reconcileFixtureGrants(ctx, owner, role)
	})
	if before.floor || !after.floor {
		t.Fatalf("restatement did not apply: before %v after %v", before.floor, after.floor)
	}
	if before.catalog != after.catalog {
		t.Fatalf("definer boundary changed:\nbefore %s\nafter  %s", before.catalog, after.catalog)
	}
	for _, want := range []string{"|t|", `{"search_path=pg_catalog, pg_temp",TimeZone=UTC}`, "|bigint"} {
		if !strings.Contains(after.catalog, want) {
			t.Fatalf("%s lacks %s", after.catalog, want)
		}
	}
	owner := testutil.OwnerPool(t, pool)
	if runtimeRole != "" {
		if n := roleCount(t, owner, `SELECT has_function_privilege($1,'public.omp_purge_mcp_calls(timestamptz,integer,integer)','EXECUTE')::int`, runtimeRole); n != 1 {
			t.Fatal("runtime role lost EXECUTE")
		}
	}
	if n := roleCount(t, owner, `SELECT count(*) FROM pg_index WHERE indexrelid='public.mcp_call_payloads_occurred_idx'::regclass AND indisvalid`); n != 1 {
		t.Fatal("payload occurred_at index missing or invalid")
	}
}

func reconcileFixtureGrants(ctx context.Context, owner *pgxpool.Pool, role string) error {
	if role == "" {
		return nil
	}
	conn, err := owner.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return store.ReconcileRuntimeGrants(ctx, conn.Conn(), role)
}
