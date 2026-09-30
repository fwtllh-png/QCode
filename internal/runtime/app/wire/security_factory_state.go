package wire

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/common/environment"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	securitypolicy "github.com/fwtllh-png/QCode/internal/security/policy"
)

type guardFactory struct {
	registry               *tool.Registry
	runtime                *securitypolicy.Runtime
	workspace, workspaceID string
	journal                *workspacejournal.Manager
	readTracker            *workspacejournal.ReadTracker
	diagnostics            verify.DiagnosticRunner
	permissions            *securitypolicy.PermissionsStore
	onNetworkAllow         toolguard.NetworkAllow
	leaseAuthority         *toolguard.LeaseAuthority
	leaseTTL               time.Duration
	approvalTTL            time.Duration
	now                    func() time.Time
	isolator               tool.Isolator
	preparationFacts       []environment.Fact
}

func newLeaseAuthority() *toolguard.LeaseAuthority {
	return toolguard.NewLeaseAuthority()
}
