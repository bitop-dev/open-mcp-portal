-- +goose Up
-- #93: a durable "held envelope delivered" marker for a required full-profile
-- sink held at an oversized row. Additive: a v0.15.2 sender never reads it and
-- keeps redelivering the held envelope on every pass, as before; its
-- finish_attempt calls (same signature) maintain the marker for newer senders.
--
-- One row per sink. The marker is current only while held_seq=cursor_seq+1 and
-- sink_revision=sinks.revision: an acknowledgement moves the cursor and every
-- settings edit (enable/disable, destination, secret, profile, required) bumps
-- the revision, so a stale row is never honoured even before it is cleared.
-- Only finish_attempt writes it, under the same floor -> sink locks and the
-- same consumed attempt token that fence the cursor; the runtime role reads it.
CREATE TABLE omp_audit_ext.sink_holds (
 sink_id uuid PRIMARY KEY REFERENCES omp_audit_ext.sinks(id) ON DELETE CASCADE,
 held_seq bigint NOT NULL CHECK(held_seq>0),
 sink_revision bigint NOT NULL CHECK(sink_revision>0),
 delivered_at timestamptz,
 failed_at timestamptz,
 failure_class text CHECK(failure_class IN ('dns','connect','tls','timeout','http_3xx','http_4xx','http_5xx','too_large','policy_denied','integrity')),
 CHECK((delivered_at IS NULL)<>(failed_at IS NULL)),
 CHECK((failed_at IS NULL)=(failure_class IS NULL))
);
-- Restated from 00064 with the marker bookkeeping added: 'held' records the
-- delivery, a failed single-row attempt of a holdable oversized row records the
-- failure (the latest result wins), and 'ok' clears it. Everything else is
-- unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION omp_audit_ext.finish_attempt(p_id uuid,p_token uuid,p_outcome text,p_error_class text,p_notify_intent boolean) RETURNS omp_audit_ext.sinks
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $$
DECLARE f omp_audit.retention_floor%ROWTYPE; s omp_audit_ext.sinks%ROWTYPE; hold boolean:=false;
BEGIN
 SELECT * INTO STRICT f FROM omp_audit.retention_floor WHERE singleton FOR UPDATE;
 SELECT * INTO STRICT s FROM omp_audit_ext.sinks WHERE id=p_id FOR UPDATE;
 IF NOT (SELECT enabled FROM omp_audit_ext.sink_settings WHERE singleton) THEN RAISE EXCEPTION 'sinks disabled'; END IF;
 IF p_token IS NULL OR s.inflight_token IS DISTINCT FROM p_token OR s.inflight_revision IS DISTINCT FROM s.revision THEN RAISE EXCEPTION 'stale sink token' USING ERRCODE='40001'; END IF;
 IF p_outcome IS NULL OR p_outcome NOT IN ('ok','held','failed') OR p_notify_intent IS NULL THEN RAISE EXCEPTION 'invalid outcome'; END IF;
 IF (s.inflight_kind='data' AND (s.inflight_first<>s.cursor_seq+1 OR s.gap_first IS NOT NULL))
 OR (s.inflight_kind='gap' AND (s.inflight_first IS DISTINCT FROM s.gap_first OR s.inflight_last IS DISTINCT FROM s.gap_last)) THEN RAISE EXCEPTION 'attempt endpoints changed'; END IF;
 IF s.inflight_kind='data' AND s.inflight_first=s.inflight_last AND s.required AND s.profile='full' THEN
  hold:=EXISTS(SELECT 1 FROM omp_audit.chain WHERE seq=s.inflight_first AND kind='entry' AND flags&2<>0);
 END IF;
 IF p_outcome='held' AND NOT hold THEN RAISE EXCEPTION 'invalid oversized hold'; END IF;
 IF p_outcome='ok' AND s.inflight_kind='data' AND s.required AND s.profile='full' AND EXISTS(SELECT 1 FROM omp_audit.chain WHERE seq BETWEEN s.inflight_first AND s.inflight_last AND flags&2<>0) THEN RAISE EXCEPTION 'oversized acknowledgement required'; END IF;
 IF p_outcome='failed' THEN
  IF p_error_class IS NULL OR p_error_class NOT IN ('dns','connect','tls','timeout','http_3xx','http_4xx','http_5xx','too_large','policy_denied','integrity') THEN RAISE EXCEPTION 'invalid error class'; END IF;
  IF p_notify_intent IS DISTINCT FROM (s.consecutive_failures>=2 AND NOT s.streak_notified) THEN RAISE EXCEPTION 'failure notification intent mismatch'; END IF;
  IF hold THEN
   INSERT INTO omp_audit_ext.sink_holds(sink_id,held_seq,sink_revision,failed_at,failure_class) VALUES(p_id,s.inflight_first,s.revision,clock_timestamp(),p_error_class)
   ON CONFLICT (sink_id) DO UPDATE SET held_seq=EXCLUDED.held_seq,sink_revision=EXCLUDED.sink_revision,delivered_at=NULL,failed_at=EXCLUDED.failed_at,failure_class=EXCLUDED.failure_class;
  END IF;
  UPDATE omp_audit_ext.sinks SET consecutive_failures=consecutive_failures+1,last_error_class=p_error_class,
   streak_id=coalesce(streak_id,gen_random_uuid()),streak_notified=streak_notified OR p_notify_intent,
   next_attempt_at=clock_timestamp()+make_interval(secs=>least(300,power(2,least(s.consecutive_failures,9)))::double precision),inflight_token=NULL WHERE id=p_id RETURNING * INTO s;
 ELSE
  IF p_error_class IS NOT NULL OR p_notify_intent IS DISTINCT FROM s.streak_notified THEN RAISE EXCEPTION 'recovery notification intent mismatch'; END IF;
  IF p_outcome='held' THEN
   INSERT INTO omp_audit_ext.sink_holds(sink_id,held_seq,sink_revision,delivered_at) VALUES(p_id,s.inflight_first,s.revision,clock_timestamp())
   ON CONFLICT (sink_id) DO UPDATE SET held_seq=EXCLUDED.held_seq,sink_revision=EXCLUDED.sink_revision,delivered_at=EXCLUDED.delivered_at,failed_at=NULL,failure_class=NULL;
  ELSE
   DELETE FROM omp_audit_ext.sink_holds WHERE sink_id=p_id;
  END IF;
  UPDATE omp_audit_ext.sinks SET cursor_seq=CASE WHEN p_outcome='held' THEN s.inflight_first-1 ELSE s.inflight_last END,
   gap_first=NULL,gap_last=NULL,consecutive_failures=0,last_error_class=NULL,last_ok_at=clock_timestamp(),
   streak_id=NULL,streak_notified=false,next_attempt_at=clock_timestamp(),inflight_kind=NULL,inflight_first=NULL,inflight_last=NULL,inflight_revision=NULL,inflight_token=NULL WHERE id=p_id RETURNING * INTO s;
 END IF;
 UPDATE omp_audit.retention_floor SET consumer_revision=consumer_revision+1 WHERE singleton;
 RETURN s;
END $$;
-- +goose StatementEnd
-- Restated from 00064: the hold ends here, so its marker is cleared with it.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION omp_audit_ext.acknowledge_oversized(p_id uuid,p_revision bigint,p_seq bigint) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $$
DECLARE f omp_audit.retention_floor%ROWTYPE; s omp_audit_ext.sinks%ROWTYPE;
BEGIN
 SELECT * INTO STRICT f FROM omp_audit.retention_floor WHERE singleton FOR UPDATE;
 SELECT * INTO STRICT s FROM omp_audit_ext.sinks WHERE id=p_id FOR UPDATE;
 IF p_revision IS NULL OR s.revision<>p_revision THEN RAISE EXCEPTION 'stale sink revision' USING ERRCODE='40001'; END IF;
 IF p_seq IS NULL OR p_seq<>s.cursor_seq+1 OR NOT s.required OR s.profile<>'full' OR s.gap_first IS NOT NULL
 OR NOT EXISTS(SELECT 1 FROM omp_audit.chain c JOIN public.audit_log a ON a.id=c.audit_id AND (a.row_uid=c.row_uid OR (a.txid IS NULL AND a.id=c.legacy_id)) WHERE c.seq=p_seq AND c.kind='entry' AND c.flags&2<>0) THEN RAISE EXCEPTION 'oversized acknowledgement refused'; END IF;
 UPDATE omp_audit_ext.sinks SET cursor_seq=p_seq,inflight_kind=NULL,inflight_first=NULL,inflight_last=NULL,inflight_revision=NULL,inflight_token=NULL WHERE id=p_id;
 DELETE FROM omp_audit_ext.sink_holds WHERE sink_id=p_id;
 INSERT INTO public.audit_log(actor_kind,action,target_type,target_id) VALUES('system','audit.sink_oversized_acknowledged','audit_sink',p_id::text);
 UPDATE omp_audit.retention_floor SET consumer_revision=consumer_revision+1 WHERE singleton;

END $$;
-- +goose StatementEnd
-- Restated from 00064: a settings edit bumps the revision, which already
-- changes the delivery identity and discards the in-flight attempt; it now
-- clears the marker too, so the new configuration is sent the held envelope.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION omp_audit_ext.update_sink(p_id uuid,p_revision bigint,p_name text,p_profile text,p_required boolean,p_enabled boolean,p_batch integer,p_url bytea,p_hint text,p_secret bytea,p_previous bytea,p_until timestamptz,p_secret_hint text) RETURNS omp_audit_ext.sinks
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET TimeZone = 'UTC' AS $$
DECLARE f omp_audit.retention_floor%ROWTYPE; s omp_audit_ext.sinks%ROWTYPE;
BEGIN
 PERFORM omp_audit_ext.validate_sink(p_name,p_profile,p_required,p_enabled,p_batch,p_url,p_hint,p_secret,p_previous,p_until,p_secret_hint);
 SELECT * INTO STRICT f FROM omp_audit.retention_floor WHERE singleton FOR UPDATE;
 SELECT * INTO STRICT s FROM omp_audit_ext.sinks WHERE id=p_id FOR UPDATE;
 IF p_revision IS NULL OR s.revision<>p_revision THEN RAISE EXCEPTION 'stale sink revision' USING ERRCODE='40001'; END IF;
 IF p_required AND (s.cursor_seq<f.purged_seq OR s.gap_first IS NOT NULL OR s.state='range_expired') THEN RAISE EXCEPTION 'sink recovery required'; END IF;
 UPDATE omp_audit_ext.sinks SET name=p_name,profile=p_profile,required=p_required,enabled=p_enabled,batch_max_records=p_batch,
 url_cipher=p_url,url_hint=p_hint,secret_cipher=p_secret,secret_previous_cipher=p_previous,secret_previous_until=p_until,secret_hint=p_secret_hint,
 revision=revision+1,inflight_kind=NULL,inflight_first=NULL,inflight_last=NULL,inflight_revision=NULL,inflight_token=NULL WHERE id=p_id RETURNING * INTO s;
 DELETE FROM omp_audit_ext.sink_holds WHERE sink_id=p_id;
 UPDATE omp_audit.retention_floor SET consumer_revision=consumer_revision+1 WHERE singleton;
 RETURN s;
END $$;
-- +goose StatementEnd
INSERT INTO omp_audit_ext.manifest(kind,identity,privileges) VALUES
 ('table','omp_audit_ext.sink_holds',ARRAY['select']);
REVOKE ALL ON ALL TABLES IN SCHEMA omp_audit_ext FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA omp_audit_ext FROM PUBLIC;
-- +goose StatementBegin
DO $$ DECLARE owner_name text; obj record; p record; BEGIN
 SELECT pg_get_userbyid(relowner) INTO STRICT owner_name FROM pg_class WHERE oid='public.audit_log'::regclass;
 FOR obj IN SELECT * FROM omp_audit_ext.manifest WHERE identity<>'omp_audit_ext.manifest' LOOP
  EXECUTE format('ALTER %s %s OWNER TO %I',obj.kind,obj.identity,owner_name);
 END LOOP;
 -- CREATE OR REPLACE keeps owner and ACL; assert the definer settings survived.
 FOR p IN SELECT * FROM pg_proc WHERE oid IN ('omp_audit_ext.finish_attempt(uuid,uuid,text,text,boolean)'::regprocedure,
  'omp_audit_ext.acknowledge_oversized(uuid,bigint,bigint)'::regprocedure,
  'omp_audit_ext.update_sink(uuid,bigint,text,text,boolean,boolean,integer,bytea,text,bytea,bytea,timestamp with time zone,text)'::regprocedure) LOOP
  IF NOT p.prosecdef OR pg_get_userbyid(p.proowner)<>owner_name
  OR NOT p.proconfig @> ARRAY['search_path=pg_catalog, pg_temp','TimeZone=UTC']
  OR position('omp_audit_ext.sink_holds' IN p.prosrc)=0
  OR EXISTS(SELECT 1 FROM aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) WHERE grantee=0) THEN RAISE EXCEPTION 'sink hold restatement assertion failed'; END IF;
 END LOOP;
END $$;
-- +goose StatementEnd
-- +goose Down
SELECT 1;
