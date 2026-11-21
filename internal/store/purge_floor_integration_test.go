package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ncecere/open-mcp-portal/internal/store/dbgen"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
)

// Migration 00032's omp_purge_mcp_calls body with 00033's TimeZone, kept to
// prove that 00066 deletes the same rows. The test creates it inside a
// transaction it rolls back, as the owner (no SECURITY DEFINER needed).
const oldPurgeCallsFunc = `CREATE FUNCTION public.omp_test_old_purge_mcp_calls(watermark timestamptz, platform_days integer, batch_size integer) RETURNS bigint
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $$
DECLARE
    previous_flag text := current_setting('omp.calls_purge', true);
    safe_watermark timestamptz;
    deleted bigint;
BEGIN
    IF batch_size IS NULL OR batch_size < 1 OR batch_size > 100000 OR platform_days IS NULL THEN
        RAISE EXCEPTION 'invalid purge arguments' USING ERRCODE = '22023';
    END IF;
    SELECT date_trunc('hour', rolled_up_to, 'UTC') - interval '1 hour'
      INTO safe_watermark FROM public.usage_rollup_state WHERE id = 1;
    IF watermark IS NULL OR safe_watermark IS NULL THEN
        RETURN 0;
    END IF;
    safe_watermark := least(watermark, safe_watermark);
    PERFORM set_config('omp.calls_purge', 'on', true);
    WITH doomed AS (
        SELECT c.id FROM public.mcp_calls c
        LEFT JOIN public.teams gt ON gt.id = c.gateway_team_id
        LEFT JOIN public.teams st ON st.id = c.server_team_id
        WHERE c.occurred_at < safe_watermark
          AND c.occurred_at < now() - make_interval(days => greatest(1, least(3650, coalesce(least(
              CASE WHEN gt.id IS NOT NULL THEN coalesce((gt.retention_overrides ->> 'call_metadata')::integer, platform_days) END,
              CASE WHEN st.id IS NOT NULL THEN coalesce((st.retention_overrides ->> 'call_metadata')::integer, platform_days) END
          ), platform_days))))
        ORDER BY c.id LIMIT batch_size FOR UPDATE OF c SKIP LOCKED
    ), removed AS (
        DELETE FROM public.mcp_calls WHERE id IN (SELECT id FROM doomed) RETURNING id
    ) SELECT count(*) INTO deleted FROM removed;
    PERFORM set_config('omp.calls_purge', coalesce(previous_flag, ''), true);
    RETURN deleted;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('omp.calls_purge', coalesce(previous_flag, ''), true);
    RAISE;
END
$$`

// PurgeMCPCallPayloads as it was before 00066.
const oldPurgePayloads = `WITH doomed AS (
    SELECT p.call_id
    FROM mcp_call_payloads p
    LEFT JOIN teams gt ON gt.id = p.gateway_team_id
    LEFT JOIN teams st ON st.id = p.server_team_id
    WHERE p.occurred_at < now() - make_interval(days => COALESCE(LEAST(
        CASE WHEN gt.id IS NOT NULL THEN COALESCE((gt.retention_overrides ->> 'captured_payloads')::int, $1::int) END,
        CASE WHEN st.id IS NOT NULL THEN COALESCE((st.retention_overrides ->> 'captured_payloads')::int, $1::int) END),
        $1::int))
    ORDER BY p.call_id
    LIMIT $2
    FOR UPDATE OF p SKIP LOCKED
)
DELETE FROM mcp_call_payloads WHERE call_id IN (SELECT call_id FROM doomed)`

type floorTeam struct {
	overrides string
	unused    bool // no call references the team
}

type floorCase struct {
	name                  string
	teams                 []floorTeam
	callDays, payloadDays int32
	noRows                bool
	callErr, payloadErr   string // SQLSTATE both versions must raise
}

// Half a day either side of every cutoff the cases use, the rollup watermark
// (3 days) and the 1- and 3650-day clamps.
var floorAges = []float64{0.5, 1.5, 2.5, 3.5, 4.5, 5.5, 7.5, 8.5, 9.5, 10.5, 11.5, 12.5, 19.5, 20.5, 29.5, 30.5,
	44.5, 45.5, 59.5, 60.5, 3649.5, 3650.5, 3999.5, 4000.5, 4999.5, 5000.5}

var mixedFloorTeams = []floorTeam{
	{overrides: `{"call_metadata":10,"captured_payloads":5}`},
	{overrides: `{"call_metadata":60,"captured_payloads":45}`},
	{overrides: `{}`},
	{overrides: `{"call_metadata":0,"captured_payloads":-3}`},      // below the 1-day clamp
	{overrides: `{"call_metadata":5000,"captured_payloads":5000}`}, // above the 3650-day clamp
	{overrides: `{"call_metadata":null,"captured_payloads":null}`},
	{overrides: `{"call_metadata":"12","captured_payloads":"8"}`},
}

var unusableFloorTeams = []floorTeam{
	{overrides: `{"call_metadata":"abc","captured_payloads":1.5}`},
	{overrides: `{"call_metadata":2147483648,"captured_payloads":5000000}`},
	{overrides: `{"call_metadata":" 4","captured_payloads":"+2"}`},
}

func unused(teams []floorTeam) []floorTeam {
	out := slices.Clone(teams)
	for i := range out {
		out[i].unused = true
	}
	return out
}

func floorCases() []floorCase {
	return []floorCase{
		{name: "mixed periods", teams: mixedFloorTeams, callDays: 30, payloadDays: 20},
		{name: "no overrides", teams: []floorTeam{{overrides: `{}`}, {overrides: `{"limits":1}`}}, callDays: 30, payloadDays: 20},
		{name: "no teams", callDays: 30, payloadDays: 20},
		{name: "only below the clamp", teams: []floorTeam{{overrides: `{"call_metadata":-5,"captured_payloads":-5}`}}, callDays: 30, payloadDays: 20},
		{name: "above the clamp", teams: []floorTeam{{overrides: `{"call_metadata":4000,"captured_payloads":4000}`}}, callDays: 5000, payloadDays: 5000},
		{name: "unusable overrides unused", teams: append(slices.Clone(mixedFloorTeams), unused(unusableFloorTeams)...), callDays: 30, payloadDays: 20},
		// The old cast accepts these; the floor does not read them and falls back.
		{name: "castable but not plain overrides", teams: append(slices.Clone(mixedFloorTeams), unusableFloorTeams[2]), callDays: 30, payloadDays: 20},
		{name: "unusable call override", teams: []floorTeam{{overrides: `{"call_metadata":"abc"}`}, mixedFloorTeams[0]},
			callDays: 30, payloadDays: 20, callErr: "22P02"},
		{name: "fractional payload override", teams: []floorTeam{{overrides: `{"captured_payloads":1.5}`}}, callDays: 30, payloadDays: 20, payloadErr: "22P02"},
		{name: "overflowing overrides", teams: []floorTeam{unusableFloorTeams[1]}, callDays: 30, payloadDays: 20, callErr: "22003", payloadErr: "22008"},
		{name: "empty tables", teams: append(slices.Clone(mixedFloorTeams), unusableFloorTeams...), callDays: 30, payloadDays: 20, noRows: true},
	}
}

// The floor (00066) must delete exactly the rows the per-row cutoff alone
// deleted, in batches until a short batch as the retention loop does, and
// raise the same errors on overrides the per-row cast rejects.
func TestPurgeFloorDeletesTheSameRows(t *testing.T) {
	for _, tc := range floorCases() {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := testutil.NewDB(t)
			seedFloorCase(t, pool, tc)
			// Payloads first: deleting calls cascades to them.
			for _, kind := range []string{"payloads", "calls"} {
				want, wantLeft, wantErr := runOldPurge(t, testutil.OwnerPool(t, pool), kind, tc)
				got, gotLeft, gotErr := runNewPurge(t, pool, kind, tc)
				compareFloorRun(t, kind, tc, [2]int64{want, got}, [2][]int64{wantLeft, gotLeft}, [2]string{wantErr, gotErr})
			}
		})
	}
}

func compareFloorRun(t *testing.T, kind string, tc floorCase, total [2]int64, left [2][]int64, code [2]string) {
	t.Helper()
	wantErr := tc.callErr
	if kind == "payloads" {
		wantErr = tc.payloadErr
	}
	if code[0] != wantErr || code[1] != wantErr {
		t.Fatalf("%s: old error %q, new error %q, want %q", kind, code[0], code[1], wantErr)
	}
	if wantErr != "" {
		return
	}
	if total[0] != total[1] || !slices.Equal(left[0], left[1]) {
		t.Fatalf("%s: old deleted %d leaving %v; new deleted %d leaving %v", kind, total[0], left[0], total[1], left[1])
	}
	if !tc.noRows && (total[0] == 0 || len(left[0]) == 0) {
		t.Fatalf("%s: fixture must delete some rows and keep some (deleted %d, kept %d)", kind, total[0], len(left[0]))
	}
}

const floorBatch = 25

func seedFloorCase(t *testing.T, pool *pgxpool.Pool, tc floorCase) {
	t.Helper()
	used := []string{}
	for i, team := range tc.teams {
		var id string
		if err := pool.QueryRow(context.Background(), `INSERT INTO teams(slug,name,retention_overrides) VALUES ($1,$1,$2::jsonb) RETURNING id::text`,
			fmt.Sprintf("floor-%d", i), team.overrides).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if !team.unused {
			used = append(used, id)
		}
	}
	// A missing team id stands for a deleted team.
	roleExec(t, pool, `UPDATE usage_rollup_state SET rolled_up_to=now()-interval '3 days'`)
	if tc.noRows {
		return
	}
	roleExec(t, pool, `WITH team AS (SELECT unnest($2::text[]::uuid[] || ARRAY[NULL, '00000000-0000-4000-8000-000000000066']::uuid[]) AS id)
 INSERT INTO mcp_calls(occurred_at,method,outcome,gateway_id,gateway_slug,gateway_name,gateway_team_id,server_team_id,principal_kind,service_account_id)
 SELECT now()-a*interval '1 day','tools/list','ok',gen_random_uuid(),'floor','Floor',g.id,s.id,'service_account',gen_random_uuid()
 FROM unnest($1::float8[]) a, team g, team s`, floorAges, used)
	roleExec(t, pool, `INSERT INTO mcp_call_payloads(call_id,occurred_at,gateway_team_id,server_team_id,by_gateway,by_server,arguments_cipher,arguments_bytes)
 SELECT id,occurred_at,gateway_team_id,server_team_id,true,false,'\x00',1 FROM mcp_calls`)
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return err.Error()
}

// drain calls batch until it returns a short batch, as internal/retention does.
func drain(t *testing.T, batch func() (int64, error)) (int64, string) {
	t.Helper()
	total := int64(0)
	for i := 0; i < 10000; i++ {
		n, err := batch()
		if err != nil {
			return total, sqlState(err)
		}
		total += n
		if n < floorBatch {
			return total, ""
		}
	}
	t.Fatal("purge did not finish")
	return 0, ""
}

func leftIDs(t *testing.T, scan func(string) ([]int64, error), kind string) []int64 {
	t.Helper()
	sql := `SELECT coalesce(array_agg(id ORDER BY id),'{}') FROM mcp_calls`
	if kind == "payloads" {
		sql = `SELECT coalesce(array_agg(call_id ORDER BY call_id),'{}') FROM mcp_call_payloads`
	}
	ids, err := scan(sql)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// runOldPurge runs the pre-00066 logic in a transaction it rolls back.
func runOldPurge(t *testing.T, owner *pgxpool.Pool, kind string, tc floorCase) (int64, []int64, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, oldPurgeCallsFunc); err != nil {
		t.Fatal(err)
	}
	total, code := drain(t, func() (int64, error) {
		if kind == "payloads" {
			tag, err := tx.Exec(ctx, oldPurgePayloads, tc.payloadDays, floorBatch)
			return tag.RowsAffected(), err
		}
		var n int64
		err := tx.QueryRow(ctx, `SELECT public.omp_test_old_purge_mcp_calls(now(),$1,$2)`, tc.callDays, floorBatch).Scan(&n)
		return n, err
	})
	if code != "" {
		return total, nil, code
	}
	return total, leftIDs(t, func(sql string) (ids []int64, err error) {
		return ids, tx.QueryRow(ctx, sql).Scan(&ids)
	}, kind), ""
}

// runNewPurge runs the current function and query, one transaction per batch.
func runNewPurge(t *testing.T, pool *pgxpool.Pool, kind string, tc floorCase) (int64, []int64, string) {
	t.Helper()
	ctx := context.Background()
	q := dbgen.New(pool)
	total, code := drain(t, func() (int64, error) {
		if kind == "payloads" {
			return q.PurgeMCPCallPayloads(ctx, dbgen.PurgeMCPCallPayloadsParams{PlatformDays: tc.payloadDays, BatchSize: floorBatch})
		}
		var n int64
		err := pool.QueryRow(ctx, `SELECT public.omp_purge_mcp_calls(now(),$1,$2)`, tc.callDays, floorBatch).Scan(&n)
		return n, err
	})
	if code != "" {
		return total, nil, code
	}
	return total, leftIDs(t, func(sql string) (ids []int64, err error) {
		return ids, pool.QueryRow(ctx, sql).Scan(&ids)
	}, kind), ""
}
