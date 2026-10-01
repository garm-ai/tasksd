# tasksd — the task service

The queue behind garm's human approvals, served as a governed tool. A runner
opens a task, a person decides it, a run is told. It is a NATS micro service
with its own Postgres, reached through `garmd` like every other tool.

## The four rules

**1. Every rule comes from `Garm-Invocation`.** The subject, the tenant and the
delegation chain are what this service knows about a caller, and there is
nothing else: no token, no clearance, no compartments. A tool that could see a
clearance is a tool that would start filtering on one, and which tasks a viewer
may see is a label on the card that `garmd` projects.

**The run is NOT one of them, and reading it from there was a bug.**
`CallContext.run_id` is declared and `garmd` never sets it — the daemon builds an
invocation with a tenant and a correlation id, knowing nothing about agents or
runs being one of its invariants — so `create_task` was refused on every call that
arrived through the daemon until the run moved onto `CreateTaskRequest.run_id`.
That field is an assertion, used for routing a decision and never for authorizing
one; the proto's comment on it is the place to read why that is sound.
`Caller.RunID` remains and is still read by `get_task_grant`'s gate, where it is
part of why that gate refuses everything.

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

**4. This repository owns one contract and writes no other.**
`garm.tasks.v1` is `proto/garm/tasks/v1/tasks.proto`, generated into
`gen/garm/tasks/v1` by `mise run gen` and committed; the import path is
`github.com/garm-ai/tasksd/gen/garm/tasks/v1` and the Go package is still
`tasksv1`. Everything else it needs — `garm.tool.v1`, `garm.card.v1`,
`garm.meta.v1`, `wire`, `callctx`, `grant`, `grants` — comes from
`github.com/garm-ai/contracts`, and the protos of the first three are vendored
under `third_party/proto` so the contract's imports resolve. **Those are never
generated**: their Go already exists in that module, and a second copy
registering the same descriptor file paths panics in `protoregistry` at init.
There is no `replace` directive in `go.mod`.

Three things follow, and each is a check rather than a memory:

- `garm.tasks.v1` is published from this module **and still from
  `contracts`**, until the removal there lands. That is safe because nothing
  links both, and `mise run one-tasks-contract` fails the build if anything —
  a direct import or a transitive one — brings
  `github.com/garm-ai/contracts/garm/tasks/v1` back into either build graph.
- Nothing may import `github.com/garm-ai/garm`, the module the contracts left,
  for the same descriptor-duplication reason: `mise run no-old-contracts`
  matches the module path exactly so `garm-ai/garmd` is untouched.
- `gen/` cannot drift from `proto/`: `mise run gen-check` regenerates and
  compares, and `mise run breaking` asks `buf` whether the last release's
  consumers still hold. What this service **advertises** is read off `gen/`, so
  a stale `gen/` is a descriptor hash nobody can reproduce from the file.

All three, and the rest, run under `mise run ci`. A binary that links two
copies of a descriptor builds, vets, links and then dies before `main` — which
is why these are checks and not conventions: `go build` is green either way.

## Layout

```
proto/garm/tasks/v1/  the contract: nine tools, the messages, the policies
gen/garm/tasks/v1/    the Go it generates, committed; `mise run gen-check`
                      refuses a copy that has drifted from the proto
third_party/proto/    the annotations and the card, vendored so the contract's
                      imports resolve — in the buf workspace, never generated
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

**`mayReadGrant` is the gate on `get_task_grant`, and it refuses everything.**
That is the state of the feature, not a bug to fix in passing. Its third step is
an unconditional denial because the check the ruling asks for — a runner may read
the grant of the task its own run was parked on, and no other — rests on a run id
that `garmd` never puts on an invocation, and because an audience gates nothing.
Read the comment on that function before changing it: a capability `create_task`
mints is what replaces both halves, and `KNOWN-GAPS.md` carries the rest.

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
mise install    the toolchain — Go and buf
mise run pg     a throwaway Postgres, and the POSTGRES_DSN to export
mise run gen    buf generate: proto/ -> gen/
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
- `decisions/2026-09-30-the-tasks-contract-moves-to-tasksd.md` — why
  `garm.tasks.v1` is in `proto/` here rather than in the contract module, why
  the package name did not change with it, and the switchover this repository
  has already done its half of

**Do not create `docs/superpowers/` here.**

## This repository is public

No customer, deployment, tenant or internal hostname appears anywhere in it.
Fixtures use `example.com` shapes and generic subjects.
