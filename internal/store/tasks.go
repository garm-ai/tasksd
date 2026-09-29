package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// State is where a task is. It is the spelling the contract uses, so a row
// read out of the database and a field on the wire say the same word.
type State string

const (
	StateOpen     State = "OPEN"
	StateClaimed  State = "CLAIMED"
	StateApproved State = "APPROVED"
	StateDeclined State = "DECLINED"
	StateAnswered State = "ANSWERED"
	StateExpired  State = "EXPIRED"
)

// Kind is what the task asks for: a decision on a tool call, or an answer to
// a question.
type Kind string

const (
	KindApproval Kind = "APPROVAL"
	KindAsk      Kind = "ASK"
)

// Decision is what was decided.
type Decision string

const (
	DecisionApprove Decision = "APPROVE"
	DecisionDecline Decision = "DECLINE"
	DecisionAnswer  Decision = "ANSWER"
)

// Predicate is who may see and decide a task: the clearance and the
// compartments the tool's approval block asks an approver to hold.
//
// The excluded subject is not here. Four-eyes is the requester on the row,
// which the service compares with the caller, so there is one place a task
// records who asked for it.
type Predicate struct {
	MinClearance string   `json:"min_clearance"`
	Compartments []string `json:"compartments"`
}

// Task is one row.
type Task struct {
	ID     string
	Tenant string
	Kind   Kind
	RunID  string
	// Requester is the person the run was started for, and the one person
	// who may not decide this task.
	Requester string
	ToolFQN   string
	Subject   string
	Material  map[string]string
	// MaterialDigest is what the grant is bound to, computed once when the
	// task is opened so a later read cannot recompute it differently.
	MaterialDigest  string
	Predicate       Predicate
	State           State
	Claimant        string
	ClaimExpiresAt  *time.Time
	Decision        Decision
	Reason          string
	DecidedBy       string
	OnBehalfOf      string
	GrantJTI        string
	Grant           string
	DecisionType    string
	Question        string
	CatalogueDigest string
	Answer          []byte

	CreatedAt time.Time
	ExpiresAt time.Time
	DecidedAt *time.Time
}

// Event is one line of a task's audit trail.
type Event struct {
	TaskID string
	Seq    int
	At     time.Time
	Actor  string
	Kind   string
	Detail map[string]any
}

// Triage is a recommendation, a comment or a reassignment somebody added to
// a task without deciding it.
type Triage struct {
	TaskID         string
	Seq            int
	At             time.Time
	Actor          string
	Action         string
	Recommendation Decision
	Reason         string
}

// Filter is a listing's query. Tenant is required: there is no listing
// across tenants, so there is no way to forget one.
type Filter struct {
	Tenant string
	States []State
	// Requester and NotRequester are the two halves of four-eyes on a
	// listing: mine, and everybody else's.
	Requester    string
	NotRequester string
	Claimant     string
	// MineOf matches a task this subject asked for OR holds the claim on,
	// which is one screen and cannot be two AND-ed filters.
	MineOf string
	RunID  string
	// AfterID continues where the previous page stopped; rows come back by
	// id descending, so this reads "id < AfterID".
	AfterID string
	Limit   int
}

const taskColumns = `id, tenant, kind, run_id, requester, tool_fqn, subject, material,
	material_digest, predicate, state, claimant, claim_expires_at, decision, reason,
	decided_by, on_behalf_of, grant_jti, "grant", decision_type, question,
	catalogue_digest, answer, created_at, expires_at, decided_at`

// Create opens a task, or finds the one the same ask already opened.
//
// Idempotent on (tenant, run_id, tool, material digest), which is the unique
// index: a runner that retries after a timeout it never saw the answer to
// gets the task it already has, with the id it was given the first time,
// and no second approval appears in anybody's queue. The bool says which of
// the two happened, so the caller can write the `created` event once.
func (d *DB) Create(ctx context.Context, t Task, ev Event) (Task, bool, error) {
	material, err := json.Marshal(nonNilMaterial(t.Material))
	if err != nil {
		return Task{}, false, err
	}
	pred, err := json.Marshal(t.Predicate)
	if err != nil {
		return Task{}, false, err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, fmt.Errorf("store: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO tasks (id, tenant, kind, run_id, requester, tool_fqn, subject,
		                   material, material_digest, predicate, state,
		                   decision_type, question, catalogue_digest,
		                   created_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (tenant, run_id, tool_fqn, material_digest) DO NOTHING`,
		t.ID, t.Tenant, string(t.Kind), t.RunID, t.Requester, t.ToolFQN, t.Subject,
		material, t.MaterialDigest, pred, string(StateOpen),
		t.DecisionType, t.Question, t.CatalogueDigest, t.CreatedAt, t.ExpiresAt)
	if err != nil {
		return Task{}, false, fmt.Errorf("store: opening a task: %w", err)
	}
	created := tag.RowsAffected() == 1

	if created {
		ev.TaskID = t.ID
		if err := appendEventTx(ctx, tx, ev); err != nil {
			return Task{}, false, err
		}
	}

	row := tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks
		WHERE tenant = $1 AND run_id = $2 AND tool_fqn = $3 AND material_digest = $4`,
		t.Tenant, t.RunID, t.ToolFQN, t.MaterialDigest)
	out, err := scanTask(row)
	if err != nil {
		return Task{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Task{}, false, fmt.Errorf("store: %w", err)
	}
	return out, created, nil
}

// Get reads one task of one tenant. A task of another tenant is not found,
// which is the same answer an id that never existed gets.
func (d *DB) Get(ctx context.Context, tenant, id string) (Task, error) {
	row := d.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1 AND tenant = $2`, id, tenant)
	t, err := scanTask(row)
	if isNoRows(err) {
		return Task{}, fmt.Errorf("store: task %s: %w", id, ErrNotFound)
	}
	return t, err
}

// List answers a queue.
func (d *DB) List(ctx context.Context, f Filter) ([]Task, error) {
	if f.Tenant == "" {
		return nil, fmt.Errorf("store: a listing names no tenant: %w", ErrNotFound)
	}
	var (
		where []string
		args  []any
	)
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	add("tenant = $%d", f.Tenant)
	if len(f.States) > 0 {
		states := make([]string, len(f.States))
		for i, s := range f.States {
			states[i] = string(s)
		}
		add("state = ANY($%d)", states)
	}
	if f.Requester != "" {
		add("requester = $%d", f.Requester)
	}
	if f.NotRequester != "" {
		add("requester <> $%d", f.NotRequester)
	}
	if f.Claimant != "" {
		add("claimant = $%d", f.Claimant)
	}
	if f.MineOf != "" {
		args = append(args, f.MineOf)
		where = append(where, fmt.Sprintf("(requester = $%d OR claimant = $%d)", len(args), len(args)))
	}
	if f.RunID != "" {
		add("run_id = $%d", f.RunID)
	}
	if f.AfterID != "" {
		add("id < $%d", f.AfterID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing tasks: %w", err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Claim is first-come, decided by the database.
//
// The UPDATE names the state it requires, so two claims racing cannot both
// win however they interleave: one matches the row and the other matches
// nothing. A read-then-write in Go would need a lock to say the same thing.
func (d *DB) Claim(ctx context.Context, tenant, id, claimant string,
	expires *time.Time, ev Event) (Task, error) {
	return d.change(ctx, tenant, id, ev, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET state = 'CLAIMED', claimant = $3, claim_expires_at = $4
			WHERE id = $1 AND tenant = $2 AND state = 'OPEN'`, id, tenant, claimant, expires)
		if err != nil {
			return fmt.Errorf("store: claiming task %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("store: task %s is not open: %w", id, ErrConflict)
		}
		return nil
	})
}

// Release hands a claim back. It succeeds for the claimant, and for anybody
// once the claim's own deadline has passed — a claim nobody can release is a
// task nobody can decide.
func (d *DB) Release(ctx context.Context, tenant, id, claimant string,
	now time.Time, ev Event) (Task, error) {
	return d.change(ctx, tenant, id, ev, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET state = 'OPEN', claimant = '', claim_expires_at = NULL
			WHERE id = $1 AND tenant = $2 AND state = 'CLAIMED'
			  AND (claimant = $3 OR (claim_expires_at IS NOT NULL AND claim_expires_at <= $4))`,
			id, tenant, claimant, now)
		if err != nil {
			return fmt.Errorf("store: releasing task %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("store: task %s is not claimed by %s: %w", id, claimant, ErrConflict)
		}
		return nil
	})
}

// Decided is what a decision writes.
type Decided struct {
	State      State
	Decision   Decision
	Reason     string
	DecidedBy  string
	OnBehalfOf string
	GrantJTI   string
	Grant      string
	Answer     []byte
	At         time.Time
}

// Decide records a terminal decision, once.
//
// The WHERE clause admits OPEN and CLAIMED and nothing else, so a second
// decision — a double-clicked button, a retried request, a second approver
// racing the first — changes no rows and returns ErrConflict. That matters
// more here than anywhere else in this file: every decision signals a
// waiting run, and two of them would dispatch the tool twice.
//
// The event is written in the same transaction as the decision, so a trail
// can never be missing the one line that says what happened.
func (d *DB) Decide(ctx context.Context, tenant, id string, dec Decided, ev Event) (Task, error) {
	switch dec.State {
	case StateApproved, StateDeclined, StateAnswered:
	default:
		return Task{}, fmt.Errorf("store: a decision cannot leave a task %s: %w", dec.State, ErrConflict)
	}
	if dec.DecidedBy == "" {
		return Task{}, fmt.Errorf("store: a decision on task %s names no decider: %w", id, ErrConflict)
	}
	return d.change(ctx, tenant, id, ev, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET state = $3, decision = $4, reason = $5, decided_by = $6,
			                 on_behalf_of = $7, grant_jti = $8, "grant" = $9,
			                 answer = $10, decided_at = $11
			WHERE id = $1 AND tenant = $2 AND state IN ('OPEN','CLAIMED')`,
			id, tenant, string(dec.State), string(dec.Decision), dec.Reason, dec.DecidedBy,
			dec.OnBehalfOf, dec.GrantJTI, dec.Grant, dec.Answer, dec.At)
		if err != nil {
			return fmt.Errorf("store: deciding task %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("store: task %s is already decided: %w", id, ErrConflict)
		}
		return nil
	})
}

// Reassign narrows a task's predicate. The service is what checks that the
// new one is at least as strict; this writes it.
func (d *DB) Reassign(ctx context.Context, tenant, id string, p Predicate,
	tr Triage, ev Event) (Task, error) {
	pred, err := json.Marshal(p)
	if err != nil {
		return Task{}, err
	}
	return d.change(ctx, tenant, id, ev, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET predicate = $3
			WHERE id = $1 AND tenant = $2 AND state IN ('OPEN','CLAIMED')`, id, tenant, pred)
		if err != nil {
			return fmt.Errorf("store: reassigning task %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("store: task %s is decided: %w", id, ErrConflict)
		}
		tr.TaskID = id
		return appendTriageTx(ctx, tx, tr)
	})
}

// AddTriage records a recommendation or a comment. Nothing about the task
// changes; that is what triage is.
func (d *DB) AddTriage(ctx context.Context, tenant, id string, tr Triage, ev Event) (Task, error) {
	return d.change(ctx, tenant, id, ev, func(ctx context.Context, tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx,
			`SELECT state FROM tasks WHERE id = $1 AND tenant = $2 FOR UPDATE`, id, tenant).Scan(&state)
		if isNoRows(err) {
			return fmt.Errorf("store: task %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if State(state) != StateOpen && State(state) != StateClaimed {
			return fmt.Errorf("store: task %s is decided: %w", id, ErrConflict)
		}
		tr.TaskID = id
		return appendTriageTx(ctx, tx, tr)
	})
}

// Expire marks what ran out of time and returns the rows it changed, so the
// caller can write one event per task without a second query.
func (d *DB) Expire(ctx context.Context, now time.Time, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.pool.Query(ctx, `
		UPDATE tasks SET state = 'EXPIRED', decided_at = $1
		WHERE id IN (
			SELECT id FROM tasks
			WHERE state IN ('OPEN','CLAIMED') AND expires_at <= $1
			ORDER BY id LIMIT $2
		)
		RETURNING `+taskColumns, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: expiring tasks: %w", err)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// change runs one row-changing statement and its trail entry in a single
// transaction, and answers with the row as it ended up.
func (d *DB) change(ctx context.Context, tenant, id string, ev Event,
	apply func(context.Context, pgx.Tx) error) (Task, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return Task{}, fmt.Errorf("store: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := apply(ctx, tx); err != nil {
		return Task{}, err
	}
	if ev.Kind != "" {
		ev.TaskID = id
		if err := appendEventTx(ctx, tx, ev); err != nil {
			return Task{}, err
		}
	}
	row := tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1 AND tenant = $2`, id, tenant)
	out, err := scanTask(row)
	if isNoRows(err) {
		return Task{}, fmt.Errorf("store: task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Task{}, fmt.Errorf("store: %w", err)
	}
	return out, nil
}

// AppendEvent writes one line of the trail on its own, for the events no row
// change goes with — an expiry swept in the background, a signal that could
// not be published.
func (d *DB) AppendEvent(ctx context.Context, ev Event) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := appendEventTx(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// appendEventTx takes the next sequence number and writes the row. There is
// no update and no delete for this table anywhere; the primary key refuses a
// repeated seq, which is how a caller learns it lost a race instead of
// overwriting what the winner wrote.
func appendEventTx(ctx context.Context, tx pgx.Tx, ev Event) error {
	detail := ev.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_events (task_id, seq, at, actor, kind, detail)
		VALUES ($1, (SELECT COALESCE(MAX(seq),0)+1 FROM task_events WHERE task_id = $1),
		        $2, $3, $4, $5)`,
		ev.TaskID, ev.At, ev.Actor, ev.Kind, body); err != nil {
		return wrapUnique(err, fmt.Sprintf("store: appending an event to task %s", ev.TaskID))
	}
	return nil
}

func appendTriageTx(ctx context.Context, tx pgx.Tx, tr Triage) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO triage (task_id, seq, at, actor, action, recommendation, reason)
		VALUES ($1, (SELECT COALESCE(MAX(seq),0)+1 FROM triage WHERE task_id = $1),
		        $2, $3, $4, $5, $6)`,
		tr.TaskID, tr.At, tr.Actor, tr.Action, string(tr.Recommendation), tr.Reason); err != nil {
		return wrapUnique(err, fmt.Sprintf("store: recording triage on task %s", tr.TaskID))
	}
	return nil
}

// Events is a task's trail, oldest first.
func (d *DB) Events(ctx context.Context, taskID string) ([]Event, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT task_id, seq, at, actor, kind, detail
		FROM task_events WHERE task_id = $1 ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: listing the trail of task %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var (
			e      Event
			detail []byte
		)
		if err := rows.Scan(&e.TaskID, &e.Seq, &e.At, &e.Actor, &e.Kind, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TriageOf is what has been recommended, commented or reassigned so far.
func (d *DB) TriageOf(ctx context.Context, taskID string) ([]Triage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT task_id, seq, at, actor, action, recommendation, reason
		FROM triage WHERE task_id = $1 ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: listing the triage of task %s: %w", taskID, err)
	}
	defer rows.Close()
	out := []Triage{}
	for rows.Next() {
		var (
			tr  Triage
			rec string
		)
		if err := rows.Scan(&tr.TaskID, &tr.Seq, &tr.At, &tr.Actor, &tr.Action, &rec, &tr.Reason); err != nil {
			return nil, err
		}
		tr.Recommendation = Decision(rec)
		out = append(out, tr)
	}
	return out, rows.Err()
}

func scanTask(s scanner) (Task, error) {
	var (
		t        Task
		kind     string
		state    string
		decision string
		material []byte
		pred     []byte
	)
	err := s.Scan(&t.ID, &t.Tenant, &kind, &t.RunID, &t.Requester, &t.ToolFQN, &t.Subject,
		&material, &t.MaterialDigest, &pred, &state, &t.Claimant, &t.ClaimExpiresAt,
		&decision, &t.Reason, &t.DecidedBy, &t.OnBehalfOf, &t.GrantJTI, &t.Grant,
		&t.DecisionType, &t.Question, &t.CatalogueDigest, &t.Answer,
		&t.CreatedAt, &t.ExpiresAt, &t.DecidedAt)
	if err != nil {
		return Task{}, err
	}
	t.Kind, t.State, t.Decision = Kind(kind), State(state), Decision(decision)
	if err := json.Unmarshal(material, &t.Material); err != nil {
		return Task{}, fmt.Errorf("store: task %s material: %w", t.ID, err)
	}
	if err := json.Unmarshal(pred, &t.Predicate); err != nil {
		return Task{}, fmt.Errorf("store: task %s predicate: %w", t.ID, err)
	}
	if t.Material == nil {
		t.Material = map[string]string{}
	}
	return t, nil
}

func nonNilMaterial(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// wrapUnique turns Postgres's unique-violation into ErrConflict, so a caller
// racing for a sequence number can tell "try again" apart from every other
// way a write can fail.
func wrapUnique(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%s: %w", what, ErrConflict)
	}
	return fmt.Errorf("%s: %w", what, err)
}
