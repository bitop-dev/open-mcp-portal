-- +goose NO TRANSACTION
-- +goose Up
-- Both retention purges (#106) chose their rows with a per-row cutoff computed
-- from two team joins, so the only usable bound was the rollup watermark (about
-- now): a batch that found fewer rows than it asked for read the whole table
-- (0.4 to 1.4 s at 300k rows, every pass; under 1 ms now). Each now also bounds
-- the scan by a floor, the cutoff of the shortest period any row can have: the
-- platform period or a team's override, whichever is smallest across every
-- team. A row's own period is never shorter, and the clamp keeps the order, so
-- the floor holds for every row the per-row cutoff deletes: the same rows go,
-- oldest first (the retention loop stops on a short batch, so only the order
-- within a pass changes). An override that is not a plain integer drops the
-- floor (the previous behaviour, including its error) instead of adding an
-- error. Ordering by occurred_at keeps the occurred_at index under a generic
-- cached plan, which would otherwise walk the primary key.
--
-- The function keeps its signature, owner, ACL, SECURITY DEFINER and settings.
-- CREATE OR REPLACE keeps the owner and ACL but replaces the SET clauses, so
-- they are spelled out again (00033 added TimeZone). Without a transaction
-- here, the restatement and its checks are one statement: a failed check
-- leaves the previous definition in place.
--
-- mcp_call_payloads gets an occurred_at index for the payload purge
-- (internal/store/queries/operations.sql), built concurrently; an interrupted
-- invalid build is removed and rebuilt. Rerunning the migration is safe. The
-- previous release runs unchanged against this schema.
-- +goose StatementBegin
DO $migration$
DECLARE
    fn constant regprocedure := 'public.omp_purge_mcp_calls(timestamptz,integer,integer)'::regprocedure;
    table_owner oid;
    old_owner oid;
    old_acl aclitem[];
BEGIN
    SELECT relowner INTO STRICT table_owner FROM pg_catalog.pg_class WHERE oid = 'public.mcp_calls'::regclass;
    SELECT proowner, proacl INTO STRICT old_owner, old_acl FROM pg_catalog.pg_proc WHERE oid = fn;
    IF old_owner <> table_owner THEN
        RAISE EXCEPTION 'omp_purge_mcp_calls must be owned by the owner of mcp_calls';
    END IF;
    EXECUTE $create$
CREATE OR REPLACE FUNCTION public.omp_purge_mcp_calls(watermark timestamptz, platform_days integer, batch_size integer) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $fn$
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
    WITH floor AS (
        -- The cutoff of the shortest possible period, read in the same snapshot
        -- as the rows. A value the per-row cast could reject leaves the CASE
        -- NULL, so least() falls back to the watermark.
        SELECT least(safe_watermark, CASE WHEN bool_and(o IS NULL OR o ~ '^-?[0-9]{1,9}$') IS NOT FALSE THEN
            now() - make_interval(days => greatest(1, least(3650, least(platform_days,
                min(CASE WHEN o ~ '^-?[0-9]{1,9}$' THEN o::integer END)))))
        END) AS cutoff
        FROM (SELECT t.retention_overrides ->> 'call_metadata' AS o FROM public.teams t) overrides
    ), doomed AS (
        SELECT c.id FROM public.mcp_calls c
        LEFT JOIN public.teams gt ON gt.id = c.gateway_team_id
        LEFT JOIN public.teams st ON st.id = c.server_team_id
        WHERE c.occurred_at < (SELECT cutoff FROM floor)
          AND c.occurred_at < safe_watermark
          AND c.occurred_at < now() - make_interval(days => greatest(1, least(3650, coalesce(least(
              CASE WHEN gt.id IS NOT NULL THEN coalesce((gt.retention_overrides ->> 'call_metadata')::integer, platform_days) END,
              CASE WHEN st.id IS NOT NULL THEN coalesce((st.retention_overrides ->> 'call_metadata')::integer, platform_days) END
          ), platform_days))))
        ORDER BY c.occurred_at, c.id LIMIT batch_size FOR UPDATE OF c SKIP LOCKED
    ), removed AS (
        DELETE FROM public.mcp_calls WHERE id IN (SELECT id FROM doomed) RETURNING id
    ) SELECT count(*) INTO deleted FROM removed;
    PERFORM set_config('omp.calls_purge', coalesce(previous_flag, ''), true);
    RETURN deleted;
EXCEPTION WHEN OTHERS THEN
    PERFORM set_config('omp.calls_purge', coalesce(previous_flag, ''), true);
    RAISE;
END
$fn$
$create$;
    IF NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_proc p
        WHERE p.oid = fn AND p.prosecdef AND p.prokind = 'f' AND p.prorettype = 'pg_catalog.int8'::regtype
          AND p.proconfig = ARRAY['search_path=pg_catalog, pg_temp', 'TimeZone=UTC']
          AND p.proowner = old_owner AND p.proacl IS NOT DISTINCT FROM old_acl
          AND NOT EXISTS (SELECT 1 FROM pg_catalog.aclexplode(coalesce(p.proacl, pg_catalog.acldefault('f', p.proowner))) a
                          WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')) THEN
        RAISE EXCEPTION 'omp_purge_mcp_calls lost its definer settings, owner or ACL';
    END IF;
END
$migration$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $$
BEGIN
 IF EXISTS (SELECT 1 FROM pg_catalog.pg_index WHERE indexrelid = to_regclass('public.mcp_call_payloads_occurred_idx') AND NOT indisvalid) THEN
  DROP INDEX public.mcp_call_payloads_occurred_idx;
 END IF;
END $$;
-- +goose StatementEnd
CREATE INDEX CONCURRENTLY IF NOT EXISTS mcp_call_payloads_occurred_idx ON public.mcp_call_payloads (occurred_at);
-- +goose Down
SELECT 1;
