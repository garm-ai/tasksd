# tasksd

**The task service behind garm's human approvals.** When an agent is about to
make a call that needs a person's yes, something has to hold the question
until a person answers it. That is this: a queue of tasks, each one a tool
call waiting on a decision, with the values the person is being asked about,
the audience that may answer, and the trail of what happened.

It is a governed tool like any other. The contract lives in
[garm](https://github.com/garm-ai/garm) as `garm.tasks.v1`, the service
answers over NATS, and every call goes through
[garmd](https://github.com/garm-ai/garmd) first — so opening a task, reading
the queue, claiming one and deciding it are each a governed call with a
ledger row, a declared clearance and an audience, the same as the payment the
task is about.

## Contents

- [What it does](#what-it-does)
- [The tools it serves](#the-tools-it-serves)
- [The rules it enforces](#the-rules-it-enforces)
- [Running it](#running-it)
- [Configuration](#configuration)
- [The store](#the-store)
- [Telling a run its answer](#telling-a-run-its-answer)
- [What it does not do](#what-it-does-not-do)
- [Working here](#working-here)
- [Licence](#licence)

## What it does

A runner reaches a step where the tool it wants to call needs an approval. It
calls `create_task` with the tool, the values the approval is over, and who
may decide. The task lands in a queue.

A person opens their queue, sees the task, claims it, reads what is being
asked, and approves or declines with a reason. Approving means their own
client mints an approval token against their own credentials and hands it in
with the decision; this service checks that the token names this task and
these values before it records anything.

The run is then told, and carries on — with the approval, if it got one.

An agent can work the same queue without being able to say yes. It can
recommend, comment, hand a task to a stricter audience, or decline it with a
reason. There is no argument to any method here that lets an agent approve.

## The tools it serves

| Tool | Who calls it | What it does |
|---|---|---|
| `create_task` | a runner | Opens a task for the run it is executing |
| `list_tasks` | a person, an agent | The queue, what is mine, what is done — as cards |
| `get_task` | a person, an agent | One task: the frame, its trail, its triage, its card |
| `approval_card` | a person | The card a person decides from |
| `claim_task` | a person, an agent | Takes a task out of the queue so two people do not decide it at once |
| `release_task` | a person, an agent | Hands a claim back |
| `decide_task` | a person | Approves, declines or answers |
| `triage_task` | an agent | Recommends, comments, reassigns or declines — never approves |

Each one is declared in `garm.tasks.v1` with its verb, its clearance and its
audience. Nothing in this repository decides who may call what; garmd does
that from the catalogue, before the call arrives.

## The rules it enforces

Everything below comes from `Garm-Invocation`, the assertions garmd puts on
the call. There is no token here and no clearance: what a viewer may see is
decided by the label on the card, which garmd projects.

- **Four eyes.** The person a run belongs to cannot decide that run's task.
  The queue does not show it to them and a decision by them is refused.
- **No approval through an agent.** A call to `decide_task` that arrives with
  a delegation chain is refused, and so is an approval token that carries
  one. The token service refuses to mint such a token and garmd refuses to
  verify it, so the rule holds in three places.
- **A claim before a decision.** A task is claimed first, and only its
  claimant decides it. A claim lapses after a while so a task nobody
  finished goes back to the queue.
- **The approval names this task.** The token has to name this tool, this
  subject, this task id and a digest over the values this service stored —
  not values the caller sent. Two identical payments asked twice cannot share
  one approval.
- **One ask, one task.** A runner retrying a call it never saw the answer to
  gets the task it already opened.
- **Decided once.** A second decision on a decided task changes nothing and
  says so.

## Running it

```
go install github.com/garm-ai/tasksd/cmd/tasksd@latest

tasksd \
  --nats nats://nats.example.internal:4222 \
  --postgres 'postgres://tasksd@db.example.internal:5432/tasksd?sslmode=require' \
  --grant-keys /etc/tasksd/approval-keys.json
```

It migrates its database at startup, connects to the broker, and serves. It
refuses to start without a database it can reach or a key set it can read: a
service that came up without either would advertise itself and then refuse
every call, which reads as an outage rather than as the mistake it is.

`SIGTERM` and `SIGINT` drain: calls in flight finish, no new ones are taken.

To run it inside another process — a development stack that runs several garm
components together — import the repository root and call `Serve`:

```go
err := tasksd.Serve(ctx, tasksd.Config{
    NATS: "nats://127.0.0.1:4222", Postgres: dsn, GrantKeys: jwks,
})
```

It blocks until the context is cancelled, then drains. The binary is flag
parsing and a call to the same function.

## Configuration

| Flag | What it is |
|---|---|
| `--nats` | The broker to serve on |
| `--postgres` | This service's own database. Also read from `POSTGRES_DSN` |
| `--grant-keys` | The JWKS of the service that mints approvals. Required |
| `--concurrency` | Calls in flight at once. Default 16 |
| `--claim-ttl` | How long a claim holds before anybody may release it. Default 30m |
| `--sweep-every` | How often tasks that ran out of time are marked expired. Default 1m |

There is no policy file. Who may approve what is declared on the tool being
approved and carried on the task; who may call these tools is in the
catalogue.

## The store

Three tables of its own: `tasks`, `task_events` and `triage`. Migrations are
numbered and recorded in a version table, applied at startup, one transaction
each.

The values a person is being asked about are stored as readable JSON, because
they are what the approval's digest is computed over and what the card shows
— a value a person cannot read is a value they cannot approve. One column
holds a credential, `grant` on `tasks`, and it holds a spent one: the
approval that was presented, kept for the audit trail. A test walks every
other column and the trail to check that nothing else ever does.

## Telling a run its answer

A decision is recorded first and published second, on
`garm.tasks.v1.decided.<tenant>.<run_id>`, with the task and its trail
position as the message id so a redelivery is recognised rather than acted on
twice. A decision that cannot be published stands, and the trail records that
the run was not told; the run's own waiting window bounds how long it sits
there.

## What it does not do

- **It does not mint approvals.** The person's own client does that, against
  the person's own credentials, at the token service. A service minting on
  somebody's behalf would have the token service attesting this service's
  word instead of the person's.
- **It does not spend them.** Single use belongs to garmd, which holds the
  replay cache and checks the approval again when the approved call is made.
- **It does not decide who may see a task.** Each card carries the task's
  audience as a label and garmd drops what a viewer does not reach.
- **It does not authenticate, authorise or redact.** garmd did all of that
  before the call arrived.
- **It does not know about agents, runs or tools** beyond names it stores and
  hands back.

## Working here

```
mise install    the toolchain
mise run pg     a throwaway Postgres, and the POSTGRES_DSN to export
mise run test   the tests
mise run ci     what CI runs
```

The store and wire tests need Postgres and skip without `POSTGRES_DSN`. They
are not optional — CI always sets it — but a fake Postgres would be testing
the fake, and the failures worth catching here are constraint violations and
the behaviour of an update that matches no rows.

`KNOWN-GAPS.md` says what is not built yet and what is left to another
release.

## Licence

MIT. See [LICENSE](LICENSE).
