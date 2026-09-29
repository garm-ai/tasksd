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

## Not ours, and in the way

**The race detector cannot run over the wire-level suite.** `tool-go` v0.5.0
runs a request's handler in a goroutine of its own, and `nats.go`'s micro
package reads that request's error field after the handler returns, to count
it in the endpoint's statistics. Any tool service that answers a coded
refusal therefore trips the detector, inside those two libraries and nowhere
near this repository — the two stacks in the report are `micro.(*request).Error`
and `micro.(*service).reqHandler`. `mise run test` runs the detector over
this repository's own packages and runs the wire suite without it. It goes
back the moment `tool-go` answers from the goroutine micro is waiting on, or
stops reading the request after handing it over.

## Where a check is thinner than the design

**The wire shape this service advertises is computed here.** A daemon
compares what a service advertises with what the catalogue declares, and
refuses to route when they differ. The value is derived from the linked
contract by `internal/tasks/contract.go`, using the encoding
`garm catalogue build` stamps — written here because that function is not
exported from `garm` and no generated binding for this contract exists yet.
The right home for it is `garm`'s contracts module, beside the approval
verification that moved there; when it lands this becomes an import. A
mismatch is loud rather than silent: the daemon refuses to route and says
which package and why.

**A triage decline is a decision made outside the claim.** `triage_task`
admits a person and an agent, and a DECLINE through it refuses the requester
— four eyes — but does not require the claim the way `decide_task` does,
because the design does not ask it to. Somebody who learns a task id and is
inside the tenant can therefore decline a task they never claimed. Until the
daemon's instance authorization lands, four eyes is the guard on that path.

**The invocation carries no runner identity.** §4.4 asks `create_task` to
check that an execution identity is present beside the delegation chain.
`garm.tool.v1.InvocationContext` has no such field at v0.17.0, so what is
checked is that the principal is a person, that the chain names an agent, and
that a run id is on the call. The check gets stricter when the contract
carries the identity.

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
