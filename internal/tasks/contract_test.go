package tasks_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/wire"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
	"github.com/garm-ai/tasksd/internal/tasks"
)

// The nine tools the contract declares, with the route each is reachable
// on. This is the table a deployment's catalogue has to agree with, and it
// is read off the contract rather than written down twice — so a contract
// that renames a tool fails here rather than in somebody's acceptance test.
func TestTheContractDeclaresNineToolsOnTheirOwnRoutes(t *testing.T) {
	want := map[string]string{
		"garm.tasks.v1.create_task":    "/garm.tasks.v1.TasksService/CreateTask",
		"garm.tasks.v1.get_task_grant": "/garm.tasks.v1.TasksService/GetTaskGrant",
		"garm.tasks.v1.list_tasks":     "/garm.tasks.v1.TasksService/ListTasks",
		"garm.tasks.v1.get_task":       "/garm.tasks.v1.TasksService/GetTask",
		"garm.tasks.v1.approval_card":  "/garm.tasks.v1.TasksService/ApprovalCard",
		"garm.tasks.v1.claim_task":     "/garm.tasks.v1.TasksService/ClaimTask",
		"garm.tasks.v1.release_task":   "/garm.tasks.v1.TasksService/ReleaseTask",
		"garm.tasks.v1.decide_task":    "/garm.tasks.v1.TasksService/DecideTask",
		"garm.tasks.v1.triage_task":    "/garm.tasks.v1.TasksService/TriageTask",
	}
	got, err := tasks.Contract()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the contract declares %d tools, want %d", len(got), len(want))
	}
	for _, tool := range got {
		route, known := want[tool.FQN]
		if !known {
			t.Errorf("unexpected tool %q", tool.FQN)
			continue
		}
		if tool.FullMethod != route {
			t.Errorf("%s is on %q, want %q", tool.FQN, tool.FullMethod, route)
		}
		// The subject both ends derive from the same function, so neither
		// keeps a routing table that could drift from the other's.
		if wire.Subject(tool.FullMethod) == "" {
			t.Errorf("%s resolves to no subject", tool.FQN)
		}
	}
}

// Every method declares a tool set, and `escalation` holds the two halves of one
// act and nothing else.
//
// Both halves are rules about the daemon rather than style. A tool in NO set is
// reachable only by a caller in NO set — garmd's `inScope` refuses a scoped
// caller every tool that shares none of its sets — so an unscoped method is not
// a lenient one, it is the one every scoped role is refused. That has gone wrong
// twice here: decide_task and approval_card were once unreachable by every staff
// role in the bank example, and create_task was unreachable by any runner whose
// principal named a set, which is why nothing in the platform had ever opened a
// task on this service.
//
// The second half is the least-privilege half, and it is the one that would rot
// quietly. The principal holding `escalation` is the whole platform's runner, so
// a method added to this set later is reach handed to every run it executes.
// Membership is therefore pinned by name and not left to review. It holds
// create_task and get_task_grant because opening an escalation and collecting the
// grant its decision minted are two halves of ONE act — which is what naming a
// set for the act rather than for the caller buys, and why that pairing cost no
// claims policy a line.
func TestEveryMethodDeclaresASetAndEscalationHoldsOpeningAndCollecting(t *testing.T) {
	svc := tasksv1.File_garm_tasks_v1_tasks_proto.Services().Get(0)

	var escalation []string
	for i := 0; i < svc.Methods().Len(); i++ {
		md := svc.Methods().Get(i)
		pol, _ := proto.GetExtension(md.Options(), toolv1.E_Tool).(*toolv1.ToolPolicy)
		if pol == nil {
			t.Errorf("%s carries no tool annotation at all", md.Name())
			continue
		}
		if len(pol.GetSets()) == 0 {
			t.Errorf("%s declares no tool set, so garmd refuses it to every caller "+
				"that names one — which is every role in the bank example but the "+
				"customer's", md.Name())
			continue
		}
		if slices.Contains(pol.GetSets(), "escalation") {
			escalation = append(escalation, pol.GetName())
		}
	}
	slices.Sort(escalation)

	if want := []string{"create_task", "get_task_grant"}; !slices.Equal(escalation, want) {
		t.Errorf("`escalation` holds %v, want %v — holding this set must let a "+
			"runner park a run on a decision and pick that decision up, and nothing "+
			"else", escalation, want)
	}

	// Declared as well as used. A catalogue carries the file's declarations, and
	// a tool naming a set the file never declared names something that matches
	// no caller — which costs that tool every scoped caller and says nothing.
	decl, _ := proto.GetExtension(svc.ParentFile().Options(), toolv1.E_ToolSets).(*toolv1.DeclSet)
	for _, want := range []string{"triage", "escalation"} {
		found := false
		for _, d := range decl.GetDeclared() {
			if d.GetName() == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the file declares no tool set named %q", want)
		}
	}
}

// get_task_grant is declared on the four axes garmd gates on, each pinned with
// the reason it is that value and not a wider one. It is the identical assertion
// `github.com/garm-ai/contracts` makes on its own copy of this package.
//
// The predicate is an AND over all four — `p.Verbs.Has(t.Verb) &&
// policy.Allows(p.Clearance, t.MinClearance) && p.Compartments.Covers(need) &&
// inScope(p.ToolSets, t.Sets)` — with no implication between verbs: a principal
// holding WRITE does not thereby hold READ, and one holding READ reaches every
// READ tool its clearance, compartments and SETS admit. That last clause is why
// this method may honestly declare READ: the runner is scoped to `tool_sets:
// [escalation]`, which holds create_task and this method and nothing else, so the
// READ it is granted in `sts/deploy/claims.yaml` and
// `examples/bank/auth/claims.yaml` reaches one more tool and not a class of them.
//
// An earlier revision declared VERB_WRITE to avoid touching a policy at all. That
// bought a method whose verb described the wrong act and contradicted its own
// `effects.idempotent`, and it set the precedent that a policy too narrow is
// fixed by relabelling the tool.
//
// It pins the audience and the audit for the same reason the service refuses to
// log a bearer: AUDIENCE_RUNNER is why no person's client and no model ever lists
// a credential read, and the two `record_` flags are why neither the bearer this
// returns nor the capability its request will carry reaches a ledger row by
// value.
func TestTheGrantReadBackIsDeclaredWhereTheRunnerAlreadyReaches(t *testing.T) {
	svc := tasksv1.File_garm_tasks_v1_tasks_proto.Services().Get(0)
	md := svc.Methods().ByName("GetTaskGrant")
	if md == nil {
		t.Fatal("TasksService declares no GetTaskGrant; the decided event carries a " +
			"reference and never the grant, so a woken run has no way to read the " +
			"approval it was parked on")
	}
	pol, _ := proto.GetExtension(md.Options(), toolv1.E_Tool).(*toolv1.ToolPolicy)
	if pol == nil {
		t.Fatal("GetTaskGrant carries no tool annotation at all")
	}

	if pol.GetVerb() != toolv1.Verb_VERB_READ {
		t.Errorf("verb = %s, want VERB_READ. The call returns a value and moves "+
			"nothing, which is also what effects.idempotent says, and a verb is a "+
			"description of the act before it is a policy axis — this vocabulary is "+
			"garm's own, so it is worth what each declaration keeps it worth. The "+
			"runner is granted READ for this; the SET is what bounds that, not the "+
			"verb", pol.GetVerb())
	}
	if pol.GetMinClearance() != toolv1.Clearance_CLEARANCE_PUBLIC {
		t.Errorf("min_clearance = %s, want CLEARANCE_PUBLIC; the runner's principal "+
			"is PUBLIC", pol.GetMinClearance())
	}
	if len(pol.GetCompartments()) != 0 {
		t.Errorf("compartments = %v, want none; the runner's principal holds none",
			pol.GetCompartments())
	}
	if want := []toolv1.Audience{toolv1.Audience_AUDIENCE_RUNNER}; !slices.Equal(pol.GetAudience(), want) {
		t.Errorf("audience = %v, want %v; a person never calls this and no model is "+
			"ever offered it", pol.GetAudience(), want)
	}
	if pol.GetAudit().GetRecordResponse() {
		t.Error("audit.record_response is true: the response carries the bearer, so a " +
			"ledger row would hold the credential by value")
	}
	if pol.GetAudit().GetRecordRequest() {
		t.Error("audit.record_request is true: the request is where a single-task " +
			"capability lands, and a ledger row holding one would be the same leak")
	}
}

// No message this service ANSWERS WITH carries a grant, except the one message
// whose whole purpose is to.
//
// The ruling "not a field on `Task`" made mechanical. `Task` is read by Studio
// and by people, and a field policy would not fix a bearer in it — the
// credential would still be in the message, and its omission would depend on
// every reader evaluating policy correctly. Requests are deliberately not walked:
// `DecideTaskRequest.grant` is the approval travelling INTO this service from the
// deciding person's client, which is the direction that has to work.
func TestNoProjectionAnswersWithAGrant(t *testing.T) {
	svc := tasksv1.File_garm_tasks_v1_tasks_proto.Services().Get(0)
	const allowed = "garm.tasks.v1.TaskGrant"

	seen := map[protoreflect.FullName]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		for i := 0; i < md.Fields().Len(); i++ {
			f := md.Fields().Get(i)
			if f.Name() == "grant" && string(md.FullName()) != allowed {
				t.Errorf("%s.%s: a projection carries a bearer. The grant is read back "+
					"through get_task_grant, whose answer is %s and which nothing but "+
					"the runner's set reaches.", md.FullName(), f.Name(), allowed)
			}
			if f.Kind() == protoreflect.MessageKind || f.Kind() == protoreflect.GroupKind {
				walk(f.Message())
			}
		}
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		walk(svc.Methods().Get(i).Output())
	}
}

// wireShape is the value this service advertises, written down.
//
// A golden constant rather than only a stability check, and the reason is the
// move that brought proto/ into this repository. `garm catalogue build`
// computes this same hash from its own copy of the descriptors, and garmd
// refuses to route to a service whose value differs from the catalogue's — so
// the two are one number agreed between two repositories, and the only thing
// that can keep them agreeing is writing it down on both sides.
//
// It held `contracts` v0.5.0's value through the move of proto/ into this
// repository, because `go_package` is an option and DescriptorHash reads no
// option -- which is what lets the package exist in two modules during the
// switchover without quarantining a deployment. See KNOWN-GAPS.md.
//
// IT MOVED ON 2026-10-01, and this is the real answer the comment below
// anticipated rather than a broken test: `CreateTaskRequest` gained
// `requester` (field 10), so the input descriptor of create_task has one more
// field and the hash follows. Every catalogue that carries garm.tasks.v1 has
// to be rebuilt, and `github.com/garm-ai/contracts` carries the IDENTICAL
// field at the identical number so both copies still hash the same -- which is
// the only thing keeping a deployment out of quarantine while two modules hold
// this package.
//
// IT DID NOT MOVE when create_task gained `sets: ["escalation"]` later the same
// day, and that is the stronger half of the claim above rather than a lucky
// coincidence: a tool set is an OPTION, and this hash walks only the input and
// output MESSAGE FIELDS of each method, emitting `message <FullName>` and
// `field <Number> <Name> <Cardinality> <Kind>`. So the ruling that gave the
// runner a stated reach cost no catalogue a rebuild. Verified by running this
// test either side of that change rather than reasoned about.
//
// IT MOVED AGAIN, the same day, and TWICE MORE IN ONE ROUND — which is the whole
// reason the two changes were batched: each move costs a catalogue rebuild and a
// command line tool release, and two in one release is one treadmill turn rather
// than two.
//
//  1. `TasksService` gained `GetTaskGrant`, so `garm.tasks.v1.decided` can carry a
//     reference and never a bearer and a woken run can still fetch the approval it
//     was parked on. `GetTaskGrantRequest` and `TaskGrant` are both new messages:
//     two messages and four fields. That took the value to `b80da142…3df2`.
//  2. `CreateTaskRequest` gained `run_id` (field 11), because the run has to come
//     from the request — garmd never puts one on an invocation and must not learn
//     how — and until it did, create_task was refused on every call that reached
//     this service through the daemon. One field, and the value below.
//
// What did NOT move it, in the same round, was VERB_WRITE becoming VERB_READ on
// get_task_grant. A verb is an option, and this hash emits none — confirmed by
// running this test either side of that change rather than assumed, which is the
// third time today that claim has been checked rather than remembered.
//
// EVERY CATALOGUE CARRYING garm.tasks.v1 MUST BE REBUILT by a command line tool
// linking contracts at these changes, and until it is, garmd quarantines this
// service on the mismatch.
//
// `github.com/garm-ai/contracts` carries the IDENTICAL method and messages, at
// identical numbers, so both copies still hash to this one value — which is the
// only thing keeping a deployment out of quarantine while two modules hold this
// package. It now pins the value in `garm/tasks/v1/wireshape_test.go` as well,
// so each copy guards its own; before that, only this constant did, and an edit
// in the other module moved the shape silently.
//
// If a change to the proto moves it again, that is a real answer and not a
// broken test: update this constant, update the other copy's to the same value,
// and expect every catalogue that carries garm.tasks.v1 to be rebuilt.
const wireShape = "8939ac00b2a441346826759b77d72dc568a9bb0fa32d50732cc72b3c166b5a63"

// The wire shape is what a daemon compares with the catalogue before it
// routes anything here. It has to be the agreed value, and it has to be
// stable across runs of the same build: a value that moved would quarantine
// the service for no reason.
func TestTheWireShapeIsStable(t *testing.T) {
	first, err := tasks.DescriptorHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("the wire shape is %q, want 64 hex characters", first)
	}
	if first != wireShape {
		t.Errorf("the wire shape is %q, want %q — see the constant's comment before changing it", first, wireShape)
	}
	second, err := tasks.DescriptorHash()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("two reads gave %q and %q", first, second)
	}
}
