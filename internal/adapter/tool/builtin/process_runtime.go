package builtin

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
)

func NewWorkspaceBroker(
	workspace string,
	leaseAuthority *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*workspacebroker.Runtime, error) {
	return workspacebroker.New(workspace, leaseAuthority, leaseTTL)
}

func NewWorkspaceQuery(
	workspace string,
	backend sandbox.Backend,
	leaseAuthority *authority.LeaseAuthority,
	leaseTTL time.Duration,
) (*workspacequery.Service, error) {
	brokers, err := NewWorkspaceBroker(
		workspace, leaseAuthority, leaseTTL,
	)
	if err != nil {
		return nil, err
	}
	return workspacequery.New(workspace, backend, brokers.VCS)
}
