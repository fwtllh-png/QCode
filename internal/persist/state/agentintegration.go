package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *Store) LoadAgentIntegration(
	ctx context.Context, workspaceRoot, agentID, previewDigest string,
) (subagent.IntegrationCandidate, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return subagent.IntegrationCandidate{}, false, ErrClosed
	}
	return loadAgentIntegrationRow(
		ctx, s.sqlite.DB(), workspaceRoot, "", agentID, previewDigest,
	)
}

func (s *Store) LoadAgentIntegrationSession(
	ctx context.Context, workspaceRoot, sessionID, agentID, previewDigest string,
) (subagent.IntegrationCandidate, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return subagent.IntegrationCandidate{}, false, ErrClosed
	}
	return loadAgentIntegrationRow(
		ctx, s.sqlite.DB(), workspaceRoot, sessionID, agentID, previewDigest,
	)
}

func loadAgentIntegrationRow(
	ctx context.Context,
	db *sql.DB,
	workspaceRoot, sessionID, agentID, previewDigest string,
) (subagent.IntegrationCandidate, bool, error) {
	query := `SELECT candidate_json FROM agent_integrations
		WHERE workspace_root = ? AND agent_id = ? AND preview_digest = ?`
	args := []any{workspaceRoot, agentID, previewDigest}
	if sessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	var raw []byte
	err := db.QueryRowContext(ctx, query, args...).Scan(&raw)
	if err == sql.ErrNoRows {
		return subagent.IntegrationCandidate{}, false, nil
	}
	if err != nil {
		return subagent.IntegrationCandidate{}, false, err
	}
	var candidate subagent.IntegrationCandidate
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return subagent.IntegrationCandidate{}, false, err
	}
	return candidate, true, nil
}

func (s *Store) ListAgentIntegrationRecoveries(
	ctx context.Context,
	workspaceRoot, sessionID string,
) ([]subagent.IntegrationRecovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	rows, err := s.sqlite.DB().QueryContext(ctx, `
		SELECT n.status, n.revision, i.candidate_json
		FROM agent_nodes n
		JOIN agent_integrations i
		  ON i.workspace_root = n.workspace_root
		 AND i.session_id = n.session_id AND i.agent_id = n.agent_id
		WHERE n.workspace_root = ? AND n.session_id = ?
		  AND (n.status = 'integrating' OR i.status = 'applying')
		ORDER BY i.updated_at DESC`, workspaceRoot, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]struct{})
	var out []subagent.IntegrationRecovery
	for rows.Next() {
		var status string
		var revision uint64
		var raw []byte
		if err := rows.Scan(&status, &revision, &raw); err != nil {
			return nil, err
		}
		var candidate subagent.IntegrationCandidate
		if err := json.Unmarshal(raw, &candidate); err != nil {
			return nil, err
		}
		if _, ok := seen[candidate.AgentID]; ok {
			continue
		}
		seen[candidate.AgentID] = struct{}{}
		out = append(out, subagent.IntegrationRecovery{
			Candidate: candidate, AgentStatus: subagent.Status(status),
			AgentRevision: revision,
		})
	}
	return out, rows.Err()
}

func projectAgentIntegrationTx(
	ctx context.Context,
	tx *sql.Tx,
	event protocol.Event,
	data *protocol.AgentIntegrationData,
) error {
	var candidate subagent.IntegrationCandidate
	if err := json.Unmarshal(data.Detail, &candidate); err != nil {
		return fmt.Errorf("decode agent integration detail: %w", err)
	}
	if candidate.AgentID != data.AgentID ||
		candidate.SessionID != data.SessionID ||
		candidate.PreviewDigest != data.PreviewDigest ||
		string(candidate.Status) != data.Status ||
		candidate.Revision == 0 {
		return fmt.Errorf("agent integration detail mismatch for %s", data.AgentID)
	}
	var currentRevision uint64
	var currentStatus string
	var sourceSequence uint64
	err := tx.QueryRowContext(ctx, `
		SELECT revision, status, source_sequence
		FROM agent_integrations
		WHERE workspace_root = ? AND session_id = ? AND agent_id = ?
		  AND preview_digest = ?`,
		data.WorkspaceRoot, data.SessionID, data.AgentID, data.PreviewDigest,
	).Scan(&currentRevision, &currentStatus, &sourceSequence)
	switch {
	case err == sql.ErrNoRows:
		if candidate.Revision != 1 ||
			candidate.Status != subagent.IntegrationPreviewed {
			return fmt.Errorf("integration candidate must start previewed")
		}
	case err != nil:
		return err
	case uint64(event.Sequence) <= sourceSequence:
		return nil
	case candidate.Revision != currentRevision+1:
		return fmt.Errorf(
			"integration %s revision conflict: expected %d got %d",
			data.PreviewDigest, currentRevision+1, candidate.Revision,
		)
	case !validIntegrationTransition(
		subagent.IntegrationStatus(currentStatus), candidate.Status,
	):
		return fmt.Errorf(
			"integration %s cannot transition from %s to %s",
			data.PreviewDigest, currentStatus, candidate.Status,
		)
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_integrations(
			workspace_root, session_id, agent_id, preview_digest, status, revision,
			candidate_json, source_sequence, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_root, session_id, agent_id, preview_digest) DO UPDATE SET
			status=excluded.status, revision=excluded.revision,
			candidate_json=excluded.candidate_json,
			source_sequence=excluded.source_sequence,
			updated_at=excluded.updated_at`,
		data.WorkspaceRoot, data.SessionID, data.AgentID,
		data.PreviewDigest, data.Status,
		candidate.Revision, raw, int64(event.Sequence), timestamp(event.CreatedAt),
	)
	return err
}

func validIntegrationTransition(
	from, to subagent.IntegrationStatus,
) bool {
	switch from {
	case subagent.IntegrationPreviewed:
		return to == subagent.IntegrationApplying ||
			to == subagent.IntegrationDiscarded
	case subagent.IntegrationApplying:
		return to == subagent.IntegrationApplied ||
			to == subagent.IntegrationFailed
	default:
		return false
	}
}
