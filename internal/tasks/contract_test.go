package tasks_test

import (
	"testing"

	"github.com/garm-ai/contracts/wire"

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

// wireShape is the value this service advertises, written down.
//
// A golden constant rather than only a stability check, and the reason is the
// move that brought proto/ into this repository. `garm catalogue build`
// computes this same hash from its own copy of the descriptors, and garmd
// refuses to route to a service whose value differs from the catalogue's — so
// the two are one number agreed between two repositories, and the only thing
// that can keep them agreeing is writing it down on both sides.
//
// This value is the one `github.com/garm-ai/contracts` v0.5.0 produced, before
// the proto moved here. It did not move with the proto, because `go_package`
// is an option and DescriptorHash reads no option; that is what lets the
// package exist in two modules during the switchover without quarantining a
// deployment. See KNOWN-GAPS.md.
//
// If a change to the proto moves it, that is a real answer and not a broken
// test: update this constant, and expect every catalogue that carries
// garm.tasks.v1 to be rebuilt.
const wireShape = "3a9113cd588ab3d2474ffdaaefb95bae7760dfbdcf72dcc88528b57b5101ff69"

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
