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
	theOpener   = "svc:agentd"
	anotherSvc  = "svc:somebody-else"
	theBearer   = "eyJhbGciOiJFUzI1NiJ9.a-signed-approval.sig"
	theGrantJTI = "grn_01test"
)

// approved is a task in the one state that has an approval to hand over, opened
// by the service that is allowed to collect it.
func approved() store.Task {
	return store.Task{
		ID: "tsk_01test", Tenant: "example", Kind: store.KindApproval,
		RunID: theRun, Requester: "user:asker@example.com", OpenedBy: theOpener,
		State: store.StateApproved, Decision: store.DecisionApprove,
		GrantJTI: theGrantJTI, Grant: theBearer,
	}
}

// aService is a runner calling as itself: a SERVICE principal with an agent on
// the delegation chain. Its subject is the one fact the gate reads, and the one
// fact a caller cannot choose — the token service derives it from the
// authenticated client credential — so a test that varies it is varying an
// attested value rather than a claim.
//
// It carries NO run id, which is every call that arrives through the daemon:
// garmd builds the invocation's attribution with a tenant and a correlation id
// and never a run. A gate that read one would refuse everything, and that is why
// this one does not read one.
func aService(subject string) Caller {
	return Caller{
		Subject: subject, Tenant: "example",
		Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE,
		Act: []Act{{
			Subject: "agent:example.agents.v1.Assistant",
			Kind:    toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		}},
		CallID: "ev_call_1",
	}
}

// aPerson is somebody at a keyboard calling for themselves. An approval is a
// bearer minted for a run to present, and a person has no run to present it on.
func aPerson(subject string) Caller {
	return Caller{
		Subject: subject, Tenant: "example",
		Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER, CallID: "ev_call_1",
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

// THE SUCCESS PATH. The service that opened the task collects the approval it
// was parked on.
//
// Nothing could reach this while the gate refused unconditionally, so this is
// the first test of the case the method exists for. What makes it pass is the
// caller's attested subject matching the service recorded on the row, and
// nothing else: the call names no run, because no call through the daemon does.
func TestTheOpeningServiceMayReadTheGrant(t *testing.T) {
	s := &Service{}
	if err := s.mayReadGrant(aService(theOpener), approved()); err != nil {
		code, why := codeOf(t, err)
		t.Fatalf("the service that opened the task was refused %s (%s); this is the "+
			"one caller the method exists for", code, why)
	}
}

// Another service is told what a caller of a task that never existed is told.
//
// That is the point of putting the identity first: the gate must not become a
// way to discover which task ids exist, or which of them are decided.
func TestAnotherServiceIsToldNoSuchTask(t *testing.T) {
	s := &Service{}
	code, why := codeOf(t, s.mayReadGrant(aService(anotherSvc), approved()))
	if code != CodeNoSuchTask {
		t.Fatalf("another service got %s (%s), want %s", code, why, CodeNoSuchTask)
	}
	if why != "no such task" {
		t.Errorf("refusal is %q, want the words a missing task gets, with nothing "+
			"about this one", why)
	}
	if strings.Contains(why, "APPROVED") || strings.Contains(why, "approval") {
		t.Errorf("refusal %q tells a stranger the state of a task they may not read", why)
	}

	// And the same for a task in a state that has its own refusal. This is what
	// pins the ORDER rather than the checks: the identity applied after the state
	// block would answer 409 here and tell a stranger that this task exists and
	// was declined.
	declined := approved()
	declined.State, declined.Decision = store.StateDeclined, store.DecisionDecline
	declined.Grant, declined.GrantJTI = "", ""
	code, why = codeOf(t, s.mayReadGrant(aService(anotherSvc), declined))
	if code != CodeNoSuchTask || why != "no such task" {
		t.Errorf("another service asking about a declined task got %s (%s), want %s / "+
			"no such task — the identity has to come before the state block",
			code, why, CodeNoSuchTask)
	}
}

// A person is told the same, and the subject is deliberately the opening
// service's own.
//
// So what refuses this is the KIND and not the name: `service:agentd` is a
// service principal, and a user principal that managed to present that subject
// is not the workload the grant was minted for. Pinning it with a matching
// subject is the only way this test fails if the kind check is dropped.
func TestAPersonIsToldNoSuchTask(t *testing.T) {
	s := &Service{}
	code, why := codeOf(t, s.mayReadGrant(aPerson(theOpener), approved()))
	if code != CodeNoSuchTask || why != "no such task" {
		t.Fatalf("a person got %s (%s), want %s / no such task", code, why, CodeNoSuchTask)
	}
}

// A task opened before `opened_by` existed records no service, and so can never
// hand its approval back.
//
// The column is additive, so every row written before the migration records no
// opener. It fails CLOSED rather than matching an empty subject: a task whose
// opener was never recorded is a task no caller can prove it opened. The run it
// belongs to has to be decided again, and an operator is told — the caller is
// told exactly what a stranger is told.
func TestATaskWithNoOpenerRecordedIsToldNoSuchTask(t *testing.T) {
	s := &Service{}
	x := approved()
	x.OpenedBy = ""
	code, why := codeOf(t, s.mayReadGrant(aService(theOpener), x))
	if code != CodeNoSuchTask || why != "no such task" {
		t.Fatalf("a task with no opener recorded answered %s (%s), want %s / no such task",
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
			code, why := codeOf(t, s.mayReadGrant(aService(theOpener), c.task()))
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

// An approved task with no approval recorded is this service's own broken
// invariant — Decide writes the state and the grant together — so the caller is
// told nothing about it and an operator is.
func TestAnApprovedTaskWithNoApprovalIsThisServicesFault(t *testing.T) {
	s := &Service{}
	x := approved()
	x.Grant = ""
	code, why := codeOf(t, s.mayReadGrant(aService(theOpener), x))
	if code != CodeBroke {
		t.Fatalf("an approved task with no grant answered %s (%s), want %s", code, why, CodeBroke)
	}
	if strings.Contains(why, theBearer) || strings.Contains(why, theGrantJTI) {
		t.Errorf("refusal %q says something about the approval it could not read", why)
	}
}
