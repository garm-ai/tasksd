# Track K — the tasks service

**Date:** 2026-09-29
**Status:** done — `go.mod` names `github.com/garm-ai/garm v0.17.0`, the
suite is green, `main` is pushed and tagged `v0.1.0`.

Spec: `spec/docs/superpowers/specs/2026-09-29-cards-and-tasks-as-tools-design.md`
— §2.5 (audience), §4 (the contract), §4.4 (per-method checks), §4.6 (the
signal), §4.7 (where it runs and its store), §7 (the approval binding), §8
(agents on tasks), §10.1 and §10.2 (call stacks), §11, §12.

## Where it landed

Three moves during the work, each from the controller:

1. In `agentd`'s process, with a copy of the proto — **withdrawn**: agentd
   holds no contracts. Nothing was committed; the previously untracked
   `proto/`, `buf.*` and `third_party/` were removed and agentd's tree is
   clean at `89a6d2b`.
2. In `garmd`, as `cmd/tasksd` beside the daemon — **withdrawn** before any
   file was written there. `garmd` is untouched.
3. **Its own repository**, `github.com/garm-ai/tasksd`, which is where it is.

## What is built

```
serve.go              package tasksd: Config and Serve
cmd/tasksd/main.go    flags, a JWKS file, and a call to Serve
internal/store/       Postgres: tasks, task_events, triage; numbered
                      migrations with a version table
internal/tasks/       the checks (service.go), the approval binding
                      (grant.go), the card (card.go), the contract's eight
                      methods (handlers.go), the registration (serve.go),
                      the contract read off the descriptor (contract.go)
internal/signal/      the JetStream publish that tells a waiting run
internal/ulid/        the task id
```

`Serve(ctx, Config) error` is the importable entry point the stack repository
asked for. The package is the repository root, `tasksd`; the type is
`tasksd.Config`. `cmd/tasksd/main.go` is flag parsing and one call to it, and
the wire-level test starts the service through `Serve` as well, so the path a
stack uses is the path that is covered. Cancelling the context drains — the
binary points `SIGTERM` and `SIGINT` at the same context.

`Config` carries `NATS`, `Postgres`, `GrantKeys` (the JWKS document),
`Concurrency`, `ClaimTTL`, `SweepEvery`, `Version`, `Log`, and one field the
brief did not ask for: `Signal tasks.Signaller`. Zero builds the JetStream
publisher over the connection; a process that runs the runner beside this one
can hand in its own, and the test hands in a recorder.

## The checks, per §4.4

| Method | What the service checks |
|---|---|
| `create_task` | principal is a USER, the chain names an agent, `attribution.run_id` is set, every material path passes `grant.ValidPath`, an approval names a tool and a non-empty predicate, an expiry is given; idempotent on (tenant, run, tool, digest) |
| `list_tasks` | tenant; QUEUE excludes what the caller's own run asked for; MINE is requested-by-me or claimed-by-me; DONE is terminal |
| `get_task`, `approval_card` | tenant; the requester may read the frame of their own task |
| `claim_task` | tenant; state OPEN; the caller is not the requester |
| `release_task` | tenant; the claimant, or anybody once the claim's window has passed |
| `decide_task` | tenant; **the chain carries no `act`** (403, logged `delegated_approver`); state CLAIMED; claimant is the caller; four eyes; APPROVE verifies the approval; DECLINE needs a reason; ANSWER is for a question and needs the message |
| `triage_task` | tenant; state OPEN or CLAIMED; a reason; RECOMMEND and COMMENT append; REASSIGN only to a stricter predicate; DECLINE is a decision, so four eyes holds, recorded under the agent with the person beside it, or under the person when there is no agent; never APPROVE |

## The approval

`garm` gained `contracts/grants` during this work — the shared verification
the controller said would move out of `garmd`'s `internal/`. It is an import
rather than a second implementation: `grants.Verify` checks the signature
against the configured key set, and the `Check*` methods hold the claims to
the task. `internal/tasks/grant.go` is the order of those checks and nothing
else.

What it covers: signature, no delegation chain, this tool, this subject, this
task id, a digest over the values **this service stored**, the approver is
the caller, the approver's clearance and compartments meet the task's
predicate, and the approval is neither expired nor older than the window the
task was opened with.

What it does not cover, stated in the file and in `KNOWN-GAPS.md`: single use
(the daemon holds the replay cache and checks all of this again when the
approved call is made) and the issuer and audience by name (the signature
binds the issuer, since the key set is one minter's).

## Tests

Store, against Postgres: migration versioning, opening a task with its first
trail entry, idempotency on a retried ask, a different value opening a
different task, another tenant's task not being found, one claim winning, a
lapsed claim being releasable by anybody, deciding once, a decision and its
trail entry being one write, triage changing nothing, reassignment, the queue
against what is mine, the expiry sweep, and a canary that walks every column
and the trail to check that no credential lands anywhere but `grant`.

Wire level, through `Serve`, over an embedded broker with the invocation
header the daemon sends: a runner opens a task and the trail names the agent;
a person cannot open one; a retry finds the same task; the queue shows it to
an approver and not to the requester, with the task's audience on the card;
the requester may read their own task and not claim it; claim then approve
records the decision, the approval's identifier and signals the run with the
approval; a decision through an agent is 403 and changes nothing; an approval
for another task, over other values, or unsigned is refused; a decision needs
the claim and only its holder decides; an agent declines with a reason, under
the agent, with no approval and a signal; a recommendation to approve changes
nothing; triage by a person is refused; a reassignment may only narrow;
the approval card carries the values with their paths and labels; a task is
decided once; a task of another tenant is not found.

## Ambiguities resolved, and how

**Triage by a person.** Resolved in favour of the contract, which declares
`triage_task` as `[AGENT, PERSON]`: a person reassigning or commenting on a
task nobody has got to is working on it, not deciding it. The actor recorded
is the chain's agent when there is one and the person otherwise, and a
decline through triage carries both. The two directions are covered — a
person may triage, and the same person's `decide_task` is refused the moment
their call arrives through an agent.

**No runner identity on the invocation.** §4.4 asks `create_task` to check
that `exec` is present. `garm.tool.v1.InvocationContext` has no such field, so
what is checked is the principal's kind, an agent in the chain, and a run id.
Named in `KNOWN-GAPS.md`.

**The tenant.** The design says the tenant is on the invocation and never a
request field, and §4.7 puts a `tenant` column on the row. Both: it is read
off the invocation and written to the row when the task is opened, because a
task outlives the call that opened it and nothing else here records it.

**Four eyes without an excluded-subject field.** `agentd`'s predicate carried
`exclude_subject`; `garm.tasks.v1.Predicate` does not. The requester is a
column, and every four-eyes check compares the caller with it — one place a
task records who asked for it.

**The wire shape this service advertises.** A daemon refuses to route to a
service whose advertised descriptor hash does not match the catalogue's, and
the function that computes it is `garm`'s `internal/compiler` — not
exported, and there is no generated binding for `garm.tasks.v1` because
`garm`'s contracts module cannot import `tool-go`. `internal/tasks/contract.go`
derives it from the linked descriptors with the same encoding, named as such,
with a test that pins its shape. **Recommendation: move
`compiler.DescriptorHash` to `garm/contracts` beside `grants`**, and this
becomes an import. A mismatch is loud rather than silent — the daemon
quarantines the package and says why.

**Endpoint registration by hand.** For the same reason: `Register` reads the
tool names and routes off the method annotations and mounts one endpoint per
method, so the table cannot drift from the contract. If Track G later ships a
generated binding, `internal/tasks/serve.go` collapses into a call to it.

**ANSWER tasks.** The contract's `ASK` kind and `Task.answer` are wired end to
end, but nothing validates the answer against the message type the asker
declared, because that needs the catalogue generation the task pinned and
this service loads none. Named in `KNOWN-GAPS.md`.

**The card.** Built here, labelled at the task's predicate on every element,
fact and on the card itself, with `Fact.field` set on each material value so
a client can rebuild the map for the target tool's own approval card. The ref
to the run carries `CardRef.tool_fqn`, filled with the agent the runner was
acting as — which meant storing that identity on the task, since the chain is
gone by the time a card is built. It is the identity that arrived on the
call, which is the closest thing this service is given to the agent's own
name; `KNOWN-GAPS.md` says so.

## Concerns

1. **The race detector cannot run over the wire-level suite, and that is
   somebody else's bug.** `tool-go` v0.5.0 runs a request's handler in a
   goroutine of its own, while `nats.go`'s micro package reads that
   request's error field after the handler returns, to count it in the
   endpoint's statistics. Every tool service that answers a coded refusal
   trips the detector, in those two libraries and nowhere near this
   repository — the report's two stacks are `micro.(*request).Error` and
   `micro.(*service).reqHandler`. `mise run test` runs the detector over
   this repository's own packages and the wire suite without it, with the
   reason written where the flag is. **It is worth a `tool-go` issue**: it
   affects `bankd` and `webd` too, and it hides real races in anybody's
   handler.
2. **`garm.tasks.v1` makes every scalar `optional`**, so the generated Go is
   pointer-valued and a handler that forgets a `proto.String` writes a nil.
   Reading is safe through the getters; writing is not, and there is no
   compiler help. Worth a look before the contract is frozen.
3. **The catalogue does not declare this contract yet.** Track E's. Until it
   does, nothing routes here through the daemon, and the advertised
   descriptor hash has nothing to be compared against.
4. **No end-to-end test against a real daemon.** The wire suite sends the
   invocation header the daemon sends, which is what a tool service can check
   on its own. The frames of §10.1 — Studio to the daemon to here to the
   token service and back — need `examples`' acceptance suite.
5. **The expiry sweep runs on every instance.** Idempotent, and cheap at this
   size; named in `KNOWN-GAPS.md` rather than solved with a leader election.
