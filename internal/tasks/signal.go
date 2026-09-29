package tasks

import "context"

// Signal is what a waiting run is told when its task is decided.
//
// The grant travels; the answer does not. A runner that asked a question
// reads the answer back through get_task as the agent, so what the model
// sees is projected at the agent's reach and not at the approver's.
type Signal struct {
	Tenant string
	RunID  string
	TaskID string
	// Seq is the trail position the decision landed at. It makes the
	// message's identity stable, so a redelivery is recognised as one
	// rather than acted on twice.
	Seq       int
	Decision  string
	Reason    string
	Grant     string
	DecidedBy string
}

// Signaller delivers a decision to the run that is waiting for it.
//
// An interface because the delivery is a deployment's choice and this
// service's checks are not: the tests use a recorder, and a deployment
// publishes on the broker the runner is listening to.
type Signaller interface {
	Signal(ctx context.Context, s Signal) error
}

// SignalFunc adapts a function to the interface.
type SignalFunc func(ctx context.Context, s Signal) error

func (f SignalFunc) Signal(ctx context.Context, s Signal) error { return f(ctx, s) }
