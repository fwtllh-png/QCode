package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/persist/history"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (r *SessionService) SessionLifecycleAvailable() bool {
	return r != nil && r.sessionLifecycle != nil
}

func (r *SessionService) ListSessions(
	ctx context.Context,
	query protocol.SessionListQuery,
) (protocol.SessionList, error) {
	if r.sessionLifecycle == nil {
		return protocol.SessionList{}, runtimeProblem(protocol.CodeUnavailable, "session lifecycle is unavailable", nil)
	}
	if err := query.Validate(); err != nil {
		return protocol.SessionList{},
			runtimeProblem(protocol.CodeInvalidArgument, err.Error(), err)
	}
	if r.workspaceRoot != "" {
		if query.WorkspaceRoot != "" &&
			!sameWorkspaceRoot(r.workspaceRoot, query.WorkspaceRoot) {
			return protocol.SessionList{}, runtimeProblem(
				protocol.CodeConflict,
				"session query workspace does not match the Runtime binding",
				nil,
			)
		}
		query.WorkspaceRoot = r.workspaceRoot
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	searchQuery := strings.TrimSpace(query.Query)
	storeQuery := query
	if searchQuery != "" {
		storeQuery.Query = ""
	}
	if query.Status != "" || searchQuery != "" {
		storeQuery.Status = ""
		storeQuery.Limit = 1000
	}
	page, err := r.sessionLifecycle.ListLifecycle(ctx, storeQuery)
	if err != nil {
		return protocol.SessionList{}, err
	}
	searchPage := protocol.SessionList{}
	if searchQuery != "" {
		searchStoreQuery := query
		searchStoreQuery.Status = ""
		searchStoreQuery.Limit = 1000
		searchPage, err = r.sessionLifecycle.ListLifecycle(ctx, searchStoreQuery)
		if err != nil {
			return protocol.SessionList{}, err
		}
	}
	candidates, byThread, err := r.projectSessionActivities(ctx, page.Sessions)
	if err != nil {
		return protocol.SessionList{}, err
	}
	eventMatches := []protocol.SessionSearchMatch(nil)
	if searchQuery != "" {
		eventMatches, err = r.searchSessionEvents(ctx, candidates, byThread, searchQuery)
		if err != nil {
			return protocol.SessionList{}, err
		}
	}
	matched := make(map[string]struct{}, len(searchPage.Sessions)+len(eventMatches))
	for _, value := range searchPage.Sessions {
		matched[value.SessionID] = struct{}{}
	}
	for _, match := range eventMatches {
		matched[match.SessionID] = struct{}{}
	}
	sessions := make([]protocol.SessionSummary, 0, min(len(candidates), limit))
	included := make(map[string]struct{}, len(candidates))
	for _, value := range candidates {
		if searchQuery != "" {
			if _, ok := matched[value.SessionID]; !ok {
				continue
			}
		}
		if query.Status != "" && value.Status != query.Status {
			continue
		}
		sessions = append(sessions, value)
		included[value.SessionID] = struct{}{}
		if len(sessions) == limit {
			break
		}
	}
	matchesBySession := make(map[string]protocol.SessionSearchMatch, len(eventMatches))
	for _, match := range eventMatches {
		matchesBySession[match.SessionID] = match
	}
	for _, match := range searchPage.Matches {
		if _, exists := matchesBySession[match.SessionID]; !exists {
			if match.Snippet == "" {
				match.Snippet = searchQuery
			}
			matchesBySession[match.SessionID] = match
		}
	}
	matches := make([]protocol.SessionSearchMatch, 0, len(matchesBySession))
	for _, value := range sessions {
		if match, ok := matchesBySession[value.SessionID]; ok {
			if _, ok := included[match.SessionID]; ok {
				matches = append(matches, match)
			}
		}
	}
	result := protocol.SessionList{
		Version:  protocol.SessionLifecycleVersion,
		Query:    searchQuery,
		Sessions: sessions,
		Matches:  matches,
	}
	if err := result.Validate(); err != nil {
		return protocol.SessionList{}, err
	}
	return result, nil
}

func (r *SessionService) searchSessionEvents(
	ctx context.Context,
	sessions []protocol.SessionSummary,
	byThread map[protocol.ThreadID]string,
	query string,
) ([]protocol.SessionSearchMatch, error) {
	matches := make(map[string]protocol.SessionSearchMatch, len(sessions))
	if reader, ok := r.events.(history.SearchReader); ok {
		indexed, err := reader.SearchSessionEvents(ctx, byThread, query)
		if err != nil {
			return nil, err
		}
		for _, match := range indexed {
			matches[match.SessionID] = match
		}
	} else {
		events, err := r.events.Replay(ctx, 0)
		var gap *CursorGapError
		if errors.As(err, &gap) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			sessionID := byThread[event.ThreadID]
			if sessionID == "" {
				continue
			}
			kind, snippet, ok := history.MatchSearchFields(history.SearchFields(event), query)
			if !ok {
				continue
			}
			matches[sessionID] = protocol.SessionSearchMatch{
				SessionID: sessionID, TurnID: event.TurnID,
				Kind: kind, Snippet: snippet,
			}
		}
	}
	result := make([]protocol.SessionSearchMatch, 0, len(sessions))
	for _, summary := range sessions {
		if match, ok := matches[summary.SessionID]; ok {
			result = append(result, match)
			continue
		}
		if snippet, ok := history.SearchSnippet(summary.Title, query); ok &&
			summary.LatestTurnID != "" {
			result = append(result, protocol.SessionSearchMatch{
				SessionID: summary.SessionID,
				TurnID:    summary.LatestTurnID,
				Kind:      "title",
				Snippet:   snippet,
			})
		}
	}
	return result, nil
}

func (r *SessionService) SessionStatus(
	ctx context.Context,
	sessionID string,
) (protocol.SessionSummary, error) {
	if r.sessionLifecycle == nil {
		return protocol.SessionSummary{}, runtimeProblem(protocol.CodeUnavailable, "session lifecycle is unavailable", nil)
	}
	summary, err := r.sessionLifecycle.GetLifecycle(ctx, sessionID)
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	if r.workspaceRoot != "" &&
		!sameWorkspaceRoot(r.workspaceRoot, summary.WorkspaceRoot) {
		return protocol.SessionSummary{}, runtimeProblem(
			protocol.CodeConflict,
			"session does not belong to this Runtime workspace",
			nil,
		)
	}
	return r.projectSessionActivity(ctx, summary)
}

func (r *SessionService) projectSessionActivity(
	ctx context.Context,
	summary protocol.SessionSummary,
) (protocol.SessionSummary, error) {
	summaries, _, err := r.projectSessionActivities(ctx, []protocol.SessionSummary{summary})
	if err != nil {
		return protocol.SessionSummary{}, err
	}
	return summaries[0], nil
}

func (r *SessionService) projectSessionLiveActivity(
	summary protocol.SessionSummary,
	threadIDs []protocol.ThreadID,
) protocol.SessionSummary {
	threads := make(map[protocol.ThreadID]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		threads[threadID] = struct{}{}
	}
	active := false
	for threadID := range threads {
		if _, ok := r.active.LookupThread(threadID); ok {
			active = true
			break
		}
	}
	pendingOperation := r.OperationService.hasPendingSession(summary.SessionID)
	r.EventService.mu.Lock()
	pendingApprovals := 0
	for _, approval := range r.approvals {
		if _, ok := threads[approval.ThreadID]; ok {
			pendingApprovals++
		}
	}
	pendingInputs := 0
	for _, input := range r.inputs {
		if _, ok := threads[input.ThreadID]; ok {
			pendingInputs++
		}
	}
	r.EventService.mu.Unlock()
	summary.PendingApprovals = pendingApprovals
	summary.PendingInputs = pendingInputs
	switch {
	case pendingApprovals > 0:
		summary.Status = protocol.SessionStatusAwaitingApproval
	case pendingInputs > 0:
		summary.Status = protocol.SessionStatusAwaitingInput
	case active, pendingOperation:
		summary.Status = protocol.SessionStatusRunning
	}
	return summary
}

func ensureSessionQuiescent(
	summary protocol.SessionSummary,
	action string,
) error {
	switch summary.Status {
	case protocol.SessionStatusRunning,
		protocol.SessionStatusAwaitingApproval,
		protocol.SessionStatusAwaitingInput:
		return sessionBusyProblem(
			fmt.Sprintf(
				"cannot %s session while status is %s",
				action,
				summary.Status,
			),
			summary,
		)
	default:
		return nil
	}
}
