package tasks

import (
	"fmt"
	"sort"
	"time"

	cardv1 "github.com/garm-ai/garm/contracts/garm/card/v1"

	"github.com/garm-ai/tasksd/internal/store"
)

// The ids the card's one input and its two actions carry. A client reads
// these back when it submits, so they are named once here rather than
// spelled again in every renderer.
const (
	CardActionApprove = "approve"
	CardActionDecline = "decline"
	CardInputReason   = "reason"
)

// reasonMaxLen is the size of the reason box. The service accepts any
// non-blank reason; this is a hint to the renderer, not a rule.
const reasonMaxLen = 500

// Card is the approval card for a task: the frame a person decides from.
//
// Everything on it is labelled at the task's own predicate, because who may
// see a task is a property of the row and not of the contract — two tasks of
// one tool can be for different audiences. The daemon reads those labels and
// drops what the viewer does not reach, so a card that leaves here complete
// arrives at a viewer already cut to their reach, and a viewer who does not
// reach the card at all is answered as though the task did not exist.
//
// The material is on the card because it is what the digest is computed over
// and what the person is being asked about. A tool that wants to say more —
// the name behind an account number, a balance, a screening result — serves
// its own approval card beside this one, labelled by its own field policies.
func Card(t store.Task, evs []store.Event, tr []store.Triage) *cardv1.Card {
	access := labelOf(t.Predicate)
	c := &cardv1.Card{
		Kind:       kindOf(t),
		SubjectId:  t.ID,
		Title:      titleOf(t),
		State:      cardStateOf(t.State),
		Disclosure: &cardv1.Disclosure{},
		Access:     access,
		// The generation the ask was pinned to, so a renderer can tell that
		// two cards were built from one contract.
		CatalogueDigest: t.CatalogueDigest,
	}
	if t.RunID != "" {
		// The ref names the run and the service that serves its card: the
		// agent the runner was acting as when it asked. This service holds
		// the identity that arrived on the call, which is the closest thing
		// it is given to the agent's own name.
		c.Refs = append(c.Refs, &cardv1.CardRef{
			Kind: cardv1.Kind_RUN, SubjectId: t.RunID, Title: "The run that asked",
			ToolFqn: t.Agent,
		})
	}

	if t.Question != "" {
		c.Body = append(c.Body, labelled(text(t.Question, cardv1.Emphasis_DEFAULT), access))
	}
	if len(t.Material) > 0 {
		c.Body = append(c.Body, labelled(section("Material", labelled(
			factSet(materialFacts(t.Material, access)), access)), access))
	}

	about := []*cardv1.Fact{
		{Label: "Requested by", Value: t.Requester, Access: access},
		{Label: "Requested for", Value: t.Subject, Access: access},
		{Label: "Run", Value: t.RunID, Access: access},
		{Label: "Expires", Value: when(t.ExpiresAt), Access: access},
	}
	if t.State == store.StateClaimed && t.Claimant != "" {
		about = append(about, &cardv1.Fact{Label: "Claimed by", Value: t.Claimant, Access: access})
	}
	c.Body = append(c.Body, labelled(factSet(about), access))

	if outcome := outcomeFacts(t, access); outcome != nil {
		c.Body = append(c.Body, labelled(section("Outcome", labelled(factSet(outcome), access)), access))
	}
	if len(tr) > 0 {
		els := make([]*cardv1.Element, 0, len(tr))
		for _, r := range tr {
			line := fmt.Sprintf("%s by %s at %s: %s", lower(r.Action), r.Actor, when(r.At), r.Reason)
			if r.Recommendation != "" {
				line = fmt.Sprintf("%s by %s at %s — %s: %s",
					lower(r.Action), r.Actor, when(r.At), lower(string(r.Recommendation)), r.Reason)
			}
			els = append(els, labelled(text(line, cardv1.Emphasis_SUBTLE), access))
		}
		c.Body = append(c.Body, labelled(section("Triage", els...), access))
	}
	if len(evs) > 0 {
		els := make([]*cardv1.Element, 0, len(evs))
		for _, e := range evs {
			els = append(els, labelled(text(
				fmt.Sprintf("%s by %s at %s", e.Kind, e.Actor, when(e.At)),
				cardv1.Emphasis_SUBTLE), access))
		}
		c.Body = append(c.Body, labelled(section("Audit trail", els...), access))
	}

	if t.State == store.StateOpen || t.State == store.StateClaimed {
		c.Body = append(c.Body, labelled(&cardv1.Element{
			Of: &cardv1.Element_Input{Input: &cardv1.Input{
				Id: CardInputReason, Label: "Reason", Required: true,
				Kind: &cardv1.Input_Text{Text: &cardv1.TextInput{
					MaxLen: reasonMaxLen, Multiline: true}},
			}},
		}, access))
		c.Actions = []*cardv1.Action{
			{Id: CardActionApprove, Label: "Approve", Style: cardv1.Style_POSITIVE,
				Kind: &cardv1.Action_Submit{Submit: &cardv1.Submit{}}},
			{Id: CardActionDecline, Label: "Decline", Style: cardv1.Style_DESTRUCTIVE,
				Kind: &cardv1.Action_Submit{Submit: &cardv1.Submit{}}},
		}
	}
	return c
}

// SummaryCard is one row of a queue: enough to decide whether to open it,
// and no material. A list is read by people who are not going to act on most
// of what is in it.
func SummaryCard(t store.Task) *cardv1.Card {
	access := labelOf(t.Predicate)
	facts := []*cardv1.Fact{
		{Label: "Requested by", Value: t.Requester, Access: access},
		{Label: "Expires", Value: when(t.ExpiresAt), Access: access},
	}
	if t.State == store.StateClaimed && t.Claimant != "" {
		facts = append(facts, &cardv1.Fact{Label: "Claimed by", Value: t.Claimant, Access: access})
	}
	return &cardv1.Card{
		Kind: kindOf(t), SubjectId: t.ID, Title: titleOf(t),
		State:      cardStateOf(t.State),
		Body:       []*cardv1.Element{labelled(factSet(facts), access)},
		Disclosure: &cardv1.Disclosure{},
		Access:     access,
	}
}

// labelOf turns a task's predicate into the label every part of its card
// carries. An absent label would mean the endpoint's own policy, which for
// this service is the lowest clearance any tool declares — so a card whose
// predicate is empty is labelled at the unspecified clearance, which reaches
// nobody, rather than left bare.
func labelOf(p store.Predicate) *cardv1.Label {
	return &cardv1.Label{
		Clearance:    clearanceValue(p.MinClearance),
		Compartments: append([]string(nil), p.Compartments...),
	}
}

func materialFacts(m map[string]string, access *cardv1.Label) []*cardv1.Fact {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	// Sorted, because a map has no order and a card built twice must read
	// the same. It is also the order the digest is computed in.
	sort.Strings(paths)
	out := make([]*cardv1.Fact, 0, len(paths))
	for _, p := range paths {
		out = append(out, &cardv1.Fact{
			Label: p, Value: m[p],
			// Field names the path this value came from, which is what a
			// client rebuilds the material map from when it fetches the
			// target tool's own card for the same task.
			Field: p, Access: access,
		})
	}
	return out
}

func outcomeFacts(t store.Task, access *cardv1.Label) []*cardv1.Fact {
	if t.Decision == "" && t.State != store.StateExpired {
		return nil
	}
	decision := lower(string(t.Decision))
	if t.State == store.StateExpired {
		decision = "expired"
	}
	out := []*cardv1.Fact{{Label: "Decision", Value: decision, Access: access}}
	if t.DecidedBy != "" {
		out = append(out, &cardv1.Fact{Label: "Decided by", Value: t.DecidedBy, Access: access})
	}
	if t.OnBehalfOf != "" {
		out = append(out, &cardv1.Fact{Label: "On behalf of", Value: t.OnBehalfOf, Access: access})
	}
	if t.DecidedAt != nil {
		out = append(out, &cardv1.Fact{Label: "Decided at", Value: when(*t.DecidedAt), Access: access})
	}
	if t.Reason != "" {
		out = append(out, &cardv1.Fact{Label: "Reason", Value: t.Reason, Access: access})
	}
	if t.GrantJTI != "" {
		out = append(out, &cardv1.Fact{Label: "Approval", Value: t.GrantJTI, Access: access})
	}
	return out
}

func titleOf(t store.Task) string {
	if t.Kind == store.KindAsk {
		return "A question from a run"
	}
	if t.ToolFQN == "" {
		return "An approval"
	}
	return "Approve " + t.ToolFQN
}

func kindOf(t store.Task) cardv1.Kind {
	if t.Kind == store.KindAsk {
		return cardv1.Kind_ASK
	}
	return cardv1.Kind_TASK
}

// cardStateOf is the card vocabulary's view of a task: waiting is OPEN
// whether or not somebody holds the claim, a decision of any sort is
// ANSWERED, and running out of time is its own state.
func cardStateOf(s store.State) cardv1.State {
	switch s {
	case store.StateApproved, store.StateDeclined, store.StateAnswered:
		return cardv1.State_ANSWERED
	case store.StateExpired:
		return cardv1.State_EXPIRED
	default:
		return cardv1.State_OPEN
	}
}

func labelled(e *cardv1.Element, l *cardv1.Label) *cardv1.Element {
	e.Access = l
	return e
}

func text(s string, e cardv1.Emphasis) *cardv1.Element {
	return &cardv1.Element{Of: &cardv1.Element_Text{Text: &cardv1.Text{Text: s, Emphasis: e}}}
}

func factSet(fs []*cardv1.Fact) *cardv1.Element {
	return &cardv1.Element{Of: &cardv1.Element_Facts{Facts: &cardv1.FactSet{Facts: fs}}}
}

func section(title string, els ...*cardv1.Element) *cardv1.Element {
	return &cardv1.Element{Of: &cardv1.Element_Section{
		Section: &cardv1.Section{Title: title, Elements: els}}}
}

// when is how every time on a card is written: RFC 3339, UTC, whole seconds.
// No fractional seconds, because a card is not a log.
func when(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
