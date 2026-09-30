package builtin

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
)

func NewWorkspaceBroker(
	workspace string,
	leaseAuthority *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*workspacebroker.Runtime, error) {
	return workspacebroker.New(workspace, leaseAuthority, leaseTTL)
}
