package thread

import (
	"context"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// IsAgentThread uses the durable agent graph for thread provenance. ParentID
// alone also describes checkpoint forks; titles and thread names are mutable
// display values. Terminal agents remain delegated after their Engine closes.
func (r *Repository) IsAgentThread(ctx context.Context, sessionID string, threadID protocol.ThreadID) (bool, error) {
	var delegated bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM agent_nodes WHERE session_id = ? AND thread_id = ?
	)`, sessionID, threadID).Scan(&delegated)
	return delegated, err
}
