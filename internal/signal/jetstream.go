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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/garm-ai/tasksd/internal/tasks"
)

// SubjectPrefix is where decisions are published. A runner subscribes per
// tenant, or to everything, and filters on the run id it is waiting for.
const SubjectPrefix = "garm.tasks.v1.decided"

// StreamName is the JetStream stream every decision is published into.
//
// tasksd owns it: nothing else publishes on SubjectPrefix, so — the same
// reasoning sink's dead-letter stream follows for the one subject only it
// writes (see github.com/garm-ai/sink/internal/streams) — the stream's name
// and configuration are this repository's to choose rather than a shared
// contract's. `stack`'s compose.yaml already names it GARM_TASK_DECISIONS;
// this keeps that name rather than inventing a second one.
const StreamName = "GARM_TASK_DECISIONS"

// streamConfig is what EnsureStream creates or verifies.
//
// DiscardOld, like the ledger and unlike the audit stream: a decision is
// already durable in Postgres before it is ever published here, so this
// stream is the fast path only — a run that misses the message still wakes
// on its own durable sleep. A full stream should drop the oldest notification
// rather than make a publish (and so a decide_task call) fail; there is
// nothing here worth refusing a decision over.
//
// MaxAge is short for the same reason: a message a runner did not consume
// within the window is a message whose run has almost certainly already
// woken on its own, or is closer to its own timeout than this window — so
// nothing is served by holding it longer. Duplicates matches the window
// Signal's message id is meant to be recognised within: the task and trail
// position it was decided at, so a retried publish after an acknowledgement
// this service never saw is deduplicated by the broker rather than delivered
// twice.
func streamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        StreamName,
		Subjects:    []string{SubjectPrefix + ".>"},
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		Discard:     jetstream.DiscardOld,
		MaxAge:      24 * time.Hour,
		Duplicates:  2 * time.Minute,
		Description: "garm task decisions: the fast path to a run waiting on a decide_task; the store row is the durable copy",
	}
}

// ErrStreamPolicyMismatch is returned when the stream exists with a
// configuration EnsureStream would not have created.
var ErrStreamPolicyMismatch = errors.New("signal: stream exists with a different configuration")

// EnsureStream creates the decisions stream if it is missing, and otherwise
// verifies the one already there rather than patching it.
//
// It runs once, at startup, before any Signal call rather than being checked
// on every publish: a stream is provisioned once per deployment, and a
// lookup on every decision would be a network round trip paid by every
// decision instead of once at boot. Two instances of this service starting
// at once both take this path concurrently; that is safe because a NATS
// "create stream" request is idempotent for a configuration identical to
// what is already there — the server returns the existing stream rather
// than an error — so both observe success rather than one racing the other
// into a failure. A stream that exists with a DIFFERENT configuration is
// reported rather than silently patched, matching the discipline
// github.com/garm-ai/sink/internal/streams follows for its own two streams:
// an operator who edited it, possibly during an incident, should not have
// that decision quietly reverted by the next restart.
func EnsureStream(ctx context.Context, js jetstream.JetStream) (created bool, err error) {
	want := streamConfig()
	existing, err := js.Stream(ctx, want.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		if _, err := js.CreateStream(ctx, want); err != nil {
			return false, fmt.Errorf("signal: creating stream %s: %w", want.Name, err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("signal: looking up stream %s: %w", want.Name, err)
	}
	info, err := existing.Info(ctx)
	if err != nil {
		return false, fmt.Errorf("signal: reading info for stream %s: %w", want.Name, err)
	}
	if diffs := diffStreamConfig(want, info.Config); len(diffs) > 0 {
		return false, fmt.Errorf("%w: %s differs in %d field(s): %s",
			ErrStreamPolicyMismatch, want.Name, len(diffs), strings.Join(diffs, "; "))
	}
	return false, nil
}

// diffStreamConfig lists the policy fields on which a live stream disagrees
// with what EnsureStream would have created. A missing subject is the
// dangerous case: publishing to a subject no stream captures is not an error
// in NATS, so the publisher succeeds, the record never lands, and nothing
// reports it — which is exactly the failure mode this whole file exists to
// close.
func diffStreamConfig(want, got jetstream.StreamConfig) []string {
	var out []string
	cmp := func(field string, w, g any) {
		if fmt.Sprint(w) != fmt.Sprint(g) {
			out = append(out, fmt.Sprintf("%s: want %v, running %v", field, w, g))
		}
	}
	cmp("subjects", want.Subjects, got.Subjects)
	cmp("storage", want.Storage, got.Storage)
	cmp("retention", want.Retention, got.Retention)
	cmp("discard", want.Discard, got.Discard)
	return out
}

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

// NewPublisher builds a publisher over a connection, ensuring the stream
// Signal publishes into exists before returning — see EnsureStream. This is
// the startup path: Serve calls it once, before the service advertises
// itself, so a stream that cannot be created or that disagrees with what
// this build expects stops the service the same way a bad Postgres DSN or an
// empty grant key set does, rather than surfacing later as every decision
// silently failing to signal its run.
func NewPublisher(ctx context.Context, nc *nats.Conn) (*Publisher, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("signal: %w", err)
	}
	if _, err := EnsureStream(ctx, js); err != nil {
		return nil, err
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
