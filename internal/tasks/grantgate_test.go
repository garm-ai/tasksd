package tasks

// The gate on get_task_grant, case by case.
//
// In the service's own package and with no database, because the gate is a
// function of a caller and a row and nothing else. The wire-level suite covers
// the same refusals through the broker, where a code becomes a header; this
// covers the ORDER they are applied in, which is the part a wire test cannot
// show and the part a leak would hide in.

import (
	"errors"
	"strings"
	"testing"

	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/tool-go/toolbind"

	"github.com/garm-ai/tasksd/internal/store"
)

const (
	theRun      = "run_01hq"
	anotherRun  = "run_01hz"
	theBearer   = "eyJhbGciOiJFUzI1NiJ9.a-signed-approval.sig"
	theGrantJTI = "grn_01test"
)

// approved is a task in the one state that has an approval to hand over.
func approved() store.Task {
	return store.Task{
		ID: "tsk_01test", Tenant: "example", Kind: store.KindApproval,
		RunID: theRun, Requester: "user:asker@example.com",
		State: store.StateApproved, Decision: store.DecisionApprove,
		GrantJTI: theGrantJTI, Grant: theBearer,
	}
}

// theRunner is the caller the method exists for: the runner, on the task's own
// run. Its RunID is set here by hand — garmd never sets one, which is the whole
// subject of the gate's third step.
func theRunner(runID string) Caller {
	return Caller{
		Subject: "svc:agentd", Tenant: "example",
		Kind:  toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE,
		Act:   []Act{{Subject: "agent:example.agents.v1.Assistant", Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT}},
		RunID: runID, CallID: "ev_call_1",
	}
}

func codeOf(t *testing.T, err error) (string, string) {
	t.Helper()
	if err == nil {
		return "", ""
	}
	var coded toolbind.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("refusal %v carries no code, so the runtime answers 500", err)
	}
	return coded.Code, coded.Message
}

// A caller on another run is told what a caller of a task that never existed is
// told. That is the point of the first step: the run gate must not become a way
// to discover which task ids exist, or which of them are decided.
func TestAnotherRunIsToldNoSuchTask(t *testing.T) {
	s := &Service{}
	code, why := codeOf(t, s.mayReadGrant(theRunner(anotherRun), approved()))
	if code != CodeNoSuchTask {
		t.Fatalf("another run got %s (%s), want %s", code, why, CodeNoSuchTask)
	}
	if why != "no such task" {
		t.Errorf("refusal is %q, want the words a missing task gets, with nothing "+
			"about this one", why)
	}
	if strings.Contains(why, "APPROVED") || strings.Contains(why, "approval") {
		t.Errorf("refusal %q tells a stranger the state of a task they may not read", why)
	}

	// And the same for a task in a state that has its own refusal. This is what
	// pins the ORDER rather than the checks: the run gate applied after the state
	// block would answer 409 here and tell a stranger that this task exists and
	// was declined.
	declined := approved()
	declined.State, declined.Decision = store.StateDeclined, store.DecisionDecline
	declined.Grant, declined.GrantJTI = "", ""
	code, why = codeOf(t, s.mayReadGrant(theRunner(anotherRun), declined))
	if code != CodeNoSuchTask || why != "no such task" {
		t.Errorf("another run asking about a declined task got %s (%s), want %s / "+
			"no such task — the run gate has to come before the state block",
			code, why, CodeNoSuchTask)
	}
}

// A call carrying no run at all is the case EVERY call through the daemon is
// today, and it is refused the same way: garmd builds the attribution with a
// tenant and a correlation id and never a run id.
func TestACallNamingNoRunIsToldNoSuchTask(t *testing.T) {
	s := &Service{}
	code, why := codeOf(t, s.mayReadGrant(theRunner(""), approved()))
	if code != CodeNoSuchTask || why != "no such task" {
		t.Fatalf("a call with no run got %s (%s), want %s / no such task",
			code, why, CodeNoSuchTask)
	}
}

// The states with no approval each refuse WITH A REASON, and never succeed
// emptily. A run handed an empty grant presents one to the daemon and is refused
// there, with nothing anybody can trace back to the decline that caused it.
func TestAStateWithNoApprovalRefusesWithAReason(t *testing.T) {
	cases := []struct {
		name  string
		task  func() store.Task
		code  string
		names string
	}{
		{"open", func() store.Task {
			x := approved()
			x.State, x.Decision, x.Grant, x.GrantJTI = store.StateOpen, "", "", ""
			return x
		}, CodeConflict, "not decided"},
		{"claimed", func() store.Task {
			x := approved()
			x.State, x.Decision, x.Grant, x.GrantJTI = store.StateClaimed, "", "", ""
			return x
		}, CodeConflict, "not decided"},
		{"declined", func() store.Task {
			x := approved()
			x.State, x.Decision, x.Grant, x.GrantJTI = store.StateDeclined, store.DecisionDecline, "", ""
			return x
		}, CodeConflict, "declined"},
		{"expired", func() store.Task {
			x := approved()
			x.State, x.Decision, x.Grant, x.GrantJTI = store.StateExpired, "", "", ""
			return x
		}, CodeConflict, "expired"},
		{"a question, answered", func() store.Task {
			x := approved()
			x.Kind, x.State, x.Decision = store.KindAsk, store.StateAnswered, store.DecisionAnswer
			x.Grant, x.GrantJTI = "", ""
			return x
		}, CodeRefused, "question"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Service{}
			code, why := codeOf(t, s.mayReadGrant(theRunner(theRun), c.task()))
			if code != c.code {
				t.Fatalf("%s answered %s (%s), want %s", c.name, code, why, c.code)
			}
			if !strings.Contains(why, c.names) {
				t.Errorf("refusal is %q, want it to name %q so a run's failure says why",
					why, c.names)
			}
		})
	}
}

// The method is UNREACHABLE BY DESIGN, and this is the test that says so.
//
// A caller on the task's own run, with an approved task and an approval recorded
// against it, is still refused — because possession of a capability for this task
// is what would authorise the read and nothing mints one yet. If this test starts
// failing because the gate let the call through, the capability either landed (and
// this test is now about the wrong thing) or the gate was removed, and the
// difference is the whole of the security property.
func TestTheGateRefusesEvenTheCallerItExistsFor(t *testing.T) {
	s := &Service{}
	code, why := codeOf(t, s.mayReadGrant(theRunner(theRun), approved()))
	if code != CodeDenied {
		t.Fatalf("the runner on its own run got %s (%s), want %s", code, why, CodeDenied)
	}
	if why != GrantGateUnimplementedMessage {
		t.Errorf("refusal is %q, want the message that says a capability is what is "+
			"missing rather than the caller's authority", why)
	}
	if strings.Contains(why, theBearer) {
		t.Error("the refusal quotes the approval it refused to hand over")
	}
}

// An approved task with no approval recorded is this service's own broken
// invariant — Decide writes the state and the grant together — so the caller is
// told nothing about it and an operator is.
func TestAnApprovedTaskWithNoApprovalIsThisServicesFault(t *testing.T) {
	s := &Service{}
	x := approved()
	x.Grant = ""
	code, why := codeOf(t, s.mayReadGrant(theRunner(theRun), x))
	if code != CodeBroke {
		t.Fatalf("an approved task with no grant answered %s (%s), want %s", code, why, CodeBroke)
	}
}
