package httpapi_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ncecere/open-mcp-portal/internal/auditstream"
	"github.com/ncecere/open-mcp-portal/internal/testutil"
)

// #93: the status page's sink view reports the held sequence and the delivery
// result of its envelope, and drops the notice when the hold ends.
func TestAuditStreamStatusHeldNotice(t *testing.T) {
	app := newTestApp(t, nil)
	admin := app.signIn("admin")
	ctx := context.Background()
	owner := testutil.OwnerPool(t, app.Pool)
	run := func(q string, args ...any) {
		t.Helper()
		if _, err := app.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.New()
	run(`SELECT omp_audit_ext.create_sink($1,'held','full',true,true,100,'\x01','receiver.example','\x02',NULL,NULL,'hint')`, id)
	run(`INSERT INTO public.audit_log(actor_kind,action,target_type,metadata) VALUES('system','oversized','test',jsonb_build_object('x',repeat('x',17*1024*1024)))`)
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if _, err := (auditstream.Sealer{Pool: app.Pool}).Batch(ctx); err != nil {
			t.Fatal(err)
		}
		var sealed bool
		if err := app.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM omp_audit.chain WHERE flags&2<>0)`).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if sealed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watermark blocked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := owner.Exec(ctx, `SELECT omp_audit_ext.set_sinks_enabled(true,'0.15.0')`); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := app.Pool.QueryRow(ctx, `SELECT seq FROM omp_audit.chain WHERE flags&2<>0`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	attempt := func(cursor, first, last int64, outcome string, class any) {
		t.Helper()
		var token uuid.UUID
		if err := app.Pool.QueryRow(ctx, `SELECT omp_audit_ext.begin_attempt($1,1,$2,'data',$3,$4)`, id, cursor, first, last).Scan(&token); err != nil {
			t.Fatal(err)
		}
		run(`SELECT omp_audit_ext.finish_attempt($1,$2,$3,$4,false)`, id, token, outcome, class)
	}
	if seq > 1 {
		attempt(0, 1, seq-1, "ok", nil)
	}
	type notice struct {
		DeliveredAt  *time.Time `json:"delivered_at"`
		FailedAt     *time.Time `json:"failed_at"`
		FailureClass *string    `json:"failure_class"`
	}
	status := func() (*int64, *notice) {
		t.Helper()
		code, body := admin.raw("GET", "/v1/admin/audit/stream/status", nil, nil)
		if code != 200 {
			t.Fatalf("status: %d %s", code, body)
		}
		var out struct {
			Data struct {
				Sinks []struct {
					Held   *int64  `json:"held_oversized_seq"`
					Notice *notice `json:"held_notice"`
				}
			}
		}
		if err := json.Unmarshal(body, &out); err != nil || len(out.Data.Sinks) != 1 {
			t.Fatalf("status body: %v %s", err, body)
		}
		return out.Data.Sinks[0].Held, out.Data.Sinks[0].Notice
	}
	if held, n := status(); held == nil || *held != seq || n != nil {
		t.Fatalf("before any attempt: %v %+v", held, n)
	}
	attempt(seq-1, seq, seq, "failed", "timeout")
	if held, n := status(); held == nil || n == nil || n.FailedAt == nil || n.DeliveredAt != nil || n.FailureClass == nil || *n.FailureClass != "timeout" {
		t.Fatalf("after failure: %v %+v", held, n)
	}
	attempt(seq-1, seq, seq, "held", nil)
	if held, n := status(); held == nil || n == nil || n.DeliveredAt == nil || n.FailedAt != nil || n.FailureClass != nil {
		t.Fatalf("after delivery: %v %+v", held, n)
	}
	run(`SELECT omp_audit_ext.acknowledge_oversized($1,1,$2)`, id, seq)
	if held, n := status(); held != nil || n != nil {
		t.Fatalf("after acknowledgement: %v %+v", held, n)
	}
}
