// Command tasksd serves garm.tasks.v1 over NATS.
//
// It is flag parsing and a call to tasksd.Serve. Everything the service does
// is in that function, so a development process that runs several garm
// components together starts this one the same way this binary does.
//
// Nothing here authenticates, authorises, checks a caller's clearance or
// redacts an answer. The daemon did all of that before the call arrived.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/nats-io/nats.go"

	"github.com/garm-ai/tasksd"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("tasksd stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	url := flag.String("nats", nats.DefaultURL, "NATS server URL")
	postgres := flag.String("postgres", os.Getenv("POSTGRES_DSN"),
		"Postgres DSN for this service's own database (required)")
	grantKeys := flag.String("grant-keys", "",
		"Path to the JWKS of the service that mints approvals (required)")
	concurrency := flag.Int("concurrency", 16, "Calls in flight at once")
	claimTTL := flag.Duration("claim-ttl", 0,
		"How long a claim holds before anybody may release it (default 30m)")
	sweep := flag.Duration("sweep-every", 0,
		"How often tasks that ran out of time are marked expired (default 1m)")
	flag.Parse()

	if *concurrency <= 0 {
		return errors.New("--concurrency must be at least 1")
	}
	if *grantKeys == "" {
		return errors.New("--grant-keys is required: this service decides nothing it cannot verify")
	}
	keys, err := os.ReadFile(*grantKeys)
	if err != nil {
		return fmt.Errorf("reading --grant-keys: %w", err)
	}

	// The same context the service drains on, cancelled by an interrupt or a
	// SIGTERM: one shutdown path, whether it is a person pressing control-C
	// or an orchestrator replacing the pod.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return tasksd.Serve(ctx, tasksd.Config{
		NATS: *url, Postgres: *postgres, GrantKeys: keys,
		Concurrency: *concurrency, ClaimTTL: *claimTTL, SweepEvery: *sweep,
		Version: version(), Log: log,
	})
}

// version is the module version the toolchain stamped, which the daemon
// reads back from the broker's service info: v0.1.0 when installed with
// `go install github.com/garm-ai/tasksd/cmd/tasksd@v0.1.0`. An in-tree build
// is stamped "(devel)", which is not a version the broker accepts, so it is
// reported as a development build.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "0.0.0-dev"
}
