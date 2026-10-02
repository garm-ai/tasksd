// Package tasks is the service behind garm.tasks.v1: who may open a task,
// who may see it, who may claim it and who may decide it.
//
// Everything it knows about a caller comes from Garm-Invocation, which the
// daemon sets on the hop: the subject, the tenant, the delegation chain and
// the run the call belongs to. There is no token here and no clearance —
// a tool that could see a clearance is a tool that would start filtering on
// one, and which tasks a viewer may see is decided by the label on the card
// the daemon projects, not by this service.
//
// What this service does decide is the part a label cannot express: that the
// person deciding is not the person who asked, that they hold the claim,
// that the task is still open, and that the approval they present names this
// task and these values.
package tasks

import (
	"context"

	"github.com/garm-ai/contracts/callctx"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"
)

// Caller is the identity behind one call, read off the hop and nowhere else.
type Caller struct {
	Subject string
	Tenant  string
	Kind    toolv1.PrincipalKind
	// Act is the delegation chain, innermost last: the agents acting for
	// Subject. An empty chain is a person calling for themselves.
	Act []Act
	// RunID is the run this call belongs to, from the invocation's
	// attribution.
	//
	// NOTHING IN THIS SERVICE AUTHORIZES ON IT, and that is the state of the
	// field rather than an oversight. garmd never sets it — it builds an
	// attribution with a tenant and a correlation id — so it arrives empty on
	// every call that reaches here through the daemon. `create_task` takes the
	// run from its own request field for that reason, and `get_task_grant` is
	// gated on the caller's attested identity, because a gate over this value
	// refused every call. It is decoded anyway, so that everything a call
	// carries is read in one place and the emptiness is visible here rather
	// than rediscovered by the next gate that reaches for it.
	RunID  string
	CallID string

	// agent is InvocationContext.agent (contracts v0.12.0, field 12): whose
	// run this call belongs to. Attribution, never authorization — the same
	// stance as RunID above, and for the same reason this is read through a
	// method rather than the bare field: so the one place that computes it
	// is the one place a reader checks for what it is allowed to mean.
	//
	// Unlike RunID, garmd DOES set this one (core.go's withInvocationContext,
	// from Principal.Actor) — but it is only ever non-empty when the call
	// arrived through a real delegation chain. The one caller that reaches
	// create_task, a SERVICE self-mint, carries no chain at all as of sts
	// v0.6.1 (the agent named in that mint is spent on CanRun and never
	// becomes an actor — decisions/2026-10-02-the-agent-is-authorization-
	// input-not-an-actor.md), so it arrives empty THERE too, same as RunID.
	// Nothing here refuses on that: see Create's own comment at the check
	// this field used to require.
	agent string
}

// Act is one link of the delegation chain.
type Act struct {
	Subject string
	Kind    toolv1.PrincipalKind
}

// Delegated reports whether somebody is acting for the subject. It is the
// check that keeps an agent from approving: the chain is empty or the call
// is not a person's own.
func (c Caller) Delegated() bool { return len(c.Act) > 0 }

// Agent is whose run this call belongs to — attribution, from
// InvocationContext.agent — or "" when the call does not say.
//
// It used to be derived from the chain, c.Act[len-1].Subject: the innermost
// delegate. That conflated two different claims — act carries delegation
// FOR AUTHORITY, and reading attribution off it is what let an agent's own
// tool_sets fold into a service's and empty them (F21), and separately left
// this value readable only via a Kind the daemon never set (F22). Sourced
// from attribution instead, it answers only "whose run was it" and nothing
// here authorizes on the answer.
func (c Caller) Agent() string { return c.agent }

// CallerFrom reads the invocation context off ctx.
//
// It asserts the shape and never the authority: the daemon already decided
// that this caller may reach this tool. What this service needs is a subject
// to attribute a decision to and a tenant to confine it, and it refuses a
// call that names neither rather than recording a task nobody owns.
func CallerFrom(ctx context.Context) (Caller, error) {
	ic := callctx.FromContext(ctx)
	if ic == nil {
		// The tool runtime refuses a call with no invocation context before
		// a handler sees it, so reaching here means a runtime that decoded
		// one and did not attach it.
		return Caller{}, refuse(CodeRefused, "the call carries no invocation context")
	}
	c := Caller{
		Subject: ic.GetPrincipal().GetSubject(),
		Tenant:  ic.GetAttribution().GetTenant(),
		Kind:    ic.GetPrincipal().GetKind(),
		RunID:   ic.GetAttribution().GetRunId(),
		CallID:  ic.GetCallId(),
		agent:   ic.GetAgent(),
	}
	if c.Subject == "" {
		return Caller{}, refuse(CodeRefused, "the invocation context names no subject")
	}
	if c.Tenant == "" {
		return Caller{}, refuse(CodeRefused, "the invocation context names no tenant")
	}
	for _, a := range ic.GetAct() {
		c.Act = append(c.Act, Act{Subject: a.GetSubject(), Kind: a.GetKind()})
	}
	return c, nil
}
