package wire

import (
	"context"

	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func (f guardFactory) Build(context.Context) (*toolguard.Guard, error) {
	options := toolguard.Options{
		Registry: f.registry, Policy: f.runtime,

		ForceEditPlanApproval: f.forceEditReview, Now: f.now, Diagnostics: f.diagnostics, OnNetworkAllow: f.onNetworkAllow, Workspace: f.workspace, WorkspaceID: f.workspaceID, WorkspaceGeneration: 1, LeaseAuthority: f.leaseAuthority, LeaseTTL: f.leaseTTL, ApprovalTTL: f.approvalTTL,
		ReadTracker: f.readTracker, Journal: f.journal, Isolator: f.isolator,
		ModuleProxy: f.moduleProxy, AuthBindReport: f.authBindReport,
	}
	if f.permissions != nil {
		f.runtime.BindUserRuleSource(f.permissions)
		options.PersistAllow = func(invocation policy.Invocation) error {
			_, err := f.permissions.AppendAllow(invocation)
			return err
		}
	}
	return toolguard.New(options)
}

func bindEngineGuardFactory(
	options *agentengine.Options,
	base guardFactory,
	observe toolguard.ApprovalObserver,
) {
	if base.runtime == nil {
		options.GuardFactory = nil
		return
	}
	factory := base
	factory.registry = options.Tools
	factory.runtime = options.Security
	factory.workspace = options.Workspace
	factory.journal = options.Journal
	factory.readTracker = options.ReadTracker
	factory.diagnostics = options.Diagnostics
	factory.onNetworkAllow = options.OnNetworkAllow
	factory.now = options.Observability.Now
	if options.Workspace != base.workspace {
		factory.permissions, factory.workspaceID = nil, ""
		factory.isolator = nil
	}
	options.Guard = nil
	options.GuardFactory = func(ctx context.Context) (*toolguard.Guard, error) {
		guard, err := factory.Build(ctx)
		if err != nil {
			return nil, err
		}
		guard.SetApprovalObserver(observe)
		return guard, nil
	}
}
