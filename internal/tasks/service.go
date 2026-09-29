package tasks

import (
	"context"
	"log/slog"
	"strings"
	"time"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/grant"

	"github.com/garm-ai/tasksd/internal/store"
	"github.com/garm-ai/tasksd/internal/ulid"
)

// Service is the task queue.
type Service struct {
	DB     *store.DB
	Signal Signaller
	Now    func() time.Time
	Log    *slog.Logger

	// ClaimTTL is how long a claim holds before anybody may release it. A
	// person who claims a task and closes the tab must not take it with
	// them.
	ClaimTTL time.Duration
	// NewID mints a task id. A test replaces it; nothing else does.
	NewID func() string

	// GrantKeys verifies the approvals handed to Decide. Without it no
	// approval can be checked, and Serve refuses to start.
	GrantKeys GrantKeys
}

// DefaultClaimTTL is the claim window when none is configured.
const DefaultClaimTTL = 30 * time.Minute

// maxPageSize bounds one listing, matching the contract's own limit.
const maxPageSize = 100

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Service) newID() string {
	if s.NewID == nil {
		return ulid.NewNow()
	}
	return s.NewID()
}

func (s *Service) claimTTL() time.Duration {
	if s.ClaimTTL <= 0 {
		return DefaultClaimTTL
	}
	return s.ClaimTTL
}

// ---------------------------------------------------------------- create

// CreateInput is what a runner asks for when a call it is about to make
// needs a person.
type CreateInput struct {
	Kind            store.Kind
	Tool            string
	Subject         string
	Material        map[string]string
	Predicate       store.Predicate
	ExpiresIn       time.Duration
	DecisionType    string
	Question        string
	CatalogueDigest string
}

// Create opens a task for the run the caller is executing.
//
// The caller is a runner acting as an agent for a person: the principal is
// the person the run belongs to, the chain names the agent, and the run id
// is on the invocation. All three are required, because each of them is
// something the task cannot be answered without — there is nobody to exclude
// from deciding without the requester, nothing to signal without the run,
// and no way to attribute the ask without the agent.
//
// It is idempotent on the run, the tool and the values: a runner that
// retries after a timeout it never saw the answer to gets the task it
// already opened, not a second one in somebody's queue.
func (s *Service) Create(ctx context.Context, c Caller, in CreateInput) (store.Task, error) {
	if c.Kind != toolv1.PrincipalKind_PRINCIPAL_KIND_USER {
		return store.Task{}, refuse(CodeDenied,
			"a task is opened for a person: the call's principal is not a user")
	}
	if !c.HasAgent() {
		return store.Task{}, refuse(CodeDenied,
			"a task is opened by a runner acting as an agent: the call carries no agent")
	}
	if c.RunID == "" {
		return store.Task{}, refuse(CodeRefused, "the call names no run to signal")
	}
	kind := in.Kind
	if kind == "" {
		kind = store.KindApproval
	}
	if kind != store.KindApproval && kind != store.KindAsk {
		return store.Task{}, refusef(CodeRefused, "unknown task kind %q", kind)
	}
	for path := range in.Material {
		if err := grant.ValidPath(path); err != nil {
			return store.Task{}, refusef(CodeRefused, "material path %q is not usable: %v", path, err)
		}
	}
	if kind == store.KindApproval {
		if in.Tool == "" {
			return store.Task{}, refuse(CodeRefused, "an approval names the tool it is for")
		}
		if in.Predicate.MinClearance == "" && len(in.Predicate.Compartments) == 0 {
			return store.Task{}, refuse(CodeRefused,
				"an approval names who may decide it; the predicate is empty")
		}
	}
	if in.ExpiresIn <= 0 {
		return store.Task{}, refuse(CodeRefused, "a task names how long it is worth waiting for")
	}

	now := s.now()
	t := store.Task{
		ID: s.newID(), Tenant: c.Tenant, Kind: kind, RunID: c.RunID,
		Requester: c.Subject, Agent: c.Agent(), ToolFQN: in.Tool, Subject: in.Subject,
		Material: in.Material, MaterialDigest: grant.Digest(in.Material),
		Predicate: in.Predicate, State: store.StateOpen,
		DecisionType: in.DecisionType, Question: in.Question,
		CatalogueDigest: in.CatalogueDigest,
		CreatedAt:       now, ExpiresAt: now.Add(in.ExpiresIn),
	}
	out, created, err := s.DB.Create(ctx, t, store.Event{
		At: now, Actor: c.Agent(), Kind: "created",
		Detail: map[string]any{"tool": in.Tool, "requester": c.Subject, "kind": string(kind)},
	})
	if err != nil {
		return store.Task{}, s.fromStore(err, "a task could not be opened")
	}
	s.log().Info("task opened", "task", out.ID, "created", created,
		"tenant", c.Tenant, "run", c.RunID, "tool", in.Tool,
		"requester", c.Subject, "agent", c.Agent(), "call_id", c.CallID)
	return out, nil
}

// ---------------------------------------------------------------- read

// View is which screen a listing answers.
type View string

const (
	ViewQueue View = "QUEUE"
	ViewMine  View = "MINE"
	ViewDone  View = "DONE"
)

// List answers one screen.
//
// Four-eyes is applied here: the queue never shows a viewer a task their own
// run asked for, whatever their clearance. Which of the remaining tasks a
// viewer may see is not decided here at all — each card carries the task's
// predicate as its label and the daemon drops the ones the viewer does not
// reach, so this service never sees a clearance and never learns one.
func (s *Service) List(ctx context.Context, c Caller, v View, pageSize int, cursor string) ([]store.Task, string, error) {
	f := store.Filter{Tenant: c.Tenant, AfterID: cursor}
	switch v {
	case ViewQueue, "":
		f.States = []store.State{store.StateOpen}
		f.NotRequester = c.Subject
	case ViewMine:
		f.States = []store.State{store.StateOpen, store.StateClaimed}
		f.MineOf = c.Subject
	case ViewDone:
		f.States = []store.State{store.StateApproved, store.StateDeclined,
			store.StateAnswered, store.StateExpired}
	default:
		return nil, "", refusef(CodeRefused, "unknown view %q", v)
	}
	if pageSize <= 0 || pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	f.Limit = pageSize

	out, err := s.DB.List(ctx, f)
	if err != nil {
		return nil, "", s.fromStore(err, "a queue could not be read")
	}
	next := ""
	if len(out) == pageSize {
		next = out[len(out)-1].ID
	}
	return out, next, nil
}

// Get reads one task of the caller's tenant.
//
// The requester may read the frame of their own task — they are entitled to
// know that a decision is pending — and what they see of it is the card,
// which is labelled at the task's predicate and projected by the daemon.
func (s *Service) Get(ctx context.Context, c Caller, id string) (store.Task, error) {
	if !ulid.Valid(id) {
		return store.Task{}, refuse(CodeNoSuchTask, "no such task")
	}
	t, err := s.DB.Get(ctx, c.Tenant, id)
	if err != nil {
		return store.Task{}, s.fromStore(err, "a task could not be read")
	}
	return t, nil
}

// Trail is a task's audit trail and the triage recorded against it.
func (s *Service) Trail(ctx context.Context, t store.Task) ([]store.Event, []store.Triage, error) {
	evs, err := s.DB.Events(ctx, t.ID)
	if err != nil {
		return nil, nil, s.fromStore(err, "a task's trail could not be read")
	}
	tr, err := s.DB.TriageOf(ctx, t.ID)
	if err != nil {
		return nil, nil, s.fromStore(err, "a task's triage could not be read")
	}
	return evs, tr, nil
}

// ---------------------------------------------------------------- claim

// Claim takes a task out of the queue so two people do not decide it at
// once. First come, decided by the database.
func (s *Service) Claim(ctx context.Context, c Caller, id string) (store.Task, error) {
	t, err := s.Get(ctx, c, id)
	if err != nil {
		return store.Task{}, err
	}
	if t.Requester == c.Subject {
		return store.Task{}, refuse(CodeDenied, "this task is your own run's; somebody else decides it")
	}
	if t.State != store.StateOpen {
		return store.Task{}, refuse(CodeConflict, "this task is not open")
	}
	now := s.now()
	expires := now.Add(s.claimTTL())
	out, err := s.DB.Claim(ctx, c.Tenant, id, c.Subject, &expires, store.Event{
		At: now, Actor: c.Subject, Kind: "claimed",
	})
	if err != nil {
		return store.Task{}, s.fromStore(err, "a task could not be claimed")
	}
	return out, nil
}

// Release hands a claim back. It authorises nothing, so it is open to the
// claimant and to anybody once the claim's window has passed — a claim
// nobody can release is a task nobody can decide.
func (s *Service) Release(ctx context.Context, c Caller, id string) (store.Task, error) {
	t, err := s.Get(ctx, c, id)
	if err != nil {
		return store.Task{}, err
	}
	if t.State != store.StateClaimed {
		return store.Task{}, refuse(CodeConflict, "this task is not claimed")
	}
	now := s.now()
	out, err := s.DB.Release(ctx, c.Tenant, id, c.Subject, now, store.Event{
		At: now, Actor: c.Subject, Kind: "released",
	})
	if err != nil {
		return store.Task{}, s.fromStore(err, "a task could not be released")
	}
	return out, nil
}

// ---------------------------------------------------------------- decide

// DecideInput is a person's decision.
type DecideInput struct {
	TaskID   string
	Decision store.Decision
	Reason   string
	// Grant is the approval the deciding person's own client minted. It is
	// required to approve and meaningless otherwise.
	Grant string
	// Answer is the decision message, for a question rather than an
	// approval.
	Answer []byte
}

// Decide closes a task and tells the run.
//
// The rule that makes this safe is the first check: a call arriving with a
// delegation chain is refused. An agent cannot approve the thing it is about
// to do, and it cannot approve through a person's identity either, because
// what it would be holding is a delegated one. The token service refuses to
// mint an approval from such a token and the daemon refuses to verify one,
// so the rule holds in three places and survives any one of them being
// wrong.
func (s *Service) Decide(ctx context.Context, c Caller, in DecideInput) (store.Task, error) {
	if c.Delegated() {
		s.log().Warn("a delegated caller tried to decide a task",
			"reason", "delegated_approver", "task", in.TaskID,
			"subject", c.Subject, "agent", c.Agent(), "call_id", c.CallID)
		return store.Task{}, refuse(CodeDenied, DelegatedApproverMessage)
	}
	t, err := s.Get(ctx, c, in.TaskID)
	if err != nil {
		return store.Task{}, err
	}
	if t.Requester == c.Subject {
		return store.Task{}, refuse(CodeDenied,
			"this task is your own run's; somebody else decides it")
	}
	if t.State != store.StateClaimed {
		return store.Task{}, refuse(CodeConflict,
			"a decision needs the claim: claim this task first")
	}
	if t.Claimant != c.Subject {
		return store.Task{}, refuse(CodeDenied, "this task is claimed by somebody else")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return store.Task{}, refuse(CodeRefused, "a decision needs a reason")
	}

	now := s.now()
	dec := store.Decided{Reason: in.Reason, DecidedBy: c.Subject, At: now}
	detail := map[string]any{"reason": in.Reason}

	switch in.Decision {
	case store.DecisionApprove:
		if t.Kind != store.KindApproval {
			return store.Task{}, refuse(CodeRefused, "a question is answered, not approved")
		}
		if in.Grant == "" {
			return store.Task{}, refuse(CodeRefused, "an approval needs the grant you minted")
		}
		claims, err := checkGrant(in.Grant, s.GrantKeys, t, c.Subject, now)
		if err != nil {
			s.log().Warn("an approval was refused", "task", t.ID, "approver", c.Subject,
				"call_id", c.CallID, "err", err)
			return store.Task{}, err
		}
		dec.State, dec.Decision = store.StateApproved, store.DecisionApprove
		dec.GrantJTI, dec.Grant = claims.ID, in.Grant
		detail["grant_jti"] = claims.ID

	case store.DecisionDecline:
		dec.State, dec.Decision = store.StateDeclined, store.DecisionDecline

	case store.DecisionAnswer:
		if t.Kind != store.KindAsk {
			return store.Task{}, refuse(CodeRefused, "an approval is approved or declined, not answered")
		}
		if len(in.Answer) == 0 {
			return store.Task{}, refuse(CodeRefused, "an answer needs the decision message")
		}
		dec.State, dec.Decision, dec.Answer = store.StateAnswered, store.DecisionAnswer, in.Answer

	default:
		return store.Task{}, refusef(CodeRefused, "unknown decision %q", in.Decision)
	}

	out, err := s.DB.Decide(ctx, c.Tenant, t.ID, dec, store.Event{
		At: now, Actor: c.Subject, Kind: eventKind(dec.Decision), Detail: detail,
	})
	if err != nil {
		return store.Task{}, s.fromStore(err, "a decision could not be recorded")
	}
	s.signal(ctx, out, dec.Grant)
	return out, nil
}

// ---------------------------------------------------------------- triage

// Action is what a triage call does.
type Action string

const (
	ActionRecommend Action = "RECOMMEND"
	ActionComment   Action = "COMMENT"
	ActionReassign  Action = "REASSIGN"
	ActionDecline   Action = "DECLINE"
)

// TriageInput is an agent working on a task without deciding it.
type TriageInput struct {
	TaskID         string
	Action         Action
	Recommendation store.Decision
	Reason         string
	ReassignTo     store.Predicate
}

// Triage is working on a task without deciding it.
//
// It is open to an agent and to a person. An agent reads its queue and
// recommends, comments, hands a task to a stricter audience, or declines it
// with a reason; a person reassigning or commenting on a task nobody has got
// to is doing the same thing, and calling that a decision would be wrong.
//
// What nothing here does is approve. A recommendation of APPROVE is recorded
// as what the caller thinks and changes nothing, and there is no argument to
// this method that says yes. A no is cheap to reverse — the run may ask
// again — where a yes is not, which is why declining is here and approving
// is not.
//
// The actor is the agent when the call carries one and the person otherwise,
// and a decline through triage records both: who said no, and who they were
// acting for.
func (s *Service) Triage(ctx context.Context, c Caller, in TriageInput) (store.Task, error) {
	t, err := s.Get(ctx, c, in.TaskID)
	if err != nil {
		return store.Task{}, err
	}
	if t.State != store.StateOpen && t.State != store.StateClaimed {
		return store.Task{}, refuse(CodeConflict, "this task is already decided")
	}
	if strings.TrimSpace(in.Reason) == "" {
		return store.Task{}, refuse(CodeRefused, "triage needs a reason")
	}

	now := s.now()
	// The agent when there is one, the person when there is not. A triage
	// call is recorded under whoever made it.
	actor := c.Agent()
	onBehalfOf := c.Subject
	if actor == "" {
		actor, onBehalfOf = c.Subject, ""
	}
	tr := store.Triage{At: now, Actor: actor, Action: string(in.Action), Reason: in.Reason}

	switch in.Action {
	case ActionRecommend:
		if in.Recommendation != store.DecisionApprove && in.Recommendation != store.DecisionDecline {
			return store.Task{}, refuse(CodeRefused,
				"a recommendation is to approve or to decline")
		}
		tr.Recommendation = in.Recommendation
		out, err := s.DB.AddTriage(ctx, c.Tenant, t.ID, tr, store.Event{
			At: now, Actor: actor, Kind: "recommended",
			Detail: map[string]any{"recommendation": string(in.Recommendation), "reason": in.Reason},
		})
		if err != nil {
			return store.Task{}, s.fromStore(err, "a recommendation could not be recorded")
		}
		return out, nil

	case ActionComment:
		out, err := s.DB.AddTriage(ctx, c.Tenant, t.ID, tr, store.Event{
			At: now, Actor: actor, Kind: "commented",
			Detail: map[string]any{"reason": in.Reason},
		})
		if err != nil {
			return store.Task{}, s.fromStore(err, "a comment could not be recorded")
		}
		return out, nil

	case ActionReassign:
		if !stricter(in.ReassignTo, t.Predicate) {
			return store.Task{}, refuse(CodeDenied,
				"a task may only be handed to an audience at least as strict as the one it has")
		}
		out, err := s.DB.Reassign(ctx, c.Tenant, t.ID, in.ReassignTo, tr, store.Event{
			At: now, Actor: actor, Kind: "reassigned",
			Detail: map[string]any{"reason": in.Reason},
		})
		if err != nil {
			return store.Task{}, s.fromStore(err, "a task could not be reassigned")
		}
		return out, nil

	case ActionDecline:
		// A decline is a decision, so four eyes holds here as it holds on
		// decide_task: nobody closes the task their own run asked for, and
		// that is true of an agent acting for them too.
		if t.Requester == c.Subject {
			return store.Task{}, refuse(CodeDenied,
				"this task is your own run's; somebody else decides it")
		}
		out, err := s.DB.Decide(ctx, c.Tenant, t.ID, store.Decided{
			State: store.StateDeclined, Decision: store.DecisionDecline,
			Reason: in.Reason, DecidedBy: actor, OnBehalfOf: onBehalfOf, At: now,
		}, store.Event{
			At: now, Actor: actor, Kind: "declined",
			Detail: map[string]any{"reason": in.Reason, "on_behalf_of": onBehalfOf},
		})
		if err != nil {
			return store.Task{}, s.fromStore(err, "a decline could not be recorded")
		}
		// No grant is touched and no approval is minted: a decline
		// authorises nothing.
		s.signal(ctx, out, "")
		return out, nil

	default:
		return store.Task{}, refusef(CodeRefused, "unknown triage action %q", in.Action)
	}
}

// stricter reports whether a predicate asks for at least as much as another:
// the same clearance or higher, and every compartment the old one named.
func stricter(next, current store.Predicate) bool {
	if clearanceValue(next.MinClearance) < clearanceValue(current.MinClearance) {
		return false
	}
	have := make(map[string]bool, len(next.Compartments))
	for _, c := range next.Compartments {
		have[c] = true
	}
	for _, c := range current.Compartments {
		if !have[c] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- sweep

// Sweep marks what ran out of time.
//
// It exists beside the waiting run's own timeout because the two answer
// different questions: the run stops waiting, and this makes the row say so
// for a person looking at the queue.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	expired, err := s.DB.Expire(ctx, now, 200)
	if err != nil {
		return 0, err
	}
	for _, t := range expired {
		if err := s.DB.AppendEvent(ctx, store.Event{
			TaskID: t.ID, At: now, Actor: "tasksd", Kind: "expired",
		}); err != nil {
			s.log().Warn("an expiry could not be recorded", "task", t.ID, "err", err)
		}
	}
	return len(expired), nil
}

// ---------------------------------------------------------------- signal

// signal tells the waiting run, and records that it could not when it could
// not.
//
// The decision stands either way. A run that never hears waits until its own
// approval window closes, which is bounded; a decision rolled back because a
// message could not be published would be a person told their click did not
// count.
func (s *Service) signal(ctx context.Context, t store.Task, grantToken string) {
	if s.Signal == nil {
		return
	}
	seq := 1
	if evs, err := s.DB.Events(ctx, t.ID); err == nil {
		seq = len(evs)
	}
	err := s.Signal.Signal(ctx, Signal{
		Tenant: t.Tenant, RunID: t.RunID, TaskID: t.ID, Seq: seq,
		Decision: string(t.Decision), Reason: t.Reason, Grant: grantToken,
		DecidedBy: t.DecidedBy,
	})
	if err == nil {
		return
	}
	s.log().Error("a decision could not be delivered to its run",
		"task", t.ID, "run", t.RunID, "err", err)
	if err := s.DB.AppendEvent(ctx, store.Event{
		TaskID: t.ID, At: s.now(), Actor: "tasksd", Kind: "signal_failed",
	}); err != nil {
		s.log().Warn("a failed signal could not be recorded", "task", t.ID, "err", err)
	}
}

func eventKind(d store.Decision) string {
	switch d {
	case store.DecisionApprove:
		return "approved"
	case store.DecisionDecline:
		return "declined"
	default:
		return "answered"
	}
}
