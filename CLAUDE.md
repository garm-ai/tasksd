# tasksd — the task service

The queue behind garm's human approvals, served as a governed tool. A runner
opens a task, a person decides it, a run is told. It is a NATS micro service
with its own Postgres, reached through `garmd` like every other tool.

## The four rules

**1. Every rule comes from `Garm-Invocation`.** The subject, the tenant, the
delegation chain and the run id are what this service knows about a caller,
and there is nothing else: no token, no clearance, no compartments. A tool
that could see a clearance is a tool that would start filtering on one, and
which tasks a viewer may see is a label on the card that `garmd` projects.

**2. This service never says yes on a person's behalf.** It does not mint an
approval and it cannot: the approver's own client mints one against the
approver's own credentials, and what arrives here is that token. This service
checks it and records it. The three refusals that hold the rule — no minting
from a delegated identity, no `decide_task` with a delegation chain, no
verification of a delegated grant — live in three different processes so that
one of them being wrong is not the end of it.

**3. A queue can be worked without being closed with a yes.** `triage_task`
recommends, comments, reassigns to a stricter audience, or declines, and a
person may call it as well as an agent — reassigning a stale task is work,
not a decision. A recommendation to approve is recorded as what the caller
thinks and changes nothing. There is no argument to any method here that
lets an agent approve.

**4. No contract is written here.** `garm.tasks.v1` and `garm.card.v1` come
from `github.com/garm-ai/contracts`, generated there and imported here. There
is no `proto/` directory in this repository and there is no `replace`
directive in `go.mod`. The contracts used to live inside `garm` and moved out
into a module of their own; nothing may import `github.com/garm-ai/garm`
again, and not only for tidiness — both copies register the same descriptor
file paths, so a binary linking the two builds and then dies in
`protoregistry` at init. **That is enforced rather than remembered:**
`mise run no-old-contracts`, which `mise run ci` depends on, fails the build
when `go list -deps` names that module, matching the module path exactly so
`garm-ai/garmd` is untouched, and reading both the shipped graph and the
`-test` one because a test binary that panics in init is as dead as a
shipped one.

## Layout

```
serve.go              package tasksd: Config and Serve — everything the
                      binary does, as a function a development stack can call
cmd/tasksd/           the binary: flag parsing and a call to Serve
internal/
  store/              Postgres: tasks, task_events, triage, and the migrations
  tasks/              the service — the checks, the card, the handlers, the
                      registration against the contract
  signal/             publishing a decision to the run waiting on it
  ulid/               the task id
```

`Serve` is the tested entry point: the wire-level test starts the service
through it rather than assembling one by hand, so the path a stack uses is
the path that is covered.

## Where the checks are

`internal/tasks/service.go`. Every rule about who may do what is in that one
file, in a method per tool; `handlers.go` translates protobuf and nothing
else. A second transport would not be a second set of rules.

The one thing that is not there is the approval itself:
`internal/tasks/grant.go` holds it to the task, over
`github.com/garm-ai/contracts/grants` — the shared verification both this
service and the daemon use, so the binding is written once.

## Concurrency is instances, not goroutines

`tool-go` v0.6.0 runs a handler synchronously in the goroutine its
subscription owns, because `nats.go`'s `micro` reads a request's error field
the moment the handler returns. So `garmtool.WithConcurrency(n)` registers n
micro service instances in one queue group rather than sizing a pool, and
every instance is another responder on `$SRV.INFO`. `--concurrency` defaults
to `garmtool.DefaultConcurrency` — taken from the constant rather than
written down here, so the two cannot drift — and raising it is a decision
about the whole plane's discovery, not about this process. `Serve` passes its
logger in, so the effective configuration is one line at startup.

## Working here

```
mise install    the toolchain
mise run pg     a throwaway Postgres, and the POSTGRES_DSN to export
mise run test   go test ./... -race
mise run ci     what CI runs
```

The store and wire tests skip without `POSTGRES_DSN`. CI always sets it.

## The design record is not in this repository

It lives in **[`garm-ai/spec`](https://github.com/garm-ai/spec)** (private),
checked out beside this one at `../spec/docs/superpowers/`.

The ones that govern this repository:

- `specs/2026-09-29-cards-and-tasks-as-tools-design.md` — §4 is the contract,
  §4.4 the per-method checks, §4.6 the signal, §4.7 where it runs and its
  store, §7 the approval binding, §8 agents on tasks
- `specs/2026-09-28-approval-grants-design.md` — what an approval is
- `specs/2026-09-24-call-stack-design.md` — the ten steps a call goes through
  before it reaches here

**Do not create `docs/superpowers/` here.**

## This repository is public

No customer, deployment, tenant or internal hostname appears anywhere in it.
Fixtures use `example.com` shapes and generic subjects.
