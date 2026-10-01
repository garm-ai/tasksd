package tasks_test

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/wire"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
	"github.com/garm-ai/tasksd/internal/tasks"
)

// The eight tools the contract declares, with the route each is reachable
// on. This is the table a deployment's catalogue has to agree with, and it
// is read off the contract rather than written down twice — so a contract
// that renames a tool fails here rather than in somebody's acceptance test.
func TestTheContractDeclaresEightToolsOnTheirOwnRoutes(t *testing.T) {
	want := map[string]string{
		"garm.tasks.v1.create_task":   "/garm.tasks.v1.TasksService/CreateTask",
		"garm.tasks.v1.list_tasks":    "/garm.tasks.v1.TasksService/ListTasks",
		"garm.tasks.v1.get_task":      "/garm.tasks.v1.TasksService/GetTask",
		"garm.tasks.v1.approval_card": "/garm.tasks.v1.TasksService/ApprovalCard",
		"garm.tasks.v1.claim_task":    "/garm.tasks.v1.TasksService/ClaimTask",
		"garm.tasks.v1.release_task":  "/garm.tasks.v1.TasksService/ReleaseTask",
		"garm.tasks.v1.decide_task":   "/garm.tasks.v1.TasksService/DecideTask",
		"garm.tasks.v1.triage_task":   "/garm.tasks.v1.TasksService/TriageTask",
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

// Every method declares a tool set, and `escalation` holds create_task alone.
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
// Membership is therefore pinned by name and not left to review.
func TestEveryMethodDeclaresASetAndEscalationHoldsCreateTaskAlone(t *testing.T) {
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

	if want := []string{"create_task"}; !slices.Equal(escalation, want) {
		t.Errorf("`escalation` holds %v, want %v — holding this set must let a "+
			"runner open a task and do nothing else", escalation, want)
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
// If a change to the proto moves it again, that is a real answer and not a
// broken test: update this constant, and expect every catalogue that carries
// garm.tasks.v1 to be rebuilt.
const wireShape = "6d981dae473be75988ff835b570a1df11f36704ac7bbbbaa7acd821ed9b83e50"

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
