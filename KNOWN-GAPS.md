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

The value is **no longer** that one. `requester` on `CreateTaskRequest` moved it
to `6d981dae…3e50` on 2026-10-01 — a field, which the hash does read — and
`escalation` on `create_task` did **not** move it again the same day, because a
tool set is an option. Both repositories carry both changes identically, so the
two copies still hash alike and the overlap is still safe.

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

**Nothing here can reach a task back on resume with `escalation` alone.**
`create_task` is in a set of its own so that the runner's service principal can
be granted the reach to open a task and nothing more — it holds `escalation` and
is refused every other method of this contract. The resume path needs more than
that: the decided event carries a task id and an outcome and never the grant
(ruling R5), so a runner that wakes has to read the task back through `get_task`,
which is in `triage`. Granting the runner `triage` would hand every run it
executes the whole queue, including `claim_task` and `triage_task`, so that is
not the answer either and the question is open. Whatever closes it is a change to
this contract, not a deployment's workaround: either `get_task` gains a second
set, or a narrower read for a runner is declared. **Until then a runner can open
a task and cannot read it back.**

**A triage decline is a decision made outside the claim.** `triage_task`
admits a person and an agent, and a DECLINE through it refuses the requester
— four eyes — but does not require the claim the way `decide_task` does,
because the design does not ask it to. Somebody who learns a task id and is
inside the tenant can therefore decline a task they never claimed. Until the
daemon's instance authorization lands, four eyes is the guard on that path.

**The invocation carries no runner identity.** §4.4 asks `create_task` to
check that an execution identity is present beside the delegation chain.
`garm.tool.v1.InvocationContext` has no such field at `contracts` v0.2.0, so
what is checked is that the principal is a person, that the chain names an
agent, and that a run id is on the call. The check gets stricter when the
contract carries the identity.

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
