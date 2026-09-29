// Package signal publishes a decision to the run waiting for it.
//
// The message goes on the broker the runner already listens to, with an
// identity a redelivery can be recognised by. Nothing here waits for the
// runner: a decision is recorded before it is published, and a run that
// never hears is bounded by its own approval window.
package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/garm-ai/tasksd/internal/tasks"
)

// SubjectPrefix is where decisions are published. A runner subscribes per
// tenant, or to everything, and filters on the run id it is waiting for.
const SubjectPrefix = "garm.tasks.v1.decided"

// Subject is where one run's decision lands.
func Subject(tenant, runID string) string {
	return fmt.Sprintf("%s.%s.%s", SubjectPrefix, token(tenant), token(runID))
}

// token keeps a tenant or a run id from spanning subject tokens. Neither
// should ever contain a dot, a space or a wildcard; one that does would
// otherwise publish into a subject nobody meant.
func token(s string) string {
	r := strings.NewReplacer(".", "_", " ", "_", "*", "_", ">", "_")
	if s == "" {
		return "_"
	}
	return r.Replace(s)
}

// Payload is the message body.
type Payload struct {
	TaskID    string `json:"task_id"`
	RunID     string `json:"run_id"`
	Tenant    string `json:"tenant"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
	Grant     string `json:"grant,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`
}

// Publisher sends decisions over JetStream.
type Publisher struct{ js jetstream.JetStream }

// NewPublisher builds a publisher over a connection.
func NewPublisher(nc *nats.Conn) (*Publisher, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("signal: %w", err)
	}
	return &Publisher{js: js}, nil
}

// Signal publishes one decision.
//
// The message id is the task and the trail position it was decided at, so a
// publish retried after an acknowledgement this service never saw is
// recognised by the broker rather than delivered twice — and a run is never
// resumed twice on one decision.
func (p *Publisher) Signal(ctx context.Context, s tasks.Signal) error {
	body, err := json.Marshal(Payload{
		TaskID: s.TaskID, RunID: s.RunID, Tenant: s.Tenant,
		Decision: s.Decision, Reason: s.Reason, Grant: s.Grant, DecidedBy: s.DecidedBy,
	})
	if err != nil {
		return err
	}
	_, err = p.js.Publish(ctx, Subject(s.Tenant, s.RunID), body,
		jetstream.WithMsgID(fmt.Sprintf("%s:%d", s.TaskID, s.Seq)))
	if err != nil {
		return fmt.Errorf("signal: publishing the decision of task %s: %w", s.TaskID, err)
	}
	return nil
}
