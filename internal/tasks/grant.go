package tasks

import (
	"time"

	"github.com/garm-ai/contracts/grants"

	"github.com/garm-ai/tasksd/internal/store"
)

// GrantKeys is where the approvals handed to decide_task are verified.
//
// It is the key set of the token service that mints them. An interface
// rather than a document so a deployment that refreshes its keys can supply
// something that does, and so a test can supply the one key it signed with.
type GrantKeys = grants.KeySource

// ParseGrantKeys reads a JWKS document.
func ParseGrantKeys(doc []byte) (GrantKeys, error) { return grants.ParseJWKS(doc) }

// clockSkew is how far apart this service and the token service may be
// before a fresh approval reads as expired. Thirty seconds is the usual
// allowance for two machines with the same time source.
const clockSkew = 30 * time.Second

// checkGrant verifies an approval and holds it to the task it was given on.
//
// Verified first — signature, against the key set of the service that mints
// approvals — and only then read. An approval that does not verify is not a
// weaker approval; it is a string somebody sent.
//
// What is checked: the signature; that no delegation chain is on it, so an
// agent cannot approve through one; that it names this tool, this subject
// and this task; that its digest matches the values this service stored when
// the task was opened, not values the caller supplied; that the approver is
// the person making this call; that they held the clearance and the
// compartments the task asks of an approver; and that it is neither expired
// nor older than the window the task was opened with.
//
// What is not checked here: that the approval has not already been spent.
// Single use belongs to the daemon, which holds the replay cache and which
// checks all of this again when the approved call is finally made — so an
// approval replayed at this service closes a task that is already closed,
// and buys nothing.
func checkGrant(raw string, keys GrantKeys, t store.Task, approver string, now time.Time) (*grants.Claims, error) {
	if keys == nil {
		// Only reachable in a build that constructed the service by hand;
		// Serve refuses to start without a key set.
		return nil, refuse(CodeBroke, "no key set: this service cannot check an approval")
	}
	c, err := grants.Verify(raw, keys)
	if err != nil {
		return nil, refusef(CodeRefused, "the approval does not verify: %v", err)
	}
	if err := c.CheckNoAct(); err != nil {
		return nil, refuse(CodeDenied, DelegatedApproverMessage)
	}
	if err := c.CheckTool(t.ToolFQN); err != nil {
		return nil, refusef(CodeRefused, "%v", err)
	}
	if err := c.CheckSubject(t.Subject); err != nil {
		return nil, refusef(CodeRefused, "%v", err)
	}
	if err := c.CheckTask(t.ID); err != nil {
		return nil, refusef(CodeRefused, "%v", err)
	}
	if err := c.CheckMaterial(t.Material); err != nil {
		return nil, refusef(CodeRefused, "%v", err)
	}
	if c.Approver != approver {
		return nil, refuse(CodeDenied, "the approval was given by somebody else")
	}
	if err := c.CheckApprover(clearanceValue(t.Predicate.MinClearance), t.Predicate.Compartments); err != nil {
		return nil, refusef(CodeDenied, "%v", err)
	}
	// The task's own window is the ceiling on how stale an approval may be:
	// a task opened for fifteen minutes does not accept an hour-old one.
	maxAge := t.ExpiresAt.Sub(t.CreatedAt)
	if err := c.CheckFresh(now, clockSkew, maxAge); err != nil {
		return nil, refusef(CodeRefused, "%v", err)
	}
	if c.ID == "" {
		return nil, refuse(CodeRefused, "the approval carries no identifier")
	}
	return c, nil
}
