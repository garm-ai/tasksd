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
	Kind store.Kind
	Tool string
	// Subject is the GRANT's subject -- the resource the decision is about.
	Subject string
	// Requester is the person the task is opened FOR, and the one person
	// four-eyes excludes from deciding it. It comes from the request because
	// the CALLER is a runner acting for that person, not the person.
	Requester string
	// RunID is the run to tell when this task is decided. It comes from the
	// REQUEST and not from the invocation, because garmd never puts a run id on
	// one — and must not learn how, knowing nothing about runs being one of that
	// daemon's invariants. It is an assertion used for routing and never for
	// authorization; the field's own comment in the proto has the whole of it.
	RunID           string
	Material        map[string]string
	Predicate       store.Predicate
	ExpiresIn       time.Duration
	DecisionType    string
	Question        string
	CatalogueDigest string
}

// Create opens a task for the run the caller is executing.
//
// The caller is a runner acting as an agent for a person: the principal is the
// service, the chain names the agent, and the REQUEST names the person and the
// run. All four are required, because each is something the task cannot be
// answered without — there is nobody to exclude from deciding without the
// requester, nothing to signal without the run, and no way to attribute the ask
// without the agent.
//
// THE RUN COMES FROM THE REQUEST, and that is what made this method reachable.
// It used to read `attribution.run_id`, which garmd never sets: the daemon builds
// an invocation with a tenant and a correlation id and reads an incoming
// attribution for the correlation id alone. So every call that arrived through
// the daemon named no run and was refused, and nothing in the platform had ever
// opened a task on this service. Teaching the daemon about runs was not the fix —
// knowing nothing about agents or runs is one of its invariants — so the runner,
// which is the only party that knows which run it is executing, says so in the
// request.
//
// It is idempotent on the run, the tool and the values: a runner that
// retries after a timeout it never saw the answer to gets the task it
// already opened, not a second one in somebody's queue.
func (s *Service) Create(ctx context.Context, c Caller, in CreateInput) (store.Task, error) {
	if c.Kind != toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE {
		return store.Task{}, refuse(CodeDenied,
			"a task is opened by a runner acting for a person: the call's principal is not a service")
	}
	if in.Requester == "" {
		return store.Task{}, refuse(CodeRefused,
			"the call names no requester, so there is nobody to exclude from deciding")
	}
	if !c.HasAgent() {
		return store.Task{}, refuse(CodeDenied,
			"a task is opened by a runner acting as an agent: the call carries no agent")
	}
	if in.RunID == "" {
		return store.Task{}, refuse(CodeRefused,
			"the request names no run_id, so there is nothing to tell when this is decided")
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
		ID: s.newID(), Tenant: c.Tenant, Kind: kind, RunID: in.RunID,
		Requester: in.Requester, Agent: c.Agent(), ToolFQN: in.Tool, Subject: in.Subject,
		// The service that opened it, from the call's own subject and never
		// from a request field. It is what get_task_grant hands the approval
		// out against, so it has to be the attested value: the token service
		// derives a service subject from the authenticated client credential,
		// and a field would be a caller naming whoever it liked.
		OpenedBy: c.Subject,
		Material: in.Material, MaterialDigest: grant.Digest(in.Material),
		Predicate: in.Predicate, State: store.StateOpen,
		DecisionType: in.DecisionType, Question: in.Question,
		CatalogueDigest: in.CatalogueDigest,
		CreatedAt:       now, ExpiresAt: now.Add(in.ExpiresIn),
	}
	out, created, err := s.DB.Create(ctx, t, store.Event{
		At: now, Actor: c.Agent(), Kind: "created",
		Detail: map[string]any{"tool": in.Tool, "requester": in.Requester, "kind": string(kind)},
	})
	if err != nil {
		return store.Task{}, s.fromStore(err, "a task could not be opened")
	}
	s.log().Info("task opened", "task", out.ID, "created", created,
		"tenant", c.Tenant, "run", in.RunID, "tool", in.Tool,
		"requester", in.Requester, "opened_by", c.Subject, "agent", c.Agent(),
		"call_id", c.CallID)
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

// ------------------------------------------------------------- the read back

// Approval is the answer of get_task_grant: the credential, its identifier, and
// the task it closed.
//
// Not store.Task. A method that exists to hand over ONE credential should not
// also be a projection of the row — what a woken run needs is the grant to
// re-dispatch the call it was parked on, and nothing else.
type Approval struct {
	TaskID string
	// Grant is the bearer the deciding person's own client minted. It reaches no
	// log line and no trail entry here, and the method's audit annotation keeps
	// it out of the ledger by value.
	Grant string
	// GrantJTI identifies the approval without being one, which is what a
	// runner records and what this service logs.
	GrantJTI string
}

// ReadGrant hands a parked run the approval its task was decided with.
//
// The decided event carries a task id and an outcome and never the grant (R5) —
// a bearer on a stream is readable by anything that can consume the subject for
// that stream's retention, and a replay would replay a credential. So a woken run
// fetches the approval here instead, and without this it fails with no grant
// while the approval sits in this service's own store.
//
// Everything deciding whether the caller may have it is in mayReadGrant, in one
// function and deliberately: it is the only check standing between a scoped
// caller and a credential, and a reader looking for it should not have to
// assemble it out of three places.
func (s *Service) ReadGrant(ctx context.Context, c Caller, taskID string) (Approval, error) {
	t, err := s.Get(ctx, c, taskID)
	if err != nil {
		return Approval{}, err
	}
	if err := s.mayReadGrant(c, t); err != nil {
		return Approval{}, err
	}
	// The identifier and never the approval. This is the one log line on the
	// credential path, and what it says is which approval was collected, by
	// whom, for which run.
	s.log().Info("a run collected its approval", "task", t.ID, "run", t.RunID,
		"grant_jti", t.GrantJTI, "caller", c.Subject, "call_id", c.CallID)
	return Approval{TaskID: t.ID, Grant: t.Grant, GrantJTI: t.GrantJTI}, nil
}

// mayReadGrant is the gate on get_task_grant: the service that opened a task is
// the one that may collect its approval.
//
// THE IDENTITY IS ATTESTED, WHICH IS WHY IT IS THE CHECK. A service subject is
// derived at the token service from the authenticated client credential, so a
// caller cannot present a different one — and with a bound token the presenting
// workload is held to that identity by `cnf`, so a stolen one is useless off the
// runner it was issued to.
//
// NOT THE RUN, and that is not an omission. A per-run gate would read
// `CallContext.run_id`, and garmd never writes it: the daemon builds the
// invocation's attribution with a tenant and a correlation id and nothing else
// (`internal/toolplane/core.go`, withInvocationContext), so the value arrives
// empty on every call through the daemon and a gate over it refused everything —
// which is what this function used to do. The run id on the row is no better: it
// is a value the RUNNER asserted on create_task, so comparing the two would be
// one unattested claim checked against another. The run id's job is to key the
// decided event's subject, and routing decides who HEARS a decision, never who
// may act on one.
//
// Audience is no substitute either: an audience decides who is OFFERED a tool and
// gates nothing, so AUDIENCE_RUNNER alone would leave every runner able to read
// every task's approval.
//
// WHAT THIS DOES NOT PROTECT AGAINST, written down so the next reader does not
// take it for an oversight: within one runner, any run can read any task's
// approval. That is accepted deliberately. A compromised runner holds every
// approval it legitimately fetches anyway, and a per-task capability would have
// been minted, stored and checkpointed in that same process's own state — so it
// would have bought nothing against the same threat, while adding a second
// bearer to steal. The trust boundary is the SERVICE, and the gate is drawn at
// the trust boundary. Per-run isolation needs per-run credentials, and that is
// the right end state only for a runner shared across tenants.
//
// The order matters as much as the checks. The identity comes first and answers
// exactly what a task that never existed answers, so a caller who did not open
// this task learns nothing about it — not its state, not that it is there. Only
// the service that opened it reaches the refusals below.
func (s *Service) mayReadGrant(c Caller, t store.Task) error {
	// 1. The identity, and "no such task" for every way it can fail: a caller
	//    who may not read this task must not be able to tell it from one that is
	//    not there. One reason code for the three, because a code per reason is
	//    an oracle for which task ids exist and which of them are decided.
	if c.Kind != toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE {
		return refuse(CodeNoSuchTask, "no such task")
	}
	if t.OpenedBy == "" {
		// A row from before `opened_by` existed. Nothing records who opened it,
		// so no caller can be the service that did, and it fails closed rather
		// than matching on emptiness.
		//
		// The comparison below would refuse it anyway — CallerFrom rejects a
		// call with no subject, so nothing ever equals the empty string — and
		// this branch is here regardless, because an unrecorded opener is a
		// different fact from the wrong one and only one of them is an
		// operator's problem. The caller is told exactly what a stranger is
		// told; the log says which task can no longer resume, so the run can be
		// asked again rather than waiting on an approval nobody can hand over.
		s.log().Warn("a task opened before this service recorded the opener cannot "+
			"hand its approval back", "task", t.ID, "run", t.RunID, "caller", c.Subject)
		return refuse(CodeNoSuchTask, "no such task")
	}
	if c.Subject != t.OpenedBy {
		return refuse(CodeNoSuchTask, "no such task")
	}

	// 2. Something to hand over. A refusal with a reason and never an empty
	//    success: a run told "here is your approval: nothing" would present an
	//    empty grant and be refused at the daemon with no reason anybody can
	//    trace back to the decline that caused it.
	switch {
	case t.Kind != store.KindApproval:
		return refuse(CodeRefused,
			"this task is a question, not an approval: it was answered and minted no grant")
	case t.State == store.StateOpen || t.State == store.StateClaimed:
		return refuse(CodeConflict, "this task is not decided yet, so there is no approval")
	case t.State == store.StateDeclined:
		return refuse(CodeConflict,
			"this task was declined: a decline authorises nothing and mints no approval")
	case t.State == store.StateExpired:
		return refuse(CodeConflict,
			"this task expired undecided, so no approval was ever minted")
	case t.State != store.StateApproved:
		return refusef(CodeConflict, "this task is %s, and only an approved task has an approval", t.State)
	case t.Grant == "":
		// An approved task with no grant recorded is a broken invariant of this
		// service, not something the caller did. Decide records both together.
		s.log().Error("an approved task carries no approval to hand back",
			"task", t.ID, "run", t.RunID, "grant_jti", t.GrantJTI)
		return refuse(CodeBroke, "the approval recorded for this task could not be read")
	}

	return nil
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
	s.log().Error("a decision could not be delivered to its run over the fast path; "+
		"the run now waits for its own durable sleep to wake it instead of being told promptly",
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
