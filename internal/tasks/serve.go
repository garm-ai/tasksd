package tasks

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/garm-ai/contracts/wire"
	"github.com/garm-ai/tool-go/toolbind"

	cardv1 "github.com/garm-ai/contracts/garm/card/v1"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
)

// Register mounts every tool the contract declares on a runtime.
//
// One endpoint per method, on the route the daemon resolves from the same
// shared function it uses to publish — so neither side keeps a routing table
// that could drift from the other's. The contract version and the wire shape
// are advertised with them: a daemon refuses to route to a service whose
// shape does not match the catalogue it loaded, which is how a deployment
// learns that one of the two was built from a different contract.
func Register(r toolbind.Registrar, h Handlers, contractVersion string) error {
	tools, err := Contract()
	if err != nil {
		return err
	}
	hash, err := DescriptorHash()
	if err != nil {
		return err
	}

	handlers := map[string]struct {
		newRequest func() proto.Message
		handle     toolbind.Handler
	}{
		"CreateTask": {
			func() proto.Message { return new(tasksv1.CreateTaskRequest) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.CreateTask(ctx, m.(*tasksv1.CreateTaskRequest))
			}),
		},
		"GetTaskGrant": {
			func() proto.Message { return new(tasksv1.GetTaskGrantRequest) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.GetTaskGrant(ctx, m.(*tasksv1.GetTaskGrantRequest))
			}),
		},
		"ListTasks": {
			func() proto.Message { return new(tasksv1.ListTasksRequest) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.ListTasks(ctx, m.(*tasksv1.ListTasksRequest))
			}),
		},
		"GetTask": {
			func() proto.Message { return new(cardv1.TaskRef) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.GetTask(ctx, m.(*cardv1.TaskRef))
			}),
		},
		"ApprovalCard": {
			func() proto.Message { return new(cardv1.TaskRef) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.ApprovalCard(ctx, m.(*cardv1.TaskRef))
			}),
		},
		"ClaimTask": {
			func() proto.Message { return new(cardv1.TaskRef) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.ClaimTask(ctx, m.(*cardv1.TaskRef))
			}),
		},
		"ReleaseTask": {
			func() proto.Message { return new(cardv1.TaskRef) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.ReleaseTask(ctx, m.(*cardv1.TaskRef))
			}),
		},
		"DecideTask": {
			func() proto.Message { return new(tasksv1.DecideTaskRequest) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.DecideTask(ctx, m.(*tasksv1.DecideTaskRequest))
			}),
		},
		"TriageTask": {
			func() proto.Message { return new(tasksv1.TriageTaskRequest) },
			adapt(func(ctx context.Context, m proto.Message) (proto.Message, error) {
				return h.TriageTask(ctx, m.(*tasksv1.TriageTaskRequest))
			}),
		},
	}

	for _, t := range tools {
		e, ok := handlers[t.Method]
		if !ok {
			// A method in the contract with nothing behind it would mount
			// and answer nothing, which a caller discovers at the worst
			// moment. Refusing to start is the honest failure.
			return fmt.Errorf("tasks: the contract declares %s and this build implements no handler for it", t.Method)
		}
		if err := r.Endpoint(toolbind.ToolRef{
			FQN:     t.FQN,
			Subject: wire.Subject(t.FullMethod),
			Method:  t.Method,
			// The queue group replicas share, so the broker balances across
			// instances of this service and no other.
			Service:         ServiceName,
			ContractVersion: contractVersion,
			DescriptorHash:  hash,
		}, e.newRequest, e.handle); err != nil {
			return fmt.Errorf("tasks: registering %s: %w", t.FQN, err)
		}
	}
	return nil
}

// adapt turns a typed handler into the runtime's shape, and refuses a nil
// answer rather than publishing an empty message as a success.
func adapt(f func(context.Context, proto.Message) (proto.Message, error)) toolbind.Handler {
	return func(ctx context.Context, req proto.Message) (proto.Message, error) {
		res, err := f(ctx, req)
		if err != nil {
			return nil, err
		}
		if res == nil || isNil(res) {
			return nil, refuse(CodeBroke, "the task service answered nothing")
		}
		return res, nil
	}
}

// isNil catches a typed nil pointer returned as an interface, which a
// `return nil, nil` on a concrete type produces and a plain == nil misses.
func isNil(m proto.Message) bool { return !m.ProtoReflect().IsValid() }
