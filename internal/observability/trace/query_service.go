package trace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type TraceStore interface {
	QueryTurnInSession(
		context.Context,
		string,
		protocol.TurnID,
	) ([]Record, error)
}

type SessionReader interface {
	GetLifecycle(
		context.Context,
		string,
	) (protocol.SessionSummary, error)
}

type TraceQuery struct {
	SessionID       string            `json:"session_id"`
	TurnIDs         []protocol.TurnID `json:"turn_ids"`
	ThroughSequence protocol.Cursor   `json:"through_sequence"`
}

type TraceSnapshot struct {
	Version         int             `json:"version"`
	SessionID       string          `json:"session_id"`
	ThroughSequence protocol.Cursor `json:"through_sequence"`
	Turns           []TraceTurn     `json:"turns"`
}

type TraceTurn struct {
	TurnID protocol.TurnID `json:"turn_id"`
	// Complete means the recorder has been released and the durable root has
	// ended. Active or unavailable snapshots must remain refreshable.
	Complete  bool        `json:"complete"`
	StartedAt *time.Time  `json:"started_at,omitempty"`
	EndedAt   *time.Time  `json:"ended_at,omitempty"`
	Status    string      `json:"status"`
	Spans     []TraceSpan `json:"spans"`
}

type TraceSpan struct {
	ID         uint64     `json:"id"`
	ParentID   *uint64    `json:"parent_id,omitempty"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	DurationMS *int64     `json:"duration_ms,omitempty"`
	Sample     *uint32    `json:"sample,omitempty"`
	CallID     string     `json:"call_id,omitempty"`
}

type QueryService struct {
	sessions SessionReader
	store    TraceStore
	active   Runtime
}

func NewQueryService(
	sessions SessionReader,
	store TraceStore,
	active Runtime,
) *QueryService {
	return &QueryService{sessions: sessions, store: store, active: active}
}

func (s *QueryService) Query(
	ctx context.Context,
	query TraceQuery,
) (TraceSnapshot, error) {
	if s == nil || s.sessions == nil || s.store == nil {
		return TraceSnapshot{}, queryProblem(
			protocol.CodeUnavailable,
			"trace query is unavailable",
			nil,
		)
	}
	query.SessionID = strings.TrimSpace(query.SessionID)
	if query.SessionID == "" || len(query.TurnIDs) == 0 {
		return TraceSnapshot{}, queryProblem(
			protocol.CodeInvalidArgument,
			"trace query requires a session and at least one turn",
			nil,
		)
	}
	session, err := s.sessions.GetLifecycle(ctx, query.SessionID)
	if err != nil {
		return TraceSnapshot{}, err
	}
	if query.ThroughSequence > session.LatestSequence {
		return TraceSnapshot{}, queryProblem(
			protocol.CodeConflict,
			"trace query watermark is ahead of the session",
			nil,
		)
	}
	result := TraceSnapshot{
		Version: 1, SessionID: query.SessionID,
		ThroughSequence: query.ThroughSequence,
		Turns:           make([]TraceTurn, 0, len(query.TurnIDs)),
	}
	seen := make(map[protocol.TurnID]struct{}, len(query.TurnIDs))
	for _, turnID := range query.TurnIDs {
		if turnID == "" {
			return TraceSnapshot{}, queryProblem(
				protocol.CodeInvalidArgument,
				"trace query turn id is required",
				nil,
			)
		}
		if _, duplicate := seen[turnID]; duplicate {
			continue
		}
		seen[turnID] = struct{}{}
		durable, err := s.store.QueryTurnInSession(ctx, query.SessionID, turnID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return TraceSnapshot{}, queryProblem(
					protocol.CodeInvalidArgument,
					fmt.Sprintf("turn %s does not belong to session", turnID),
					err,
				)
			}
			return TraceSnapshot{}, err
		}
		records := s.active.ActiveTurnSpans(turnID)
		active := len(records) != 0
		if !active {
			records = durable
		}
		turn := projectTraceTurn(turnID, records)
		turn.Complete = !active && turn.EndedAt != nil
		for _, span := range turn.Spans {
			if span.EndedAt == nil {
				turn.Complete = false
			}
		}
		result.Turns = append(result.Turns, turn)
	}
	return result, nil
}

func projectTraceTurn(
	turnID protocol.TurnID,
	records []Record,
) TraceTurn {
	result := TraceTurn{
		TurnID: turnID,
		Status: "unavailable",
		Spans:  make([]TraceSpan, 0, len(records)),
	}
	for _, record := range records {
		kind, ok := publicTraceKind(record.Name)
		if !ok {
			continue
		}
		span := TraceSpan{
			ID: record.ID, Kind: kind, Status: string(record.Status),
			StartedAt: record.Started,
		}
		if record.ParentID != 0 {
			parent := record.ParentID
			span.ParentID = &parent
		}
		if !record.Ended.IsZero() {
			ended := record.Ended
			duration := record.Duration().Milliseconds()
			span.EndedAt = &ended
			span.DurationMS = &duration
		}
		if sample, ok := unsigned32(record.Attributes["sample"]); ok {
			span.Sample = &sample
		}
		if callID, ok := record.Attributes["call_id"].(string); ok {
			span.CallID = callID
		}
		result.Spans = append(result.Spans, span)
		if record.Name == NameTurn {
			started := record.Started
			result.StartedAt = &started
			result.Status = string(record.Status)
			if !record.Ended.IsZero() {
				ended := record.Ended
				result.EndedAt = &ended
			}
		}
	}
	return result
}

func publicTraceKind(name string) (string, bool) {
	switch name {
	case NameTurn:
		return "turn", true
	case NameModelCall:
		return "model", true
	case NameTool:
		return "tool", true
	case NameApprovalWait:
		return "approval", true
	case NameVerify:
		return "verification", true
	default:
		return "", false
	}
}

func unsigned32(value any) (uint32, bool) {
	switch typed := value.(type) {
	case uint32:
		return typed, true
	case uint64:
		return uint32(typed), uint64(uint32(typed)) == typed
	case int:
		return uint32(typed), typed >= 0 && uint64(typed) <= uint64(^uint32(0))
	case float64:
		value := uint32(typed)
		return value, typed >= 0 && float64(value) == typed
	default:
		return 0, false
	}
}

func queryProblem(
	code protocol.ErrorCode,
	message string,
	cause error,
) *protocol.Problem {
	return protocol.NewProblem(code, message, false, cause)
}
