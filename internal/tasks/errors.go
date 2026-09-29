package tasks

import (
	"errors"
	"fmt"

	"github.com/garm-ai/tool-go/toolbind"

	"github.com/garm-ai/tasksd/internal/store"
)

// The codes this service answers with. They are the transport's strings, not
// a taxonomy of this service's invention, and which one means what is the
// daemon's contract with its callers.
const (
	// CodeRefused is an argument that will never be acceptable.
	CodeRefused = "400"
	// CodeDenied is a caller who may reach the tool but may not do this:
	// approving your own request, approving through an agent.
	CodeDenied = "403"
	// CodeNoSuchTask is every refusal that would otherwise tell a caller
	// which task ids exist. A task of another tenant and a task that never
	// existed get the same answer.
	CodeNoSuchTask = "404"
	// CodeConflict is a row that moved: somebody claimed it first, somebody
	// decided it first.
	CodeConflict = "409"
	// CodeBroke is this service's own fault, and never carries the reason.
	CodeBroke = "500"
)

// DelegatedApproverMessage is what a delegated caller is told when they try
// to decide. It is the same rule the token service enforces at mint and the
// daemon enforces when it verifies a grant; this is the one place a person
// is told why, because the other two answer one opaque refusal to
// everything.
const DelegatedApproverMessage = "an approval cannot be given through an agent: " +
	"the call arrives with a delegation chain, and a grant is never minted for one"

// refuse builds the error shape the tool runtime reads a code off.
//
// By value, never by pointer: the runtime matches with errors.As against a
// toolbind.CodedError target, which finds a value bare or wrapped and never
// a pointer, so a handler returning one of those would have its code
// silently dropped and answer 500 where the contract says 404.
func refuse(code, message string) error {
	return toolbind.CodedError{Code: code, Message: message}
}

func refusef(code, format string, args ...any) error {
	return toolbind.CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// fromStore turns a store failure into an answer.
//
// A missing row is "no such task" whatever the reason it is missing, and a
// row in the wrong state is a conflict. Anything else is this service's
// fault and the caller is told nothing about it — the reason goes to the
// log, which is where an operator looks.
func (s *Service) fromStore(err error, what string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse(CodeNoSuchTask, "no such task")
	case errors.Is(err, store.ErrConflict):
		return refuse(CodeConflict, "the task moved: it was claimed or decided by somebody else")
	default:
		s.log().Error(what, "err", err)
		return refuse(CodeBroke, "the task store could not be reached")
	}
}
