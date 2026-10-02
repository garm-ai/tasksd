package tasksd_test

// The fixture stands the service up the way a deployment does — through
// Serve, on a real broker, with a real Postgres — and talks to it the way
// the daemon does: the protobuf body on the contract's own subject, the
// caller's assertions in the Garm-Invocation header, and a reply awaited.
//
// Over a broker rather than by calling a handler, because three of the
// things this service can get wrong are invisible from inside the process: a
// service name the broker refuses, a subject the daemon never publishes to,
// and a refusal the runtime turns into a 500. Only a caller publishing where
// the contract says to publish can tell.
//
// Through Serve rather than by assembling a service by hand, because Serve
// is what the binary and a development stack both call, and a test that
// wired its own would be covering an arrangement nobody runs.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/garm-ai/contracts/callctx"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
	"github.com/garm-ai/contracts/grant"
	"github.com/garm-ai/contracts/wire"

	"github.com/garm-ai/tasksd"
	"github.com/garm-ai/tasksd/internal/store"
	"github.com/garm-ai/tasksd/internal/tasks"
)

// The cast. Generic subjects and an example.com shape: this repository is
// public and names no deployment.
const (
	theTenant  = "example"
	theRunID   = "run_01hq"
	theAsker   = "user:asker@example.com"
	theAgent   = "agent:example.agents.v1.Assistant"
	theTriager = "agent:example.agents.v1.Triager"
	theApprove = "user:approver@example.com"
	// The runner's own identity. A task is opened BY this and FOR theAsker.
	theRunnerSvc = "svc:agentd"
	theTool      = "payments.v1.initiate_payment"
	theSubject   = "account:A-1"
)

// The routes, as the contract names them.
const (
	routeCreate  = "/garm.tasks.v1.TasksService/CreateTask"
	routeList    = "/garm.tasks.v1.TasksService/ListTasks"
	routeGet     = "/garm.tasks.v1.TasksService/GetTask"
	routeCard    = "/garm.tasks.v1.TasksService/ApprovalCard"
	routeClaim   = "/garm.tasks.v1.TasksService/ClaimTask"
	routeRelease = "/garm.tasks.v1.TasksService/ReleaseTask"
	routeDecide  = "/garm.tasks.v1.TasksService/DecideTask"
	routeTriage  = "/garm.tasks.v1.TasksService/TriageTask"
	routeGrant   = "/garm.tasks.v1.TasksService/GetTaskGrant"
)

type fixture struct {
	NC *nats.Conn

	mu      sync.Mutex
	signals []tasks.Signal

	// logs is everything the service wrote while a test ran. It is captured
	// rather than discarded because one assertion needs it: no bearer may reach
	// a log line, and a check against io.Discard would pass without ever
	// looking.
	logs *logBuffer

	key *ecdsa.PrivateKey
	kid string
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := store.TestDSN(t)

	f := &fixture{
		kid:  "test-key",
		logs: &logBuffer{},
		// The clock an approval is minted against. The service runs on the
		// real one, and an approval carries an issued-at and an expiry, so a
		// fixed instant would pass today and fail tomorrow.
		now: time.Now().UTC(),
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key

	url := embeddedNATS(t)
	f.NC = connect(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tasksd.Serve(ctx, tasksd.Config{
			NATS: url, Postgres: dsn, GrantKeys: f.jwks(t),
			Signal: tasks.SignalFunc(func(_ context.Context, s tasks.Signal) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.signals = append(f.signals, s)
				return nil
			}),
			// A sweep interval longer than any test, so expiry never runs
			// under a case that is not about it.
			SweepEvery: time.Hour,
			// DEBUG, so the assertion that nothing logs a bearer is made
			// against every line this service can write rather than the
			// subset a default level lets through.
			Log: slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Serve did not return after its context was cancelled")
		}
	})
	waitForSubject(t, f.NC, wire.Subject(routeCreate))
	waitForSubject(t, f.NC, wire.Subject(routeDecide))
	return f
}

// logBuffer collects log output from whatever goroutine wrote it. slog's
// handler serialises its own writes, but the test reads while the service is
// still running, so the buffer carries its own lock.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Logs is everything the service has written so far.
func (f *fixture) Logs() string { return f.logs.String() }

// Signals is what the runner would have been told, read under the lock: the
// handler runs on the service's own goroutine.
func (f *fixture) Signals() []tasks.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tasks.Signal(nil), f.signals...)
}

// embeddedNATS runs a server on a port the kernel picks, so parallel
// packages cannot collide and the suite does not pass or fail on what else
// is on the machine.
func embeddedNATS(t *testing.T) string {
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
	return srv.ClientURL()
}

func connect(t *testing.T, url string) *nats.Conn {
	t.Helper()
	// Above the two-second default: under -race on a loaded machine the
	// embedded server's accept can take longer, and the failure then reads
	// as a timeout against a server that is fine.
	nc, err := nats.Connect(url, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to the embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// waitForSubject polls the subject itself rather than sleeping: a fixed
// sleep either flakes on a loaded machine or is slow on every run. The probe
// carries no invocation context, so the runtime refuses it before any
// handler is reached — which is a reply, and a reply is all this waits for.
func waitForSubject(t *testing.T, nc *nats.Conn, subject string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := nc.Request(subject, nil, 250*time.Millisecond)
		if err == nil || !isNoResponder(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing ever answered on %s", subject)
		}
	}
}

func isNoResponder(err error) bool {
	return err == nats.ErrNoResponders || strings.Contains(err.Error(), "no responders")
}

// caller is who a request arrives as.
type caller struct {
	Subject string
	Kind    toolv1.PrincipalKind
	Act     []string
	RunID   string
	// Tenant is empty for everybody but the one case that is about the
	// boundary, and reads as the fixture's tenant then.
	Tenant string
}

// person is a person calling for themselves: no delegation chain, which is
// what decide_task requires.
func person(subject string) caller {
	return caller{Subject: subject, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_USER}
}

// runner is a runner executing a run as an agent. It is a SERVICE principal
// acting as itself — the person the run belongs to is named by the request's
// `requester`, not by this caller, because a runner is not that person.
//
// CAVEAT WORTH A COORDINATOR'S EYES: putting the agent on this caller's Act
// is the PRE-F21 shape — a SERVICE self-mint naming an actor at `act` at all
// is exactly what F21 fixed in sts (the agent's own tool_sets no longer
// reach `act` to be folded against). As of sts v0.6.1 a self-mint carries NO
// act entry whatsoever; the agent it named is spent on CanRun and never
// becomes one. This helper still sets one so every test built on it keeps
// asserting what it always asserted — that the task records which agent
// asked — without this task deciding, unilaterally, that those assertions
// are wrong. Task-18's own regression test, below, exercises the shape sts
// actually sends today (no Act, no Agent) and asserts create_task succeeds
// anyway, which is the part this helper's shape could not have caught.
func runner(agent, runID string) caller {
	return caller{
		Subject: theRunnerSvc, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE,
		Act: []string{agent}, RunID: runID,
	}
}

// runnerWithNoAgentAttribution is the shape sts actually mints today for a
// SERVICE self-mint (decisions/2026-10-02-the-agent-is-authorization-input-
// not-an-actor.md): no `act` entry, no agent anywhere in the token. Built
// from the real contracts primitives — toolv1.InvocationContext and
// callctx.Encode, not a replica of garmd's own Fold/Kind logic — so this is
// the minimal, verified-real envelope rather than a guess at what garmd
// does with a chain. See TestCreateTaskSucceedsWithNoAgentAttribution.
func runnerWithNoAgentAttribution(runID string) caller {
	return caller{Subject: theRunnerSvc, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_SERVICE, RunID: runID}
}

// invocation is the header the daemon sets on every hop.
func (f *fixture) invocation(t *testing.T, c caller) string {
	t.Helper()
	tenant := c.Tenant
	if tenant == "" {
		tenant = theTenant
	}
	ic := &toolv1.InvocationContext{
		Attribution: &toolv1.CallContext{
			Tenant: tenant, App: "garmd", RunId: c.RunID, CorrelationId: "corr_1",
		},
		Principal: &toolv1.InvocationPrincipal{Subject: c.Subject, Kind: c.Kind},
		// The daemon sends its ledger event id as the call id, and the
		// decoder refuses a context without one.
		CallId: "ev_call_1",
	}
	for _, a := range c.Act {
		ic.Act = append(ic.Act, &toolv1.Act{
			Subject: a, Kind: toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		})
	}
	// agent (contracts v0.12.0, field 12) is attribution, and garmd sources
	// it from Principal.Actor — the innermost chain entry — never from Act
	// itself. Mirrored here the same way: the LAST entry of c.Act when there
	// is one, same as every caller built in this file already means by
	// naming an actor. A direct caller (person, or a runner's self-mint with
	// no delegation at all) carries none, honestly — see runner()'s own
	// comment for why that is the real shape, not a gap in this fixture.
	if len(c.Act) > 0 {
		ic.Agent = c.Act[len(c.Act)-1]
	}
	enc, err := callctx.Encode(ic)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// call sends a request the way the daemon's transport does, and unmarshals
// the reply into out when there is one. It returns the error code the
// service answered with, empty on success.
func (f *fixture) call(t *testing.T, route string, c caller, req, out proto.Message) (string, string) {
	t.Helper()
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	msg := nats.NewMsg(wire.Subject(route))
	msg.Data = body
	msg.Header.Set(callctx.Header, f.invocation(t, c))

	reply, err := f.NC.RequestMsg(msg, 15*time.Second)
	if err != nil {
		t.Fatalf("requesting %s: %v", route, err)
	}
	if code := reply.Header.Get("Nats-Service-Error-Code"); code != "" {
		return code, reply.Header.Get("Nats-Service-Error")
	}
	if out != nil {
		if err := proto.Unmarshal(reply.Data, out); err != nil {
			t.Fatalf("decoding the answer of %s: %v", route, err)
		}
	}
	return "", ""
}

// ok is call for a request that must succeed.
func (f *fixture) ok(t *testing.T, route string, c caller, req, out proto.Message) {
	t.Helper()
	code, why := f.call(t, route, c, req, out)
	if code != "" {
		t.Fatalf("%s answered %s: %s", route, code, why)
	}
}

// ---------------------------------------------------------------- approvals

// jwks is the key set the service verifies approvals against.
func (f *fixture) jwks(t *testing.T) []byte {
	t.Helper()
	doc, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: f.key.Public(), KeyID: f.kid, Algorithm: string(jose.ES256), Use: "sig",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// grantFor mints the approval a person's own client would mint: signed by
// the key set above, naming the tool, the subject, this task and a digest
// over the values the service stored.
func (f *fixture) grantFor(t *testing.T, taskID string, material map[string]string, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": "https://tokens.example.com",
		"aud": "garm://garmd",
		"jti": "grn_01test",
		"iat": f.now.Unix(),
		"exp": f.now.Add(15 * time.Minute).Unix(),
		"garm_grant": map[string]any{
			"tool":                  theTool,
			"subject":               theSubject,
			"task":                  taskID,
			"material":              grant.Digest(material),
			"approver":              theApprove,
			"approver_clearance":    "RESTRICTED",
			"approver_compartments": []string{"payments"},
		},
	}
	if mutate != nil {
		mutate(claims)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", f.kid).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	token, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// unsigned is what a caller who never went to the token service can produce:
// the right shape, no signature anybody can check.
func unsigned(claims map[string]any) string {
	body, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(body) + "." + ""
}
