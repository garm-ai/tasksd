package signal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/garm-ai/tasksd/internal/signal"
	"github.com/garm-ai/tasksd/internal/tasks"
)

// A real embedded broker, JetStream enabled, on an OS-chosen port. Whether a
// publish reaches a stream is the broker's behaviour, and a fake that agreed
// with our beliefs about it would prove nothing — which is exactly how this
// bug survived: every test of this package, and of the service around it,
// used a fake Signaller that never touched a stream at all.
func embedded(t *testing.T) *nats.Conn {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
		// Without this the embedded server sizes its account limit from the
		// temp filesystem, and stream creation fails as "insufficient
		// storage" rather than as anything this test is about.
		JetStreamMaxStore: 64 << 30,
	})
	if err != nil {
		t.Fatalf("building the embedded server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("the embedded server never became ready")
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to the embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// embeddedNoJetStream is a real, reachable broker with JetStream not
// enabled — standing in for "the fast path is unavailable" without needing
// to simulate a network partition: from EnsureStream's side, a request that
// gets no JetStream API responder looks the same either way, and neither is
// the stream-not-found case CreateStream is for.
func embeddedNoJetStream(t *testing.T) *nats.Conn {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("building the embedded server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("the embedded server never became ready")
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to the embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// capturingLog is what NewPublisher's "started anyway" log line is checked
// against: a real slog.Logger, so a level or an argument mistake shows up
// the same way it would over stderr.
func capturingLog(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// This is the bug, proven directly: before this change, nothing anywhere
// created GARM_TASK_DECISIONS, so this lookup would report ErrStreamNotFound
// against a live, JetStream-enabled broker.
func TestNewPublisherCreatesTheDecisionsStream(t *testing.T) {
	nc := embedded(t)
	if _, err := signal.NewPublisher(ctx5(t), nc, nil); err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := js.Stream(ctx5(t), signal.StreamName)
	if err != nil {
		t.Fatalf("the stream %s does not exist after NewPublisher: %v", signal.StreamName, err)
	}
	info, err := s.Info(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	wantSubject := signal.SubjectPrefix + ".>"
	found := false
	for _, subj := range info.Config.Subjects {
		if subj == wantSubject {
			found = true
		}
	}
	if !found {
		t.Errorf("stream %s has subjects %v, want one of them to be %q — a publish on "+
			"SubjectPrefix would land nowhere and nothing would report it",
			signal.StreamName, info.Config.Subjects, wantSubject)
	}
}

// Restarting this service, or starting two instances at once, must not fail
// because the stream is already there — and must not create it twice.
func TestNewPublisherIsIdempotentAcrossRestartsAndConcurrentStartup(t *testing.T) {
	nc := embedded(t)
	if _, err := signal.NewPublisher(ctx5(t), nc, nil); err != nil {
		t.Fatalf("first NewPublisher: %v", err)
	}
	// A second, independent connection: what a restarted process, or a
	// second instance starting at the same time, would do.
	nc2 := embedded(t)
	if _, err := signal.NewPublisher(ctx5(t), nc2, nil); err != nil {
		t.Fatalf("second NewPublisher against an already-provisioned stream: %v", err)
	}

	// EnsureStream directly, twice more in a row, on the same connection —
	// the restart case.
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	created1, err := signal.EnsureStream(ctx5(t), js)
	if err != nil {
		t.Fatalf("EnsureStream after the stream exists: %v", err)
	}
	if created1 {
		t.Error("EnsureStream reported Created on a stream that already existed")
	}
	created2, err := signal.EnsureStream(ctx5(t), js)
	if err != nil {
		t.Fatalf("EnsureStream a further time: %v", err)
	}
	if created2 {
		t.Error("EnsureStream reported Created a second time")
	}
}

// A configuration mismatch is reported, not silently patched — the same
// discipline sink's own stream provisioning follows, and for the same
// reason: a stream an operator changed on purpose should not have that
// change quietly reverted by the next restart.
func TestEnsureStreamRefusesAWrongConfigurationAndChangesNothing(t *testing.T) {
	nc := embedded(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// Create the stream ourselves with a subject list that has lost the
	// decisions subject — the dangerous, silent case: a publish to it would
	// still "succeed" and land nowhere.
	_, err = js.CreateStream(ctx5(t), jetstream.StreamConfig{
		Name:      signal.StreamName,
		Subjects:  []string{signal.SubjectPrefix + ".acme.>"},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = signal.EnsureStream(ctx5(t), js)
	if !errors.Is(err, signal.ErrStreamPolicyMismatch) {
		t.Fatalf("EnsureStream returned %v, want ErrStreamPolicyMismatch", err)
	}
	if !strings.Contains(err.Error(), "subjects") {
		t.Errorf("the refusal does not name the field that differs: %v", err)
	}

	s, err := js.Stream(ctx5(t), signal.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != signal.SubjectPrefix+".acme.>" {
		t.Errorf("EnsureStream changed the running stream's subjects to %v; it must only report",
			info.Config.Subjects)
	}
}

// The end-to-end proof: Signal, published through a Publisher built the way
// Serve builds one, is actually readable by a consumer bound to the stream —
// which is what a runner listening for its decision would be. A test that
// only checked Publish returned no error would not have caught this bug,
// because that is exactly what Publish did before the stream existed: NATS
// JetStream publish is a request-reply, and with no stream bound to the
// subject there was no responder, so the original code's error handling was
// exercised on every single decision in a running plane. This asserts the
// message is actually there, not merely that no error came back.
func TestASignalledDecisionReachesAConsumerOnTheStream(t *testing.T) {
	nc := embedded(t)
	pub, err := signal.NewPublisher(ctx5(t), nc, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	sig := tasks.Signal{
		Tenant: "acme", RunID: "run_1", TaskID: "task_1", Seq: 1,
		Decision: "approve", DecidedBy: "user:approver@example.com",
	}
	if err := pub.Signal(ctx5(t), sig); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx5(t), signal.StreamName, jetstream.ConsumerConfig{
		FilterSubject: signal.Subject(sig.Tenant, sig.RunID),
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("creating a consumer on %s: %v", signal.StreamName, err)
	}
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetching from the consumer: %v", err)
	}
	var got jetstream.Msg
	for msg := range batch.Messages() {
		got = msg
		if err := msg.Ack(); err != nil {
			t.Errorf("acking the message: %v", err)
		}
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("the batch ended in error: %v", err)
	}
	if got == nil {
		t.Fatal("the consumer received nothing: the decision never reached the stream")
	}
	wantSubject := signal.Subject(sig.Tenant, sig.RunID)
	if subj := got.Subject(); subj != wantSubject {
		t.Errorf("message landed on subject %q, want %q", subj, wantSubject)
	}
	var payload signal.Payload
	if err := json.Unmarshal(got.Data(), &payload); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	if payload.TaskID != sig.TaskID || payload.RunID != sig.RunID || payload.Decision != sig.Decision {
		t.Errorf("payload = %+v, want it to carry task %q, run %q, decision %q",
			payload, sig.TaskID, sig.RunID, sig.Decision)
	}
}

// The first of the two branches NewPublisher must keep apart: JetStream
// being unavailable must NOT take the service down. Before this, every
// EnsureStream failure was treated alike, which meant a stream outage
// stopped tasksd from starting at all — a worse failure than the signal
// merely arriving late, for a dependency every comment in jetstream.go calls
// the fast path only.
func TestNewPublisherStartsAnywayWhenJetStreamIsUnavailableAndLogsLoudly(t *testing.T) {
	nc := embeddedNoJetStream(t)
	log, buf := capturingLog(t)

	pub, err := signal.NewPublisher(ctx5(t), nc, log)
	if err != nil {
		t.Fatalf("NewPublisher returned an error with JetStream merely unavailable: %v — "+
			"this must start anyway and let signal_failed absorb it", err)
	}
	if pub == nil {
		t.Fatal("NewPublisher returned neither an error nor a publisher")
	}
	if !strings.Contains(buf.String(), signal.StreamName) || !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("no operator-visible ERROR line naming the stream; got:\n%s", buf.String())
	}
}

// The second branch: a genuine configuration mismatch is NOT the same kind
// of failure as an unreachable broker, and must still stop the service from
// starting — this is the deterministic, deploy-time case the missing stream
// already demonstrated once.
func TestNewPublisherRefusesToStartWhenTheStreamConfigurationHasDrifted(t *testing.T) {
	nc := embedded(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	_, err = js.CreateStream(ctx5(t), jetstream.StreamConfig{
		Name:      signal.StreamName,
		Subjects:  []string{signal.SubjectPrefix + ".acme.>"},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		Discard:   jetstream.DiscardOld,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = signal.NewPublisher(ctx5(t), nc, nil)
	if !errors.Is(err, signal.ErrStreamPolicyMismatch) {
		t.Fatalf("NewPublisher returned %v, want ErrStreamPolicyMismatch — a drifted "+
			"configuration must stop the service starting", err)
	}
}

// Concern from review: Diff only checked four of the policy fields, and
// MaxAge and Duplicates — the two knobs an operator is most likely to
// retune — were not among them. A change to either must be caught the same
// way a lost subject is.
func TestEnsureStreamDetectsAChangedMaxAge(t *testing.T) {
	nc := embedded(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signal.EnsureStream(ctx5(t), js); err != nil {
		t.Fatalf("provisioning the stream: %v", err)
	}

	s, err := js.Stream(ctx5(t), signal.StreamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	// What an operator does in response to "this window is too short":
	// widen MaxAge on the running stream directly, out from under this
	// package.
	drifted := info.Config
	drifted.MaxAge = 7 * 24 * time.Hour
	if _, err := js.UpdateStream(ctx5(t), drifted); err != nil {
		t.Fatalf("updating the stream out of band: %v", err)
	}

	_, err = signal.EnsureStream(ctx5(t), js)
	if !errors.Is(err, signal.ErrStreamPolicyMismatch) {
		t.Fatalf("EnsureStream returned %v after MaxAge changed underneath it, want ErrStreamPolicyMismatch — "+
			"a retention change an operator made is being silently left in force forever", err)
	}
	if !strings.Contains(err.Error(), "max_age") {
		t.Errorf("the refusal does not name max_age: %v", err)
	}
}
