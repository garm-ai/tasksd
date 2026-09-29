// Package tasksd runs the task service.
//
// It is the whole of what `tasksd serve` does, as a function, so a process
// that wants to run several garm components together for development can
// start this one the same way the binary does — with a context it cancels
// and a configuration it built, rather than flags and an environment.
// cmd/tasksd is flag parsing and a call to Serve.
package tasksd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/garm-ai/tool-go/garmtool"

	"github.com/garm-ai/tasksd/internal/signal"
	"github.com/garm-ai/tasksd/internal/store"
	"github.com/garm-ai/tasksd/internal/tasks"
)

// Config is everything the service needs to run. Nothing here is read from
// the environment: a caller that wants environment variables reads them and
// fills this in, which is what makes one process able to run two of these.
type Config struct {
	// NATS is the broker to serve on.
	NATS string
	// Postgres is the DSN of this service's own database. It is migrated at
	// startup and the service refuses to start if it cannot be reached.
	Postgres string

	// GrantKeys is the JWKS document the approvals presented to decide_task
	// are verified against — the key set of the token service that mints
	// them. Without it no approval can be checked, and the service refuses
	// to start rather than accept decisions it cannot verify.
	GrantKeys []byte

	// Concurrency bounds the calls in flight. Zero takes the runtime's own
	// default.
	Concurrency int
	// ClaimTTL is how long a claim holds before anybody may release it.
	// Zero takes the service's default.
	ClaimTTL time.Duration
	// SweepEvery is how often tasks that ran out of time are marked
	// expired. Zero takes a minute.
	SweepEvery time.Duration

	// Signal is where a decision is delivered. Zero publishes on the broker
	// this service is connected to, which is what a deployment wants; a
	// process that runs the runner beside this one can hand in its own.
	Signal tasks.Signaller

	// Version is what the service advertises to the broker. A caller that
	// leaves it empty gets a development version.
	Version string
	// Log is where this service writes. Zero is a text handler on stderr.
	Log *slog.Logger
}

// ContractVersion is the version of the garm contracts this build was
// generated against, advertised beside the wire shape so a daemon can tell
// which contract this process implements.
const ContractVersion = "v0.17.0"

// defaultSweep is how often expiry runs when the caller names no interval.
const defaultSweep = time.Minute

// Serve runs the service until ctx is cancelled, then drains.
//
// The order is fixed and every step before the first subscription is a
// refusal that changes nothing: the key set, the database and its
// migrations, the broker, then the endpoints. A service that came up without
// one of them would advertise itself and refuse every call, which reads as
// an outage rather than as the configuration mistake it is.
func Serve(ctx context.Context, cfg Config) error {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Postgres == "" {
		return errors.New("tasksd: --postgres is required: this service keeps the queue")
	}
	if len(cfg.GrantKeys) == 0 {
		return errors.New("tasksd: --grant-keys is required: an approval verified against nothing is an approval nobody checked")
	}
	keys, err := tasks.ParseGrantKeys(cfg.GrantKeys)
	if err != nil {
		return fmt.Errorf("tasksd: reading the approval key set: %w", err)
	}

	db, err := store.Open(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer db.Close()
	version, err := db.Migrate(ctx)
	if err != nil {
		return err
	}

	url := cfg.NATS
	if url == "" {
		url = nats.DefaultURL
	}
	// Reconnect forever rather than exit: a service that dies because the
	// broker blinked turns a transient outage into a deployment event.
	nc, err := nats.Connect(url,
		nats.Name("tasksd"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("disconnected from nats", "err", err)
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("tasksd: connecting to nats at %s: %w", url, err)
	}
	defer nc.Close()

	signaller := cfg.Signal
	if signaller == nil {
		publisher, err := signal.NewPublisher(nc)
		if err != nil {
			return err
		}
		signaller = publisher
	}
	svc := &tasks.Service{
		DB: db, Signal: signaller, Log: log, ClaimTTL: cfg.ClaimTTL, GrantKeys: keys,
	}

	opts := []garmtool.Option{}
	if cfg.Concurrency > 0 {
		opts = append(opts, garmtool.WithConcurrency(cfg.Concurrency))
	}
	runtime := garmtool.New("tasks", serviceVersion(cfg.Version), opts...)
	if err := tasks.Register(runtime, tasks.Handlers{Svc: svc}, ContractVersion); err != nil {
		return err
	}

	sweep := cfg.SweepEvery
	if sweep <= 0 {
		sweep = defaultSweep
	}
	go sweeper(ctx, svc, sweep, log)

	log.Info("serving", "service", tasks.ServiceName, "version", serviceVersion(cfg.Version),
		"contract", ContractVersion, "nats", nc.ConnectedUrl(), "schema", version)
	if err := runtime.Run(ctx, nc); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("drained")
	return nil
}

// sweeper marks what ran out of time, so the queue says so for a person
// looking at it and not only for the run that stopped waiting.
func sweeper(ctx context.Context, svc *tasks.Service, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := svc.Sweep(ctx)
			if err != nil && ctx.Err() == nil {
				log.Warn("expiring tasks", "err", err)
				continue
			}
			if n > 0 {
				log.Info("tasks expired", "count", n)
			}
		}
	}
}

func serviceVersion(v string) string {
	if v == "" {
		return "0.0.0-dev"
	}
	return v
}
