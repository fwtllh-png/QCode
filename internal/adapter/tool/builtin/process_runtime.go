package builtin

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func NewWorkspaceBroker(
	workspace string,
	leaseAuthority *authority.LeaseAuthority,
	leaseTTL time.Duration,
	backend sandbox.Backend,
) (*workspacebroker.Runtime, error) {
	return workspacebroker.New(workspace, leaseAuthority, leaseTTL, backend)
}
