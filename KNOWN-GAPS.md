# Known gaps

What is not built, what is half built, and what was left to another release.
Kept honest by the changes that close the entries.

## Not built yet

**Questions to a person (`ASK`) are accepted but not exercised.** A task of
kind `ASK` can be opened, answered through `decide_task` with the answer
message, and read back on `Task.answer`. Nothing validates the answer against
the message type the asker declared in `decision_type`, because that needs
the catalogue generation the task pinned and this service does not load one.
Until it does, the answer is stored as the bytes it arrived as.

**The answer is not sealed.** §4.7 asks for `answer bytea` sealed at rest.
The column exists and holds the message as it arrived. Sealing needs a key to
configure and a decision about where it comes from, and nothing puts anything
secret there yet.

**No paging beyond a cursor.** `list_tasks` returns up to a hundred cards and
a cursor. There is no count and no total; a queue nobody can read to the
bottom is already too long.

**Claim lapse is checked on a release, not swept.** A claim past its window
can be released by anybody, which is what unblocks the task. The row still
says `CLAIMED` until somebody does that or until the task expires.

## Mid-transition

**`garm.tasks.v1` exists in two modules, and that is deliberate.** The proto
is `proto/garm/tasks/v1/tasks.proto` here, taken from
`github.com/garm-ai/contracts` v0.5.0 — and it is still in that module too,
because removing it there is a separate step in a quiet tree. Somebody reading
this mid-transition should not be alarmed, and here is exactly why.

*Nothing links both.* The two copies register the same descriptor file path,
`garm/tasks/v1/tasks.proto`, and a process reaching both panics in
`protoregistry` during package init — it builds, vets, links and then does not
start. This repository imports its own copy and no longer imports the contract
module's, in either the shipped graph or the `-test` one, and
`mise run one-tasks-contract` is what keeps that true rather than remembered.
The module requirement stays, because `garm/tool/v1`, `garm/card/v1`,
`garm/meta/v1`, `wire`, `callctx`, `grant` and `grants` all still come from it
— so what is forbidden is one package under that module, not the module.

*The catalogue still matches.* A deployment's catalogue currently declares the
contract module's copy while this service serves its own. The two differ in
exactly one thing that reaches the descriptor, the `go_package` option, and
`DescriptorHash` — which garmd's `internal/serve/reconcile.go` compares at
mount and `internal/tasks/contract.go` computes here — hashes message full
names, field numbers, field names, cardinalities and kinds, and reads no
option. That was verified against garm's `internal/compiler/emit_micro.go`
rather than assumed, and the value is written down in
`internal/tasks/contract_test.go` so it cannot move unnoticed: it was
`3a9113cd…ff69` before the move and after it. So nothing quarantines.

The value is **no longer** that one, and it moved twice on 2026-10-01.
`requester` on `CreateTaskRequest` took it to `6d981dae…3e50` — a field, which the
hash does read — while `escalation` on `create_task` did **not** move it, because a
tool set is an option. Then one round moved it twice more, batched on purpose because each move costs a
catalogue rebuild and a command line tool release: `GetTaskGrant` took it to
`b80da142…3df2` — its request and its answer are both new messages, four fields
the walk did not cover — and `run_id` on `CreateTaskRequest` took it to
**`8939ac00b2a441346826759b77d72dc568a9bb0fa32d50732cc72b3c166b5a63`**, which is
the current value. `get_task_grant` going from `VERB_WRITE` to `VERB_READ` in the
same round moved it not at all, a verb being an option, confirmed either side
rather than assumed. **Every catalogue carrying `garm.tasks.v1` has to be rebuilt
by a command line tool linking contracts at these changes, and until it is, garmd
quarantines this service on the mismatch.**

Both repositories carry all of it identically, so the two copies still hash alike
and the overlap is still safe. That is now guarded from both ends: the contract
module pins the same constant in `garm/tasks/v1/wireshape_test.go`, computed from
its own descriptor. Before that only this repository's constant existed, so an
edit there could move the shape and nothing in that repository would say so.

*What is left.* Two steps, neither of them this repository's:

1. `garm.tasks.v1` comes out of `github.com/garm-ai/contracts` — the proto,
   the generated Go and the lint exemptions — and the CLI's blank import of it
   goes with it. That is when the duplication ends and
   `mise run one-tasks-contract` becomes a check about the past.
2. The bank example's `catalogue.yaml` names `module:
   github.com/garm-ai/tasksd` instead of `github.com/garm-ai/contracts` for
   that package. One line, and the recorded provenance is then the version
   that produced the bytes, which is the truthfulness bug the move closes.

Until (1), a consumer that wants the task tools may reach either copy and get
the same wire shape. After it, there is one.

## Where a check is thinner than the design

**The wire shape this service advertises is computed here, and owning the
proto did not change that.** A daemon compares what a service advertises with
what the catalogue declares, and refuses to route when they differ. The value
is derived from the contract by `internal/tasks/contract.go`, using the
encoding `garm catalogue build` stamps.

Owning the proto was the obvious candidate for closing this and it does not,
for two reasons worth writing down. The generator that would emit the constant,
`cmd/protoc-gen-garm-go`, lives in the command line tool's repository, so
adding it to `buf.gen.yaml` would buy a table this service already derives at
run time from the same annotations — at the price of a build-time dependency on
that repository. And the function itself, `descriptorHash` in garm's
`internal/compiler/emit_micro.go`, is unexported in another module's
`internal/` package, which no amount of owning a proto reaches.

So the answer is unchanged and the right home is unchanged: a hash that a
producer and a consumer must agree on belongs in the module they share,
`github.com/garm-ai/contracts`, beside `grants` — which both this service and
the daemon already import. v0.5.0 still exports nothing that supersedes it: the
module carries the descriptor-hash *field* on `garm.catalogue.v1.Catalogue` and
no function that computes one. This becomes an import when it lands there.

What did change is that the value is now pinned rather than only checked for
stability. `internal/tasks/contract_test.go` holds it as a constant, because
the number is agreed between two repositories and writing it down on both sides
is the only thing that can keep them agreeing. A mismatch is loud rather than
silent either way: the daemon refuses to route and says which package and why.

**`get_task_grant` works, and the gate is drawn at the service that opened the
task. This entry is what is left rather than why nothing works.**

The read back is no longer missing from the contract. `escalation` holds
`create_task` and `get_task_grant` — two halves of one act — and the method
declares `verb: VERB_READ`, `min_clearance: CLEARANCE_PUBLIC`, no compartment and
that one set.

**The verb cost the runner's policy a line, and that is the right way round.** An
earlier revision declared `VERB_WRITE` so that nothing outside these two
repositories had to change: garmd's visibility predicate is an AND over verb,
clearance, compartments and sets with no implication between verbs, and the runner
held `verbs: [WRITE]`. But the call returns a value and moves nothing — which its
own `effects.idempotent` already says — so WRITE put a write in the catalogue an
auditor reads where nothing is written, and made "the policy is too narrow" a
thing you fix by relabelling the tool. The runner is granted `READ` in
`sts/deploy/claims.yaml` and `examples/bank/auth/claims.yaml` instead, and **the
set bounds that and not the verb**: scoped to `escalation`, which holds these two
tools and nothing else, READ reaches one more method rather than every
`CLEARANCE_PUBLIC`, uncompartmented, `VERB_READ` tool in the mounted catalogue.

**What it gates on is the attested service identity.** `tasks.opened_by` records
the subject on the call that opened a task — `create_task` writes it, migration
`0002_opened_by.sql` adds it — and `mayReadGrant` requires a SERVICE principal
whose subject equals it. That subject is derived at the token service from the
authenticated client credential, so a caller cannot name a different service; with
a bound token, `cnf` holds the presenting workload to it as well.

**The run could not do that job, and the earlier gate over it is what refused
every call.** `Caller.RunID` comes from `InvocationContext.attribution.run_id` and
**garmd never writes that field**: it builds the attribution with a tenant and a
correlation id and nothing else (`internal/toolplane/core.go`,
`withInvocationContext`). The run on the row is no better — the runner asserted it
on `create_task` — so comparing the two would have been one unattested claim
checked against another. A run id routes a decision to whoever is listening, and
routing decides who hears, never who may.

**It cost no contract change.** `opened_by` is this service's own column and is on
no message, so the wire shape did not move and no catalogue has to be rebuilt for
it — the first change in this programme that escapes that treadmill. The
single-task capability that was being designed is dropped: it would have been a
second bearer to mint, store, return once and checkpoint, for a boundary the
platform already attests.

**The limit, stated rather than solved: within one runner, any run can read any
task's approval.** A compromised runner holds every approval it legitimately
fetches anyway, and a capability would have been checkpointed in that same
process's state — so it bought nothing against the same threat. The trust boundary
is the service. Per-run isolation needs per-run credentials, and that is the right
end state only for a runner shared across tenants.

**A task opened before `opened_by` existed can never hand its approval back.** The
column is additive and nothing can backfill it — the opener was never recorded,
and taking it from `agent` would invent the fact the gate checks — so such a row
carries the empty string, matches no caller, and is answered "no such task" with a
line in the log naming it. A run parked on one of those has to ask again. Nothing
in any deployment has opened a task on this service, so the set is expected to be
empty; it is written down because a dev database can hold one.

**The same emptiness used to break `create_task`, and that half is fixed.**
`Create` requires a run to have something to signal, and it read
`attribution.run_id` — so every `create_task` arriving through the daemon was
refused, which is why nothing in the platform had ever opened a task on this
service. It now takes the run from `CreateTaskRequest.run_id`, a field the runner
sets, because the runner is the only party that knows which run it is executing
and teaching garmd about runs is not available: knowing nothing about agents or
runs is one of that daemon's invariants, and `CallContext.run_id` existing is not
permission to make it fill one in.

That field is an **assertion** — nothing attests it — and it is used for
**routing** and never for authorization. It keys
`garm.tasks.v1.decided.<tenant>.<run_id>`, and routing decides who hears a
decision, never who may act on one. So the worst a runner can do by naming
somebody else's run is wake it spuriously; that runner then goes to collect the
approval and is refused, holding no capability for a task that was never its.
Noise, not privilege. It is also why the gate above is not built on the same
value: a run id asserted on the create and a run id asserted on the read would be
one unattested claim checked against another.

**A triage decline is a decision made outside the claim.** `triage_task`
admits a person and an agent, and a DECLINE through it refuses the requester
— four eyes — but does not require the claim the way `decide_task` does,
because the design does not ask it to. Somebody who learns a task id and is
inside the tenant can therefore decline a task they never claimed. Until the
daemon's instance authorization lands, four eyes is the guard on that path.

**The invocation carries no runner identity, and no run id either.** §4.4 asks
`create_task` to check that an execution identity is present beside the delegation
chain. `garm.tool.v1.InvocationContext` has no such field, so what is checked is
the principal's kind, that the chain names an agent, and that a run id is on the
call. The check gets stricter when the contract carries the identity.

The run id is not a contract gap and is no longer a gap in `Create` either:
`CallContext.run_id` is declared, `garmd` never sets it, and `Create` therefore
takes the run from its own request field instead. `Caller.RunID` is kept rather
than retired, and **nothing authorizes on it now**: the gate on `get_task_grant`
read it until 2026-10-02 and refused every call as a result. It is decoded so that
everything a call carries is read in one place, and the comment on the field says
it authorizes nothing, so the next gate does not reach for it.

**Nothing here spends an approval.** Single use belongs to the daemon, which
holds the replay cache. An approval replayed at this service closes a task
that is already closed, which changes nothing, but this service would not be
the one to notice.

**The issuer and audience of an approval are not checked by name.** The
signature is checked against the key set of the service that mints them,
which is configured, so a token signed by anything else does not verify. A
deployment that trusts more than one minter would want the issuer checked by
name as well.

## Left to another release

**Card projection.** Every element of a card carries the task's audience as a
label, and the daemon drops what a viewer does not reach. Until the daemon's
projection ships, a card leaves here complete and whoever the catalogue lets
call the tool sees all of it.

**The target tool's own approval card.** The design has a second card beside
this one, served by the tool being approved, with whatever context it wants
to add. Nothing here needs changing for that; it is named so the absence is
not read as an oversight.

**No metrics and no tracing.** The broker's own service statistics are what
there is. The trace context is on the invocation and is not propagated.

**One instance decides; several may serve.** Claims and decisions are settled
by conditional updates in Postgres, so several instances are safe. The expiry
sweep runs on every instance and does the same work more than once; it is
idempotent, and a leader election is not worth it at this size.
