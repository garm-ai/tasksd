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

// The wire shape is what a daemon compares with the catalogue before it
// routes anything here. It has to be stable across runs of the same build:
// a value that moved would quarantine the service for no reason.
func TestTheWireShapeIsStable(t *testing.T) {
	first, err := tasks.DescriptorHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("the wire shape is %q, want 64 hex characters", first)
	}
	second, err := tasks.DescriptorHash()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("two reads gave %q and %q", first, second)
	}
}
