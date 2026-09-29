package tasksd_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	cardv1 "github.com/garm-ai/garm/contracts/garm/card/v1"
	tasksv1 "github.com/garm-ai/garm/contracts/garm/tasks/v1"
	toolv1 "github.com/garm-ai/garm/contracts/garm/tool/v1"
)

// theMaterial is what the person is being asked about, and what the
// approval's digest is computed over.
func theMaterial() map[string]string {
	return map[string]string{"amount_minor_units": "2500", "currency_code": "EUR"}
}

func createRequest() *tasksv1.CreateTaskRequest {
	return &tasksv1.CreateTaskRequest{
		Kind:     tasksv1.Kind_APPROVAL.Enum(),
		Tool:     proto.String(theTool),
		Subject:  proto.String(theSubject),
		Material: theMaterial(),
		Predicate: &tasksv1.Predicate{
			MinClearance: toolv1.Clearance_CLEARANCE_RESTRICTED.Enum(),
			Compartments: []string{"payments"},
		},
		ExpiresInSeconds: proto.Uint32(900),
	}
}

// openTask is the first half of almost every case: a run asks for an
// approval and gets a task id back.
func openTask(t *testing.T, f *fixture) string {
	t.Helper()
	var res tasksv1.CreateTaskResponse
	f.ok(t, routeCreate, runner(theAsker, theAgent, theRunID), createRequest(), &res)
	if res.GetTaskId() == "" {
		t.Fatal("create_task answered no task id")
	}
	return res.GetTaskId()
}

// A runner acting as an agent for a person opens a task, and the task
// records all three: the run to signal, the person who asked, and the agent
// that asked for them.
func TestARunnerOpensATaskForTheRunItIsExecuting(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var got tasksv1.Task
	f.ok(t, routeGet, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, &got)
	if got.GetState() != tasksv1.State_OPEN {
		t.Errorf("state = %s, want OPEN", got.GetState())
	}
	if got.GetRequester() != theAsker || got.GetRunId() != theRunID {
		t.Errorf("requester = %q, run = %q", got.GetRequester(), got.GetRunId())
	}
	if got.GetTool() != theTool || got.GetSubject() != theSubject {
		t.Errorf("tool = %q, subject = %q", got.GetTool(), got.GetSubject())
	}
	if len(got.GetEvents()) != 1 || got.GetEvents()[0].GetActor() != theAgent {
		t.Errorf("trail = %+v", got.GetEvents())
	}
	// The card points at the run, and names the service that serves its
	// card: the agent the runner was acting as.
	refs := got.GetCard().GetRefs()
	if len(refs) != 1 || refs[0].GetSubjectId() != theRunID || refs[0].GetToolFqn() != theAgent {
		t.Errorf("refs = %+v", refs)
	}
}

// A person calling for themselves is not a runner. Without an agent in the
// chain there is nobody to attribute the ask to and nothing to exclude from
// deciding it.
func TestAPersonCannotOpenATask(t *testing.T) {
	f := newFixture(t)
	code, why := f.call(t, routeCreate, person(theApprove), createRequest(), nil)
	if code != "403" {
		t.Fatalf("create_task by a person answered %s: %s", code, why)
	}
}

// A runner that retries a call it never saw the answer to must get the task
// it already opened.
func TestARetriedAskFindsTheTaskItAlreadyOpened(t *testing.T) {
	f := newFixture(t)
	first := openTask(t, f)
	second := openTask(t, f)
	if first != second {
		t.Errorf("a retry opened %s beside %s", second, first)
	}
}

// Four eyes, on the queue: an approver sees the task and the person whose
// run asked for it does not.
func TestTheQueueShowsATaskToAnApproverAndNotToTheRequester(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var approver tasksv1.ListTasksResponse
	f.ok(t, routeList, person(theApprove),
		&tasksv1.ListTasksRequest{View: tasksv1.ListTasksRequest_QUEUE.Enum()}, &approver)
	if len(approver.GetCards()) != 1 || approver.GetCards()[0].GetSubjectId() != id {
		t.Fatalf("the approver's queue = %+v", cardIDs(approver.GetCards()))
	}
	card := approver.GetCards()[0]
	// Every card carries the task's audience, because who may see a task is
	// a property of the row and the daemon is what drops what a viewer does
	// not reach.
	if card.GetAccess().GetClearance() != toolv1.Clearance_CLEARANCE_RESTRICTED {
		t.Errorf("the card's label = %+v", card.GetAccess())
	}
	if len(card.GetAccess().GetCompartments()) != 1 || card.GetAccess().GetCompartments()[0] != "payments" {
		t.Errorf("the card's compartments = %v", card.GetAccess().GetCompartments())
	}

	var requester tasksv1.ListTasksResponse
	f.ok(t, routeList, person(theAsker),
		&tasksv1.ListTasksRequest{View: tasksv1.ListTasksRequest_QUEUE.Enum()}, &requester)
	if len(requester.GetCards()) != 0 {
		t.Errorf("the requester's queue = %+v", cardIDs(requester.GetCards()))
	}
}

// The requester may read the frame of their own task — they are entitled to
// know a decision is pending — and may not decide it.
func TestTheRequesterMayReadTheirOwnTaskAndNotDecideIt(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var got tasksv1.Task
	f.ok(t, routeGet, person(theAsker), &cardv1.TaskRef{TaskId: proto.String(id)}, &got)
	if got.GetTaskId() != id {
		t.Fatalf("get_task answered %q", got.GetTaskId())
	}
	code, _ := f.call(t, routeClaim, person(theAsker), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	if code != "403" {
		t.Errorf("the requester claimed their own task: %s", code)
	}
}

// The whole path a person takes: claim, approve with the token their own
// client minted, and the run is told.
func TestAnApproverClaimsDecidesAndTheRunIsTold(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var claimed tasksv1.Task
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, &claimed)
	if claimed.GetState() != tasksv1.State_CLAIMED || claimed.GetClaimant() != theApprove {
		t.Fatalf("claimed = %+v", claimed.GetState())
	}

	var decided tasksv1.Task
	f.ok(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("the invoice matches"), Grant: proto.String(f.grantFor(t, id, theMaterial(), nil)),
	}, &decided)

	if decided.GetState() != tasksv1.State_APPROVED {
		t.Fatalf("state = %s, want APPROVED", decided.GetState())
	}
	if decided.GetDecidedBy() != theApprove || decided.GetReason() != "the invoice matches" {
		t.Errorf("decided by %q, reason %q", decided.GetDecidedBy(), decided.GetReason())
	}
	if decided.GetGrantJti() != "grn_01test" {
		t.Errorf("grant jti = %q; the approval's identifier is recorded", decided.GetGrantJti())
	}

	signals := f.Signals()
	if len(signals) != 1 {
		t.Fatalf("the run was told %d times", len(signals))
	}
	s := signals[0]
	if s.RunID != theRunID || s.TaskID != id || s.Decision != "APPROVE" {
		t.Errorf("signal = %+v", s)
	}
	if s.Grant == "" {
		t.Error("the approval did not travel to the run")
	}
}

// An agent cannot approve, and the refusal does not depend on the token: the
// call arrives with a delegation chain and that is the whole of it.
func TestADecisionThroughAnAgentIsRefused(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)

	delegated := caller{
		Subject: theApprove, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theAgent},
	}
	code, why := f.call(t, routeDecide, delegated, &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("looks fine to me"), Grant: proto.String(f.grantFor(t, id, theMaterial(), nil)),
	}, nil)
	if code != "403" {
		t.Fatalf("a delegated decision answered %s: %s", code, why)
	}

	var still tasksv1.Task
	f.ok(t, routeGet, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, &still)
	if still.GetState() != tasksv1.State_CLAIMED {
		t.Errorf("state = %s; the refusal changed the task", still.GetState())
	}
	if len(f.Signals()) != 0 {
		t.Error("a refused decision told the run")
	}
}

// An approval is held to the task it was given on. Two identical payments
// asked twice digest the same, so without the task claim one approval would
// close either of them.
func TestAnApprovalForAnotherTaskIsRefused(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)

	other := f.grantFor(t, "another-task", theMaterial(), nil)
	code, why := f.call(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("checked"), Grant: proto.String(other),
	}, nil)
	if code != "400" {
		t.Fatalf("an approval for another task answered %s: %s", code, why)
	}
}

// An approval over other values is refused: the digest is computed from what
// this service stored, not from anything the caller sent.
func TestAnApprovalOverOtherValuesIsRefused(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)

	bigger := map[string]string{"amount_minor_units": "250000", "currency_code": "EUR"}
	code, why := f.call(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("checked"), Grant: proto.String(f.grantFor(t, id, bigger, nil)),
	}, nil)
	if code != "400" {
		t.Fatalf("an approval over other values answered %s: %s", code, why)
	}
}

// A token nobody signed is not a weak approval; it is a string somebody
// sent.
func TestAnUnsignedApprovalIsRefused(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)

	code, why := f.call(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("checked"),
		Grant: proto.String(unsigned(map[string]any{"jti": "grn_forged", "garm_grant": map[string]any{
			"tool": theTool, "subject": theSubject, "task": id,
		}})),
	}, nil)
	if code != "400" {
		t.Fatalf("an unsigned approval answered %s: %s", code, why)
	}
}

// A decision needs the claim, and only its holder decides.
func TestADecisionNeedsTheClaim(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	code, _ := f.call(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_DECLINE.Enum(),
		Reason: proto.String("no"),
	}, nil)
	if code != "409" {
		t.Errorf("deciding an unclaimed task answered %s", code)
	}

	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	code, _ = f.call(t, routeDecide, person("user:somebody-else@example.com"),
		&tasksv1.DecideTaskRequest{TaskId: proto.String(id),
			Decision: tasksv1.Decision_DECLINE.Enum(), Reason: proto.String("no")}, nil)
	if code != "403" {
		t.Errorf("deciding somebody else's claim answered %s", code)
	}
}

// An agent may say no. It is recorded under the agent with the person it was
// acting for, no approval is involved, and the run is told.
func TestAnAgentDeclinesATaskWithAReason(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	triager := caller{
		Subject: theApprove, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theTriager},
	}
	var got tasksv1.Task
	f.ok(t, routeTriage, triager, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_DECLINE.Enum(),
		Reason: proto.String("the beneficiary is not on the approved list"),
	}, &got)

	if got.GetState() != tasksv1.State_DECLINED {
		t.Fatalf("state = %s, want DECLINED", got.GetState())
	}
	if got.GetDecidedBy() != theTriager {
		t.Errorf("decided by %q; a triage decline is recorded under the agent", got.GetDecidedBy())
	}
	if got.GetGrantJti() != "" {
		t.Errorf("a decline minted or recorded an approval: %q", got.GetGrantJti())
	}
	signals := f.Signals()
	if len(signals) != 1 || signals[0].Decision != "DECLINE" || signals[0].Grant != "" {
		t.Errorf("signal = %+v", signals)
	}
}

// A recommendation to approve is what the agent thinks, and changes nothing.
func TestAnAgentRecommendsAndNothingIsDecided(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	triager := caller{
		Subject: theApprove, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theTriager},
	}
	var got tasksv1.Task
	f.ok(t, routeTriage, triager, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_RECOMMEND.Enum(),
		Recommendation: tasksv1.Decision_APPROVE.Enum(), Reason: proto.String("the invoice matches"),
	}, &got)

	if got.GetState() != tasksv1.State_OPEN {
		t.Errorf("state = %s; a recommendation decided the task", got.GetState())
	}
	if len(got.GetTriage()) != 1 || got.GetTriage()[0].GetRecommendation() != tasksv1.Decision_APPROVE {
		t.Fatalf("triage = %+v", got.GetTriage())
	}
	if len(f.Signals()) != 0 {
		t.Error("a recommendation told the run")
	}
}

// Triage is open to a person as well. Somebody commenting on a task nobody
// has got to is working on it, not deciding it — and it is recorded under
// them, with nobody they were acting for.
func TestAPersonMayTriage(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var got tasksv1.Task
	f.ok(t, routeTriage, person(theApprove), &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_COMMENT.Enum(),
		Reason: proto.String("the amount looks unusual for this account"),
	}, &got)

	if got.GetState() != tasksv1.State_OPEN {
		t.Errorf("state = %s; a comment decided the task", got.GetState())
	}
	if len(got.GetTriage()) != 1 || got.GetTriage()[0].GetActor() != theApprove {
		t.Fatalf("triage = %+v", got.GetTriage())
	}
	if len(f.Signals()) != 0 {
		t.Error("a comment told the run")
	}
}

// The two directions the audience draws: a person may triage, and the same
// person may not decide when their call arrives through an agent.
func TestAPersonMayTriageThroughAnAgentAndNotDecideThroughOne(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	delegated := caller{
		Subject: theApprove, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theTriager},
	}
	var got tasksv1.Task
	f.ok(t, routeTriage, delegated, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_COMMENT.Enum(),
		Reason: proto.String("the beneficiary was added last week"),
	}, &got)
	if len(got.GetTriage()) != 1 || got.GetTriage()[0].GetActor() != theTriager {
		t.Errorf("triage = %+v; a delegated comment is recorded under the agent", got.GetTriage())
	}

	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	code, why := f.call(t, routeDecide, delegated, &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_DECLINE.Enum(),
		Reason: proto.String("no"),
	}, nil)
	if code != "403" {
		t.Fatalf("a delegated decline through decide_task answered %s: %s", code, why)
	}
}

// A decline through triage is still a decision, so four eyes holds: the
// person whose run asked cannot close it, and neither can an agent acting
// for them.
func TestATriageDeclineIsRefusedToTheRequester(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	forTheAsker := caller{
		Subject: theAsker, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theTriager},
	}
	code, why := f.call(t, routeTriage, forTheAsker, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_DECLINE.Enum(),
		Reason: proto.String("never mind"),
	}, nil)
	if code != "403" {
		t.Fatalf("the requester declined their own task: %s %s", code, why)
	}
}

// A task may be handed to a stricter audience and never to a looser one.
func TestAReassignmentMayOnlyNarrowTheAudience(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	triager := caller{
		Subject: theApprove, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER,
		Act: []string{theTriager},
	}

	code, why := f.call(t, routeTriage, triager, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_REASSIGN.Enum(),
		Reason:     proto.String("anybody can do this"),
		ReassignTo: &tasksv1.Predicate{MinClearance: toolv1.Clearance_CLEARANCE_INTERNAL.Enum()},
	}, nil)
	if code != "403" {
		t.Fatalf("a looser audience answered %s: %s", code, why)
	}

	var got tasksv1.Task
	f.ok(t, routeTriage, triager, &tasksv1.TriageTaskRequest{
		TaskId: proto.String(id), Action: tasksv1.TriageTaskRequest_REASSIGN.Enum(),
		Reason: proto.String("this needs compliance"),
		ReassignTo: &tasksv1.Predicate{
			MinClearance: toolv1.Clearance_CLEARANCE_RESTRICTED.Enum(),
			Compartments: []string{"payments", "compliance"},
		},
	}, &got)
	if len(got.GetPredicate().GetCompartments()) != 2 {
		t.Errorf("predicate = %+v", got.GetPredicate())
	}
}

// The card a person decides from: the values, labelled at the task's
// audience, with the path each one came from so a client can ask the tool
// being approved for its own rendering of the same values.
func TestTheApprovalCardCarriesTheValuesAndTheirLabels(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var card cardv1.Card
	f.ok(t, routeCard, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, &card)

	if card.GetKind() != cardv1.Kind_TASK || card.GetState() != cardv1.State_OPEN {
		t.Errorf("kind = %s, state = %s", card.GetKind(), card.GetState())
	}
	if card.GetAccess().GetClearance() != toolv1.Clearance_CLEARANCE_RESTRICTED {
		t.Errorf("the card's label = %+v", card.GetAccess())
	}
	found := map[string]string{}
	for _, el := range card.GetBody() {
		collectFacts(el, found)
	}
	if found["amount_minor_units"] != "2500" || found["currency_code"] != "EUR" {
		t.Errorf("the card's values = %v", found)
	}
	if len(card.GetActions()) != 2 {
		t.Errorf("actions = %+v", card.GetActions())
	}
}

// A task is decided once. The second decision changes nothing and the run is
// told once.
func TestATaskIsDecidedOnce(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)

	req := &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_DECLINE.Enum(),
		Reason: proto.String("the beneficiary is unknown"),
	}
	f.ok(t, routeDecide, person(theApprove), req, nil)
	code, _ := f.call(t, routeDecide, person(theApprove), req, nil)
	if code != "409" {
		t.Errorf("the second decision answered %s", code)
	}
	if len(f.Signals()) != 1 {
		t.Errorf("the run was told %d times", len(f.Signals()))
	}
}

// A task of another tenant is not there. The same answer an id that never
// existed gets, so a caller learns nothing by probing.
func TestATaskOfAnotherTenantIsNotFound(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	other := person(theApprove)
	other.Tenant = "somebody-else"
	code, _ := f.call(t, routeGet, other, &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	if code != "404" {
		t.Errorf("a task of another tenant answered %s", code)
	}
}

func collectFacts(el *cardv1.Element, into map[string]string) {
	switch of := el.GetOf().(type) {
	case *cardv1.Element_Facts:
		for _, fact := range of.Facts.GetFacts() {
			if fact.GetField() != "" {
				into[fact.GetField()] = fact.GetValue()
			}
		}
	case *cardv1.Element_Section:
		for _, child := range of.Section.GetElements() {
			collectFacts(child, into)
		}
	}
}

func cardIDs(cards []*cardv1.Card) []string {
	out := make([]string, len(cards))
	for i, c := range cards {
		out[i] = c.GetSubjectId()
	}
	return out
}
