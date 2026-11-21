package auditstream

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
)

const oversizedRow = `INSERT INTO public.audit_log(actor_kind,action,target_type,metadata) VALUES('system','oversized','test',jsonb_build_object('x',repeat('x',17*1024*1024)))`

type holdMarker struct {
	seq, revision int64
	delivered     bool
	class         *string
}

func readHold(t *testing.T, p *pgxpool.Pool, id uuid.UUID) *holdMarker {
	t.Helper()
	var m holdMarker
	err := p.QueryRow(ctx, `SELECT held_seq,sink_revision,delivered_at IS NOT NULL,failure_class FROM omp_audit_ext.sink_holds WHERE sink_id=$1`, id).Scan(&m.seq, &m.revision, &m.delivered, &m.class)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &m
}

// passes runs the real sender pass n times, ignoring backoff eligibility by
// moving the persisted next attempt into the past first.
func passes(t *testing.T, owner *pgxpool.Pool, s *Sender, leader *pgx.Conn, id uuid.UUID, n int) {
	t.Helper()
	for range n {
		exec(t, owner, `UPDATE omp_audit_ext.sinks SET next_attempt_at=now()-interval '1 hour' WHERE id=$1`, id)
		if err := s.pass(ctx, leader); err != nil {
			t.Fatal(err)
		}
	}
}

// #93: repeated passes while held send the held envelope once after a success,
// retry it after a failure, and a new leader honours the durable marker.
func TestSenderHeldEnvelopeDeliveredOnce(t *testing.T) {
	s, leader, id, status, bodies := senderReceiver(t, true)
	owner := testutil.OwnerPool(t, s.Pool)
	exec(t, s.Pool, oversizedRow)
	drain(t, s.Pool)

	*status = 503
	passes(t, owner, s, leader, id, 2)
	if len(*bodies) != 2 {
		t.Fatalf("failed held envelope not retried: %d", len(*bodies))
	}
	if m := readHold(t, s.Pool, id); m == nil || m.seq != 1 || m.revision != 1 || m.delivered || m.class == nil || *m.class != "http_5xx" {
		t.Fatalf("failure not recorded: %+v", m)
	}
	if count(t, s.Pool, `SELECT consecutive_failures FROM omp_audit_ext.sinks WHERE id=$1`, id) != 2 {
		t.Fatal("held failures left the existing backoff")
	}

	*status = 204
	passes(t, owner, s, leader, id, 4)
	if len(*bodies) != 3 {
		t.Fatalf("held envelope redelivered after success: %d", len(*bodies))
	}
	if m := readHold(t, s.Pool, id); m == nil || !m.delivered || m.class != nil {
		t.Fatalf("delivery not recorded: %+v", m)
	}
	if count(t, s.Pool, `SELECT cursor_seq FROM omp_audit_ext.sinks WHERE id=$1`, id) != 0 || count(t, s.Pool, `SELECT consecutive_failures FROM omp_audit_ext.sinks WHERE id=$1`, id) != 0 {
		t.Fatal("hold or recovery changed")
	}
	if count(t, s.Pool, `SELECT count(*) FROM notification_events WHERE event='audit.sink_recovered'`) != 0 {
		t.Fatal("unnotified streak produced a recovery notice")
	}

	// Leader change: a new session and sender instance read the durable marker.
	_ = leader.Close(ctx)
	next, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(ctx)
	restarted := &Sender{Pool: s.Pool, Box: s.Box, Client: s.Client}
	passes(t, owner, restarted, next, id, 3)
	if len(*bodies) != 3 {
		t.Fatalf("new leader redelivered the held envelope: %d", len(*bodies))
	}

	// A v0.15.2 sender has no marker check; its begin/finish keep the marker
	// delivered, and a stale token can neither deliver nor mark.
	stale := sinkBegin(t, s.Pool, id, 1, 0, "data", 1, 1)
	current := sinkBegin(t, s.Pool, id, 1, 0, "data", 1, 1)
	sinkReject(t, s.Pool, `SELECT omp_audit_ext.finish_attempt($1,$2,'failed','timeout',false)`, id, stale)
	if m := readHold(t, s.Pool, id); m == nil || !m.delivered {
		t.Fatal("stale token changed the marker")
	}
	exec(t, s.Pool, `SELECT omp_audit_ext.finish_attempt($1,$2,'held',NULL,false)`, id, current)
	passes(t, owner, restarted, next, id, 2)
	if len(*bodies) != 3 || count(t, s.Pool, `SELECT cursor_seq FROM omp_audit_ext.sinks WHERE id=$1`, id) != 0 {
		t.Fatal("mixed-version attempt broke the hold")
	}
}

// #93: acknowledging ends the hold and its marker; delivery resumes, the next
// oversized row gets its own marker, and a settings edit resends the envelope.
func TestSenderHeldMarkerAcknowledgeAndEdit(t *testing.T) {
	s, leader, id, _, bodies := senderReceiver(t, true)
	owner := testutil.OwnerPool(t, s.Pool)
	exec(t, s.Pool, oversizedRow)
	exec(t, s.Pool, oversizedRow)
	exec(t, s.Pool, insert)
	drain(t, s.Pool)
	passes(t, owner, s, leader, id, 2)
	if len(*bodies) != 1 || readHold(t, s.Pool, id) == nil {
		t.Fatalf("first hold: %d bodies", len(*bodies))
	}
	first := readHold(t, s.Pool, id)

	exec(t, s.Pool, `SELECT omp_audit_ext.acknowledge_oversized($1,1,1)`, id)
	if readHold(t, s.Pool, id) != nil {
		t.Fatal("acknowledgement left the marker")
	}
	passes(t, owner, s, leader, id, 3)
	m := readHold(t, s.Pool, id)
	if len(*bodies) != 2 || m == nil || m.seq != 2 || !m.delivered || count(t, s.Pool, `SELECT cursor_seq FROM omp_audit_ext.sinks WHERE id=$1`, id) != 1 {
		t.Fatalf("next oversized row: %d bodies, %+v (was %+v)", len(*bodies), m, first)
	}

	// An edit (here re-enabling with the same settings) bumps the revision:
	// the marker is cleared and the new revision is sent the envelope once.
	exec(t, s.Pool, `SELECT omp_audit_ext.update_sink(id,revision,name,profile,required,false,batch_max_records,url_cipher,url_hint,secret_cipher,secret_previous_cipher,secret_previous_until,secret_hint) FROM omp_audit_ext.sinks WHERE id=$1`, id)
	if readHold(t, s.Pool, id) != nil {
		t.Fatal("edit left the marker")
	}
	exec(t, s.Pool, `SELECT omp_audit_ext.update_sink(id,revision,name,profile,required,true,batch_max_records,url_cipher,url_hint,secret_cipher,secret_previous_cipher,secret_previous_until,secret_hint) FROM omp_audit_ext.sinks WHERE id=$1`, id)
	passes(t, owner, s, leader, id, 3)
	if m = readHold(t, s.Pool, id); len(*bodies) != 3 || m == nil || m.revision != 3 || !m.delivered {
		t.Fatalf("after edit: %d bodies, %+v", len(*bodies), m)
	}

	exec(t, s.Pool, `SELECT omp_audit_ext.acknowledge_oversized($1,3,2)`, id)
	passes(t, owner, s, leader, id, 1)
	if count(t, s.Pool, `SELECT cursor_seq FROM omp_audit_ext.sinks WHERE id=$1`, id) < 3 || readHold(t, s.Pool, id) != nil || len(*bodies) != 4 {
		t.Fatal("delivery did not resume after acknowledgement")
	}

	// Deleting the sink removes any marker with it.
	exec(t, s.Pool, oversizedRow)
	drain(t, s.Pool)
	passes(t, owner, s, leader, id, 3)
	if readHold(t, s.Pool, id) == nil {
		t.Fatal("third hold not recorded")
	}
	exec(t, s.Pool, `SELECT omp_audit_ext.delete_sink(id,revision) FROM omp_audit_ext.sinks WHERE id=$1`, id)
	if count(t, s.Pool, `SELECT count(*) FROM omp_audit_ext.sink_holds`) != 0 {
		t.Fatal("delete left the marker")
	}
}
