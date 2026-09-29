package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/garm-ai/tasksd/internal/store"
)

const tenant = "example"

func at() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) }

func aTask(id string) store.Task {
	return store.Task{
		ID: id, Tenant: tenant, Kind: store.KindApproval, RunID: "run-1",
		Requester: "user:asker@example.com", ToolFQN: "payments.v1.initiate_payment",
		Subject:  "account:A-1",
		Material: map[string]string{"amount_minor_units": "2500", "currency_code": "EUR"},
		// The digest is the service's to compute; the store keeps whatever
		// it is handed, and the idempotency index is over that value.
		MaterialDigest: "sha256:aaa",
		Predicate: store.Predicate{
			MinClearance: "CLEARANCE_RESTRICTED", Compartments: []string{"payments"},
		},
		State: store.StateOpen, CreatedAt: at(), ExpiresAt: at().Add(time.Hour),
	}
}

func created() store.Event {
	return store.Event{At: at(), Actor: "agent:assistant", Kind: "created"}
}

func TestMigrateIsIdempotentAndRecordsItsVersion(t *testing.T) {
	db := store.TestDB(t)
	v, err := db.Migrate(t.Context())
	if err != nil {
		t.Fatalf("re-running the migrations: %v", err)
	}
	if v != 1 {
		t.Errorf("version = %d, want 1", v)
	}
	got, err := db.SchemaVersion(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("recorded version = %d, want 1", got)
	}
}

func TestCreateOpensATaskWithItsFirstTrailEntry(t *testing.T) {
	db := store.TestDB(t)
	got, isNew, err := db.Create(t.Context(), aTask("t1"), created())
	if err != nil {
		t.Fatal(err)
	}
	if !isNew {
		t.Error("the first create did not report a new task")
	}
	if got.State != store.StateOpen || got.Requester != "user:asker@example.com" {
		t.Errorf("task = %+v", got)
	}
	if got.Material["amount_minor_units"] != "2500" {
		t.Errorf("material = %v", got.Material)
	}
	evs, err := db.Events(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "created" || evs[0].Seq != 1 {
		t.Errorf("trail = %+v", evs)
	}
}

// A runner that retries an ask it never saw the answer to must get the task
// it already has, and nobody's queue may grow a second copy of it.
func TestCreateIsIdempotentOnTheRunTheToolAndTheValues(t *testing.T) {
	db := store.TestDB(t)
	first, _, err := db.Create(t.Context(), aTask("t1"), created())
	if err != nil {
		t.Fatal(err)
	}
	retry := aTask("t2") // a fresh id, the same ask
	second, isNew, err := db.Create(t.Context(), retry, created())
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Error("a retry of the same ask opened a second task")
	}
	if second.ID != first.ID {
		t.Errorf("the retry answered %s; the first task is %s", second.ID, first.ID)
	}
	evs, err := db.Events(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Errorf("the retry added a trail entry: %+v", evs)
	}
}

func TestADifferentValueOpensADifferentTask(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	other := aTask("t2")
	other.MaterialDigest = "sha256:bbb"
	_, isNew, err := db.Create(t.Context(), other, created())
	if err != nil {
		t.Fatal(err)
	}
	if !isNew {
		t.Error("a different amount reused the first task")
	}
}

func TestATaskOfAnotherTenantIsNotFound(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(t.Context(), "somebody-else", "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOnlyOneClaimWins(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	expires := at().Add(30 * time.Minute)
	got, err := db.Claim(t.Context(), tenant, "t1", "user:approver@example.com", &expires,
		store.Event{At: at(), Actor: "user:approver@example.com", Kind: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateClaimed || got.Claimant != "user:approver@example.com" {
		t.Errorf("task = %+v", got)
	}
	_, err = db.Claim(t.Context(), tenant, "t1", "user:reviewer@example.com", &expires,
		store.Event{At: at(), Actor: "user:reviewer@example.com", Kind: "claimed"})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("the second claim returned %v, want ErrConflict", err)
	}
}

func TestAClaimIsReleasedByItsHolderOrOnceItsWindowHasPassed(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	expires := at().Add(30 * time.Minute)
	if _, err := db.Claim(t.Context(), tenant, "t1", "user:approver@example.com", &expires,
		store.Event{At: at(), Actor: "user:approver@example.com", Kind: "claimed"}); err != nil {
		t.Fatal(err)
	}
	// Somebody else, while the claim holds.
	if _, err := db.Release(t.Context(), tenant, "t1", "user:reviewer@example.com", at(),
		store.Event{At: at(), Actor: "user:reviewer@example.com", Kind: "released"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("a stranger released a live claim: %v", err)
	}
	// The same stranger, after the window.
	got, err := db.Release(t.Context(), tenant, "t1", "user:reviewer@example.com", expires.Add(time.Minute),
		store.Event{At: at(), Actor: "user:reviewer@example.com", Kind: "released"})
	if err != nil {
		t.Fatalf("releasing a lapsed claim: %v", err)
	}
	if got.State != store.StateOpen || got.Claimant != "" {
		t.Errorf("task = %+v", got)
	}
}

func TestATaskIsDecidedOnce(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	dec := store.Decided{
		State: store.StateApproved, Decision: store.DecisionApprove,
		Reason: "checked against the invoice", DecidedBy: "user:approver@example.com",
		GrantJTI: "grn_1", Grant: "a.b.c", At: at(),
	}
	got, err := db.Decide(t.Context(), tenant, "t1", dec,
		store.Event{At: at(), Actor: "user:approver@example.com", Kind: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateApproved || got.GrantJTI != "grn_1" || got.DecidedAt == nil {
		t.Errorf("task = %+v", got)
	}
	if _, err := db.Decide(t.Context(), tenant, "t1", dec,
		store.Event{At: at(), Actor: "user:approver@example.com", Kind: "approved"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("the second decision returned %v, want ErrConflict", err)
	}
}

// The decision and the line of the trail that records it are one write. A
// trail missing the entry for what happened is the one gap this table exists
// to prevent.
func TestADecisionAndItsTrailEntryAreOneWrite(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Decide(t.Context(), tenant, "t1", store.Decided{
		State: store.StateDeclined, Decision: store.DecisionDecline,
		Reason: "the beneficiary is unknown", DecidedBy: "user:approver@example.com", At: at(),
	}, store.Event{At: at(), Actor: "user:approver@example.com", Kind: "declined",
		Detail: map[string]any{"reason": "the beneficiary is unknown"}}); err != nil {
		t.Fatal(err)
	}
	evs, err := db.Events(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[1].Kind != "declined" || evs[1].Seq != 2 {
		t.Fatalf("trail = %+v", evs)
	}
	if evs[1].Detail["reason"] != "the beneficiary is unknown" {
		t.Errorf("detail = %v", evs[1].Detail)
	}
}

func TestTriageIsRecordedAndChangesNothing(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	got, err := db.AddTriage(t.Context(), tenant, "t1", store.Triage{
		At: at(), Actor: "agent:reviewer", Action: "RECOMMEND",
		Recommendation: store.DecisionApprove, Reason: "the invoice matches",
	}, store.Event{At: at(), Actor: "agent:reviewer", Kind: "recommended"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateOpen || got.Decision != "" {
		t.Errorf("triage changed the task: %+v", got)
	}
	tr, err := db.TriageOf(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 1 || tr[0].Recommendation != store.DecisionApprove || tr[0].Seq != 1 {
		t.Errorf("triage = %+v", tr)
	}
}

func TestReassignNarrowsThePredicate(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	got, err := db.Reassign(t.Context(), tenant, "t1", store.Predicate{
		MinClearance: "CLEARANCE_RESTRICTED", Compartments: []string{"payments", "compliance"},
	}, store.Triage{At: at(), Actor: "agent:reviewer", Action: "REASSIGN", Reason: "needs compliance"},
		store.Event{At: at(), Actor: "agent:reviewer", Kind: "reassigned"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Predicate.Compartments) != 2 {
		t.Errorf("predicate = %+v", got.Predicate)
	}
}

func TestListSeparatesTheQueueFromWhatIsMine(t *testing.T) {
	db := store.TestDB(t)
	mine := aTask("t1")
	if _, _, err := db.Create(t.Context(), mine, created()); err != nil {
		t.Fatal(err)
	}
	other := aTask("t2")
	other.RunID, other.Requester, other.MaterialDigest = "run-2", "user:other@example.com", "sha256:bbb"
	if _, _, err := db.Create(t.Context(), other, created()); err != nil {
		t.Fatal(err)
	}

	queue, err := db.List(t.Context(), store.Filter{
		Tenant: tenant, States: []store.State{store.StateOpen},
		NotRequester: "user:asker@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].ID != "t2" {
		t.Errorf("queue = %+v", ids(queue))
	}

	minelist, err := db.List(t.Context(), store.Filter{
		Tenant: tenant, States: []store.State{store.StateOpen, store.StateClaimed},
		MineOf: "user:asker@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(minelist) != 1 || minelist[0].ID != "t1" {
		t.Errorf("mine = %+v", ids(minelist))
	}
}

func TestExpirySweepsWhatRanOutOfTime(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	got, err := db.Expire(t.Context(), at().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != store.StateExpired {
		t.Fatalf("expired = %+v", got)
	}
	// And an expired task is not decided again.
	if _, err := db.Decide(t.Context(), tenant, "t1", store.Decided{
		State: store.StateApproved, Decision: store.DecisionApprove,
		Reason: "too late", DecidedBy: "user:approver@example.com", At: at(),
	}, store.Event{At: at(), Actor: "user:approver@example.com", Kind: "approved"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("an expired task was decided: %v", err)
	}
}

// No column but `grant` may ever hold a credential. The rule is worth a test
// rather than a comment, because the material and the trail's detail are
// both free-form and both written from values a caller supplied.
func TestNoColumnButTheGrantHoldsACredential(t *testing.T) {
	db := store.TestDB(t)
	if _, _, err := db.Create(t.Context(), aTask("t1"), created()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Decide(t.Context(), tenant, "t1", store.Decided{
		State: store.StateApproved, Decision: store.DecisionApprove,
		Reason: "checked", DecidedBy: "user:approver@example.com",
		GrantJTI: "grn_1", Grant: "header.payload.signature", At: at(),
	}, store.Event{At: at(), Actor: "user:approver@example.com", Kind: "approved",
		Detail: map[string]any{"grant_jti": "grn_1"}}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Pool().Query(t.Context(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_name = 'tasks' AND table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, c := range columns {
		if c == "grant" {
			continue
		}
		var n int
		q := `SELECT count(*) FROM tasks WHERE ` + quoted(c) + `::text LIKE '%header.payload.signature%'`
		if err := db.Pool().QueryRow(t.Context(), q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		if n != 0 {
			t.Errorf("column %q holds the grant", c)
		}
	}
	var n int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM task_events WHERE detail::text LIKE '%header.payload.signature%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the trail holds the grant")
	}
}

func quoted(c string) string { return `"` + c + `"` }

func ids(ts []store.Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
