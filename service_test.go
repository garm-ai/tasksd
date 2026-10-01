package tasksd_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	cardv1 "github.com/garm-ai/contracts/garm/card/v1"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
	"github.com/garm-ai/tasksd/internal/signal"
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
		// The person this task is opened FOR. The caller is the runner, which
		// is not that person, so the requester has to be said rather than
		// inferred.
		Requester: proto.String(theAsker),
		// The run to tell when this is decided, said rather than read off the
		// invocation: garmd builds an attribution with a tenant and a correlation
		// id and never a run id, so a service asking the invocation for a run is
		// asking for something that never arrives.
		RunId: proto.String(theRunID),
	}
}

// openTask is the first half of almost every case: a run asks for an
// approval and gets a task id back.
func openTask(t *testing.T, f *fixture) string {
	t.Helper()
	var res tasksv1.CreateTaskResponse
	f.ok(t, routeCreate, runner(theAgent, theRunID), createRequest(), &res)
	if res.GetTaskId() == "" {
		t.Fatal("create_task answered no task id")
	}
	return res.GetTaskId()
}

// A person calling create_task directly is refused. A task is opened BY a
// runner FOR a person; a person opening their own approval would be asking
// themselves, and four-eyes would have nobody left to exclude.
func TestCreateRefusesAPersonCallingItDirectly(t *testing.T) {
	f := newFixture(t)
	code, why := f.call(t, routeCreate, person(theAsker), createRequest(), nil)
	if code != "403" {
		t.Fatalf("a person opened a task directly: code %q, %s", code, why)
	}
	if !strings.Contains(why, "service") {
		t.Errorf("refusal was %q, want it to name the principal kind", why)
	}
}

// No requester is a refusal, not a default. There is nobody to exclude from
// deciding without it, so a task opened that way could be approved by the very
// person who caused it.
func TestCreateRefusesAnEmptyRequester(t *testing.T) {
	f := newFixture(t)
	req := createRequest()
	req.Requester = nil
	code, why := f.call(t, routeCreate, runner(theAgent, theRunID), req, nil)
	if code == "" {
		t.Fatal("a task was opened with no requester; there is nobody to exclude")
	}
	if !strings.Contains(why, "requester") {
		t.Errorf("refusal was %q, want it to name the requester", why)
	}
}

// The four-eyes exclusion reads the REQUESTER FIELD and not the caller. That is
// the whole point of the field: the caller is a service, and excluding a
// service from deciding excludes nobody who was ever going to decide.
func TestTheFourEyesExclusionUsesTheRequesterField(t *testing.T) {
	f := newFixture(t)
	id := openTask(t, f)

	var got tasksv1.Task
	f.ok(t, routeGet, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, &got)
	if got.GetRequester() != theAsker {
		t.Errorf("requester = %q, want the request's %q — not the caller", got.GetRequester(), theAsker)
	}
	if got.GetRequester() == theRunnerSvc {
		t.Error("the requester is the calling service; the field was ignored")
	}

	// And the person named by the field is the one who may not decide.
	code, why := f.call(t, routeClaim, person(theAsker), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	if code == "" {
		t.Fatalf("the requester reached their own task: %s", why)
	}
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

// ------------------------------------------------- reading the approval back

// decidedTask is a task taken all the way to APPROVED, with the approval the
// deciding person's client minted recorded against it. It returns the task id and
// the bearer, so a test can assert what must never appear anywhere.
func decidedTask(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	id := openTask(t, f)
	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	bearer := f.grantFor(t, id, theMaterial(), nil)
	f.ok(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("the invoice matches"), Grant: proto.String(bearer),
	}, nil)
	return id, bearer
}

// anotherRunner is a second service on the same broker: the same shape as the
// one that opened the task, and a different attested subject. Nothing but the
// subject differs, so a test using it varies exactly what the gate reads.
func anotherRunner() caller {
	c := runner(theAgent, theRunID)
	c.Subject = "svc:somebody-else"
	return c
}

// get_task_grant is mounted, reachable on its own route, and HANDS THE APPROVAL
// TO THE SERVICE THAT OPENED THE TASK.
//
// This is the whole resume path driven end to end for the first time: a runner
// opens a task, a person approves it with a token their own client minted, and
// the runner collects that token back. Before the gate was drawn at the service
// identity it refused unconditionally, and a parked run could never resume.
//
// It also proves `create_task` records the opener, which nothing on the wire can
// show: `opened_by` is this service's own column and is on no message. If Create
// stopped setting it, the row would record no opener and this call would be told
// the task is not there.
//
// The caller names NO run of its own — `runner(theAgent, theRunID)` puts one on
// the invocation, but garmd never does, and the gate reads none. What authorises
// this is the subject.
func TestTheOpeningServiceCollectsItsApproval(t *testing.T) {
	f := newFixture(t)
	id, bearer := decidedTask(t, f)

	var got tasksv1.TaskGrant
	f.ok(t, routeGrant, runner(theAgent, theRunID),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, &got)
	if got.GetTaskId() != id {
		t.Errorf("the answer names task %q, want %q", got.GetTaskId(), id)
	}
	if got.GetGrant() != bearer {
		t.Errorf("the approval handed back is not the one the approver minted:\n got %q\nwant %q",
			got.GetGrant(), bearer)
	}
	if got.GetGrantJti() != "grn_01test" {
		t.Errorf("grant jti = %q, want the identifier of the approval that was recorded",
			got.GetGrantJti())
	}
}

// A service that did not open the task is told what a caller of a task that never
// existed is told, and so is a person.
//
// This is the half of the gate that keeps it from being a listing: a refusal
// saying "not yours" would still tell the caller the task is there and decided,
// and an audience gates nothing at all — it decides who is OFFERED a tool.
func TestOnlyTheOpeningServiceIsToldTheTaskIsThere(t *testing.T) {
	f := newFixture(t)
	id, _ := decidedTask(t, f)

	code, why := f.call(t, routeGrant, anotherRunner(),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, nil)
	if code != "404" {
		t.Fatalf("another service got %q: %s, want 404", code, why)
	}
	if why != "no such task" {
		t.Errorf("refusal is %q, want the words a missing task gets", why)
	}

	// A person, with no delegation chain and a USER principal. An approval is a
	// bearer minted for a run to present, and there is no run behind a keyboard.
	code, why = f.call(t, routeGrant, person(theApprove),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, nil)
	if code != "404" || why != "no such task" {
		t.Errorf("a person got %q: %s, want 404 / no such task", code, why)
	}
}

// A task with no approval refuses with a reason, and never succeeds emptily. A
// run handed nothing would present nothing to the daemon and fail there, with no
// trace back to the decline that caused it.
func TestATaskWithNoApprovalRefusesWithAReason(t *testing.T) {
	f := newFixture(t)

	// Not decided yet.
	open := openTask(t, f)
	code, why := f.call(t, routeGrant, runner(theAgent, theRunID),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(open)}, nil)
	if code != "409" || !strings.Contains(why, "not decided") {
		t.Errorf("an undecided task answered %q: %s, want 409 naming that it is not decided", code, why)
	}

	// Declined. A decline authorises nothing and mints no approval, and that has
	// to read as a refusal rather than an approval that happens to be empty.
	f.ok(t, routeTriage, person(theApprove), &tasksv1.TriageTaskRequest{
		TaskId: proto.String(open), Action: tasksv1.TriageTaskRequest_DECLINE.Enum(),
		Reason: proto.String("the beneficiary is not on the approved list"),
	}, nil)
	code, why = f.call(t, routeGrant, runner(theAgent, theRunID),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(open)}, nil)
	if code != "409" || !strings.Contains(why, "declined") {
		t.Errorf("a declined task answered %q: %s, want 409 naming the decline", code, why)
	}
}

// A task of another tenant is not there, on this route as on every other.
func TestTheGrantReadBackIsConfinedToItsTenant(t *testing.T) {
	f := newFixture(t)
	id, _ := decidedTask(t, f)

	other := runner(theAgent, theRunID)
	other.Tenant = "somebody-else"
	code, _ := f.call(t, routeGrant, other, &tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, nil)
	if code != "404" {
		t.Errorf("a task of another tenant answered %q", code)
	}
}

// NO BEARER REACHES A LOG LINE.
//
// The whole path is driven first — a task opened, claimed, approved with a real
// signed token, the approval read back SUCCESSFULLY, and a second read by another
// service refused — and then every line the service wrote is searched for the
// token and for each of its three segments. The successful read is what makes
// this test worth having: that is the one call in the service that puts a bearer
// in a reply, so it is the one that could put it in a log. The segments matter separately: a handler that logged a
// request or an error by value would print the whole token, and a formatter that
// truncated one would still print the header and payload.
//
// The fixture captures the log at DEBUG for this test. Before it did, the log
// went to io.Discard, so this assertion would have passed without looking at
// anything — which is the failure mode worth naming, not the leak.
func TestNoBearerReachesALogLine(t *testing.T) {
	f := newFixture(t)
	id, bearer := decidedTask(t, f)

	f.call(t, routeGrant, runner(theAgent, theRunID),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, nil)
	f.call(t, routeGrant, anotherRunner(),
		&tasksv1.GetTaskGrantRequest{TaskId: proto.String(id)}, nil)

	logs := f.Logs()
	if logs == "" {
		t.Fatal("nothing was logged at all, so this test proves nothing; the fixture " +
			"has to capture the service's log for it to mean anything")
	}
	if strings.Contains(logs, bearer) {
		t.Error("the approval appears in the log by value")
	}
	for i, segment := range strings.Split(bearer, ".") {
		if segment == "" {
			continue
		}
		if strings.Contains(logs, segment) {
			t.Errorf("segment %d of the approval appears in the log; a truncated "+
				"credential is still a credential", i)
		}
	}
	// The positive control: the captured log really covers the calls under test,
	// so the absence above is an absence and not an empty buffer. The task id is
	// what this path writes down, and so is the approval's IDENTIFIER — the one
	// thing about a grant a line here may say, and the line ReadGrant writes when
	// it hands one over.
	if !strings.Contains(logs, id) {
		t.Errorf("no line names task %s, so the captured log is not the log of the "+
			"calls this test made", id)
	}
	if !strings.Contains(logs, "grn_01test") {
		t.Error("no line names the approval's identifier, so the successful read " +
			"either did not happen or was not logged — and then this test is not " +
			"looking at the log of the call that carries a bearer")
	}
}

// ------------------------------------------------- the run comes from the request

// A runner names the run in the REQUEST, and the invocation is not consulted.
//
// This is the whole of why create_task had never once succeeded in the platform.
// `Create` required a run id and read it from `attribution.run_id`, which garmd
// NEVER SETS — it builds an invocation with a tenant and a correlation id, and
// reads an incoming attribution for the correlation id alone. So every call that
// arrived through the daemon named no run and was refused, and the queue Studio
// pages was empty because nothing could write to it.
//
// The caller here carries no run on its invocation, which is every caller
// arriving through garmd. The task is opened anyway, and the run it records is
// the one the request named.
//
// The fix is not to teach garmd about runs: knowing nothing about agents or runs
// is one of that daemon's invariants, and a daemon populating a run id is run
// semantics in the one component that must not have any. The runner is the only
// party that knows which run it is executing, so the runner declares it.
func TestCreateTakesTheRunFromTheRequestAndNotTheInvocation(t *testing.T) {
	f := newFixture(t)

	noRunOnTheCall := runner(theAgent, "")
	var res tasksv1.CreateTaskResponse
	f.ok(t, routeCreate, noRunOnTheCall, createRequest(), &res)
	if res.GetTaskId() == "" {
		t.Fatal("create_task answered no task id for a caller whose invocation names " +
			"no run — which is every caller arriving through garmd")
	}

	var got tasksv1.Task
	f.ok(t, routeGet, person(theApprove),
		&cardv1.TaskRef{TaskId: proto.String(res.GetTaskId())}, &got)
	if got.GetRunId() != theRunID {
		t.Errorf("run = %q, want the request's %q", got.GetRunId(), theRunID)
	}
}

// No run is a refusal, and the refusal names the FIELD. A task whose decision can
// reach nothing is a person asked a question for no reason.
//
// It names the field rather than the invocation because that is where a caller
// can do something about it. The old message — "the call names no run to
// signal" — sent whoever read it looking at a header that was never going to
// carry one.
func TestCreateRefusesARequestThatNamesNoRun(t *testing.T) {
	f := newFixture(t)
	req := createRequest()
	req.RunId = nil

	code, why := f.call(t, routeCreate, runner(theAgent, theRunID), req, nil)
	if code == "" {
		t.Fatal("a task was opened with no run; nothing can be told when it is decided")
	}
	if !strings.Contains(why, "run_id") {
		t.Errorf("refusal is %q, want it to name the run_id field — the caller can fix "+
			"a field, and cannot fix an invocation garmd builds", why)
	}
	// And not by falling back to the invocation, which DOES name a run here.
	if strings.Contains(why, "invocation") {
		t.Errorf("refusal is %q; the invocation is not where the run comes from any more", why)
	}
}

// The subject the decided event lands on is built from the run the REQUEST named.
//
// This is the point of the field rather than a detail of it: the run id keys
// `garm.tasks.v1.decided.<tenant>.<run_id>`, which is the one thing it is used
// for. It is an ASSERTION — nothing attests it — and that is acceptable precisely
// because routing decides who HEARS a decision and never who MAY act on one. The
// worst a runner can do by naming somebody else's run is wake it spuriously; the
// approval is handed back on the SERVICE that opened the task, so a different
// service that names this run is refused it. Within one service that is the limit
// `mayReadGrant` records rather than solves, and a spurious wake is still noise
// rather than privilege.
func TestTheDecidedSubjectIsBuiltFromTheRunTheRequestNamed(t *testing.T) {
	f := newFixture(t)

	const itsOwnRun = "run_01theRequestsOwn"
	req := createRequest()
	req.RunId = proto.String(itsOwnRun)

	var res tasksv1.CreateTaskResponse
	// No run on the invocation, so nothing but the request could be the source.
	f.ok(t, routeCreate, runner(theAgent, ""), req, &res)
	id := res.GetTaskId()

	f.ok(t, routeClaim, person(theApprove), &cardv1.TaskRef{TaskId: proto.String(id)}, nil)
	f.ok(t, routeDecide, person(theApprove), &tasksv1.DecideTaskRequest{
		TaskId: proto.String(id), Decision: tasksv1.Decision_APPROVE.Enum(),
		Reason: proto.String("the invoice matches"),
		Grant:  proto.String(f.grantFor(t, id, theMaterial(), nil)),
	}, nil)

	signals := f.Signals()
	if len(signals) != 1 {
		t.Fatalf("the run was told %d times", len(signals))
	}
	if signals[0].RunID != itsOwnRun {
		t.Fatalf("the decision was addressed to run %q, want %q", signals[0].RunID, itsOwnRun)
	}
	want := signal.Subject(theTenant, itsOwnRun)
	if got := signal.Subject(signals[0].Tenant, signals[0].RunID); got != want {
		t.Errorf("the decided subject is %q, want %q", got, want)
	}
	if !strings.Contains(want, "run_01theRequestsOwn") {
		t.Fatalf("the subject %q does not carry the run at all, so this test proves "+
			"nothing about where the run came from", want)
	}
}
