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
	// attribution. Only a runner's calls carry one.
	RunID  string
	CallID string
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

// Agent is the innermost delegate — the agent that made this call on the
// subject's behalf — or "" when the subject called for themselves.
func (c Caller) Agent() string {
	if len(c.Act) == 0 {
		return ""
	}
	return c.Act[len(c.Act)-1].Subject
}

// HasAgent reports whether the chain names at least one agent, which is what
// distinguishes a runner's call from a person's.
func (c Caller) HasAgent() bool {
	for _, a := range c.Act {
		if a.Kind == toolv1.PrincipalKind_PRINCIPAL_KIND_AGENT {
			return true
		}
	}
	return false
}

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
