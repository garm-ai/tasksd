package tasks

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cardv1 "github.com/garm-ai/contracts/garm/card/v1"
	toolv1 "github.com/garm-ai/contracts/garm/tool/v1"

	tasksv1 "github.com/garm-ai/tasksd/gen/garm/tasks/v1"
	"github.com/garm-ai/tasksd/internal/store"
)

// Handlers is the contract's nine methods over the service.
//
// It translates and nothing else: every rule about who may do what is in
// Service, so a reader looking for the checks finds them in one file and a
// second transport would not be a second set of them.
type Handlers struct{ Svc *Service }

// CreateTask opens a task for a run.
func (h Handlers) CreateTask(ctx context.Context, req *tasksv1.CreateTaskRequest) (*tasksv1.CreateTaskResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Create(ctx, c, CreateInput{
		Kind:            kindFrom(req.GetKind()),
		Tool:            req.GetTool(),
		Subject:         req.GetSubject(),
		Requester:       req.GetRequester(),
		RunID:           req.GetRunId(),
		Material:        req.GetMaterial(),
		Predicate:       predicateFrom(req.GetPredicate()),
		ExpiresIn:       time.Duration(req.GetExpiresInSeconds()) * time.Second,
		DecisionType:    req.GetDecisionType(),
		Question:        req.GetQuestion(),
		CatalogueDigest: req.GetCatalogueDigest(),
	})
	if err != nil {
		return nil, err
	}
	return &tasksv1.CreateTaskResponse{TaskId: proto.String(t.ID)}, nil
}

// GetTaskGrant hands a parked run the approval it was waiting for.
//
// Every refusal comes back from the service, which is where the gate is. This
// translates and nothing else — in particular it does not log the answer it is
// building, because the answer is a bearer.
func (h Handlers) GetTaskGrant(ctx context.Context, req *tasksv1.GetTaskGrantRequest) (*tasksv1.TaskGrant, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	a, err := h.Svc.ReadGrant(ctx, c, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return &tasksv1.TaskGrant{
		TaskId:   proto.String(a.TaskID),
		Grant:    proto.String(a.Grant),
		GrantJti: proto.String(a.GrantJTI),
	}, nil
}

// ListTasks answers one screen as a list of cards.
func (h Handlers) ListTasks(ctx context.Context, req *tasksv1.ListTasksRequest) (*tasksv1.ListTasksResponse, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	ts, cursor, err := h.Svc.List(ctx, c, viewFrom(req.GetView()), int(req.GetPageSize()), req.GetCursor())
	if err != nil {
		return nil, err
	}
	out := &tasksv1.ListTasksResponse{Cursor: proto.String(cursor)}
	for _, t := range ts {
		out.Cards = append(out.Cards, SummaryCard(t))
	}
	return out, nil
}

// GetTask is the typed frame plus the card.
func (h Handlers) GetTask(ctx context.Context, req *cardv1.TaskRef) (*tasksv1.Task, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Get(ctx, c, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return h.taskProto(ctx, t)
}

// ApprovalCard is the card a person decides from.
//
// The material on the ref is ignored: this service serves what it stored,
// which is what the digest was computed over. A client fills the ref from
// this card's own facts when it goes on to ask the target tool for its
// rendering of the same values.
func (h Handlers) ApprovalCard(ctx context.Context, req *cardv1.TaskRef) (*cardv1.Card, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Get(ctx, c, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	evs, tr, err := h.Svc.Trail(ctx, t)
	if err != nil {
		return nil, err
	}
	return Card(t, evs, tr), nil
}

// ClaimTask takes a task out of the queue.
func (h Handlers) ClaimTask(ctx context.Context, req *cardv1.TaskRef) (*tasksv1.Task, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Claim(ctx, c, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return h.taskProto(ctx, t)
}

// ReleaseTask hands a claim back.
func (h Handlers) ReleaseTask(ctx context.Context, req *cardv1.TaskRef) (*tasksv1.Task, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Release(ctx, c, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return h.taskProto(ctx, t)
}

// DecideTask closes a task.
func (h Handlers) DecideTask(ctx context.Context, req *tasksv1.DecideTaskRequest) (*tasksv1.Task, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	var answer []byte
	if a := req.GetAnswer(); a != nil {
		b, err := proto.Marshal(a)
		if err != nil {
			return nil, refuse(CodeRefused, "the answer could not be read")
		}
		answer = b
	}
	t, err := h.Svc.Decide(ctx, c, DecideInput{
		TaskID:   req.GetTaskId(),
		Decision: decisionFrom(req.GetDecision()),
		Reason:   req.GetReason(),
		Grant:    req.GetGrant(),
		Answer:   answer,
	})
	if err != nil {
		return nil, err
	}
	return h.taskProto(ctx, t)
}

// TriageTask is the agent's path.
func (h Handlers) TriageTask(ctx context.Context, req *tasksv1.TriageTaskRequest) (*tasksv1.Task, error) {
	c, err := CallerFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.Svc.Triage(ctx, c, TriageInput{
		TaskID:         req.GetTaskId(),
		Action:         actionFrom(req.GetAction()),
		Recommendation: decisionFrom(req.GetRecommendation()),
		Reason:         req.GetReason(),
		ReassignTo:     predicateFrom(req.GetReassignTo()),
	})
	if err != nil {
		return nil, err
	}
	return h.taskProto(ctx, t)
}

// taskProto is the typed frame: the row, its trail, its triage and its card.
func (h Handlers) taskProto(ctx context.Context, t store.Task) (*tasksv1.Task, error) {
	evs, tr, err := h.Svc.Trail(ctx, t)
	if err != nil {
		return nil, err
	}
	out := &tasksv1.Task{
		TaskId:    proto.String(t.ID),
		Kind:      kindProto(t.Kind).Enum(),
		State:     stateProto(t.State).Enum(),
		Tool:      proto.String(t.ToolFQN),
		Subject:   proto.String(t.Subject),
		Requester: proto.String(t.Requester),
		RunId:     proto.String(t.RunID),
		Predicate: predicateProto(t.Predicate),
		Claimant:  proto.String(t.Claimant),
		DecidedBy: proto.String(t.DecidedBy),
		Decision:  decisionProto(t.Decision).Enum(),
		Reason:    proto.String(t.Reason),
		GrantJti:  proto.String(t.GrantJTI),
		CreatedAt: timestamppb.New(t.CreatedAt),
		ExpiresAt: timestamppb.New(t.ExpiresAt),
		Card:      Card(t, evs, tr),
	}
	if t.DecidedAt != nil {
		out.DecidedAt = timestamppb.New(*t.DecidedAt)
	}
	for _, e := range evs {
		out.Events = append(out.Events, &tasksv1.Event{
			Seq: proto.Uint32(uint32(e.Seq)), At: timestamppb.New(e.At),
			Actor: proto.String(e.Actor), Kind: proto.String(e.Kind),
			Detail: proto.String(detailText(e.Detail)),
		})
	}
	for _, r := range tr {
		out.Triage = append(out.Triage, &tasksv1.Triage{
			Actor:          proto.String(r.Actor),
			Action:         actionProto(Action(r.Action)).Enum(),
			Recommendation: decisionProto(r.Recommendation).Enum(),
			Reason:         proto.String(r.Reason),
			At:             timestamppb.New(r.At),
		})
	}
	if len(t.Answer) > 0 {
		var a anypb.Any
		if err := proto.Unmarshal(t.Answer, &a); err == nil {
			out.Answer = &a
		}
	}
	return out, nil
}

// detailText is the one thing a trail entry says beyond what and by whom.
// The reason, when there is one, because that is what a person reads; never
// a credential, and the only column that holds one is not this.
func detailText(d map[string]any) string {
	if d == nil {
		return ""
	}
	if s, ok := d["reason"].(string); ok {
		return s
	}
	return ""
}

func kindFrom(k tasksv1.Kind) store.Kind {
	switch k {
	case tasksv1.Kind_ASK:
		return store.KindAsk
	case tasksv1.Kind_APPROVAL:
		return store.KindApproval
	default:
		return ""
	}
}

func kindProto(k store.Kind) tasksv1.Kind {
	if k == store.KindAsk {
		return tasksv1.Kind_ASK
	}
	return tasksv1.Kind_APPROVAL
}

func stateProto(s store.State) tasksv1.State {
	return tasksv1.State(tasksv1.State_value[string(s)])
}

func decisionFrom(d tasksv1.Decision) store.Decision {
	switch d {
	case tasksv1.Decision_APPROVE:
		return store.DecisionApprove
	case tasksv1.Decision_DECLINE:
		return store.DecisionDecline
	case tasksv1.Decision_ANSWER:
		return store.DecisionAnswer
	default:
		return ""
	}
}

func decisionProto(d store.Decision) tasksv1.Decision {
	return tasksv1.Decision(tasksv1.Decision_value[string(d)])
}

func actionFrom(a tasksv1.TriageTaskRequest_Action) Action {
	switch a {
	case tasksv1.TriageTaskRequest_RECOMMEND:
		return ActionRecommend
	case tasksv1.TriageTaskRequest_COMMENT:
		return ActionComment
	case tasksv1.TriageTaskRequest_REASSIGN:
		return ActionReassign
	case tasksv1.TriageTaskRequest_DECLINE:
		return ActionDecline
	default:
		return ""
	}
}

func actionProto(a Action) tasksv1.TriageTaskRequest_Action {
	return tasksv1.TriageTaskRequest_Action(tasksv1.TriageTaskRequest_Action_value[string(a)])
}

func viewFrom(v tasksv1.ListTasksRequest_View) View {
	switch v {
	case tasksv1.ListTasksRequest_MINE:
		return ViewMine
	case tasksv1.ListTasksRequest_DONE:
		return ViewDone
	default:
		return ViewQueue
	}
}

func predicateFrom(p *tasksv1.Predicate) store.Predicate {
	if p == nil {
		return store.Predicate{}
	}
	name := toolv1.Clearance_name[int32(p.GetMinClearance())]
	if p.GetMinClearance() == toolv1.Clearance_CLEARANCE_UNSPECIFIED {
		name = ""
	}
	return store.Predicate{MinClearance: name, Compartments: p.GetCompartments()}
}

func predicateProto(p store.Predicate) *tasksv1.Predicate {
	return &tasksv1.Predicate{
		MinClearance: clearanceValue(p.MinClearance).Enum(),
		Compartments: p.Compartments,
	}
}
