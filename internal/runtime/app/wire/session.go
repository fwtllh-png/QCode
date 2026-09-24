package wire

import (
	"context"
	"errors"
	"os"

	"github.com/fwtllh-png/QCode/internal/adapter/mcp"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/policy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type sessionConfiguration struct {
	snapshot config.Snapshot
	profile  protocol.SessionProfile
}

// RegisterResource adds a construction-time resource to the session's close
// stack. Modules that build long-lived host processes — a resident
// language-server pool, for one — register them here so session teardown
// retires them with everything else, in reverse order.
func (s *Session) RegisterResource(
	name string, close func(context.Context) error,
) error {
	if s.resources == nil {
		return errors.New("session resources are not initialized")
	}
	return s.resources.Add(name, close)
}

func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.resources != nil {
			s.closeErr = s.resources.Close(ctx)
		}
	})
	return s.closeErr
}

// ProviderID and ModelID report the immutable route selected while the
// long-lived runtime was bootstrapped. Transports may expose this route for
// discovery, but changing it requires constructing a new runtime.
func (s *Session) ProviderID() string { return s.providerID }

func (s *Session) ModelID() string { return s.modelID }

func (s *Session) ModelCapabilities() protocol.ModelCapabilities {
	return s.modelCapabilities
}

func (s *Session) ProviderCatalog() protocol.ProviderCatalog {
	return s.providerCatalog
}

func (s *Session) ModelCatalog() protocol.ModelCatalog {
	return s.modelCatalog
}

// DefaultProfile is the immutable Runtime profile derived from the resolved
// configuration. Hosts may validate transport capabilities against it, but do
// not own or replace its defaults.
func (s *Session) DefaultProfile() protocol.SessionProfile {
	if s == nil {
		return protocol.SessionProfile{}
	}
	profile := s.configuration.profile
	profile.EnabledToolIDs = append([]string(nil), profile.EnabledToolIDs...)
	return profile
}

// ConfigSnapshot reports the resolved configuration and field provenance used
// to construct this Runtime.
func (s *Session) ConfigSnapshot() config.Snapshot {
	if s == nil {
		return config.Snapshot{}
	}
	return config.CloneSnapshot(s.configuration.snapshot)
}

func (s *Session) SessionWorkspaces() app.SessionWorkspaceManager {
	if s == nil {
		return nil
	}
	return s.chatWorkspaces
}

// JournalRecovery reports what interrupted turns this process found at startup
// and what it did with them. It is empty in the normal case and for hosts running
// without a durable journal.
func (s *Session) JournalRecovery() workspacejournal.Recovery {
	if s == nil {
		return workspacejournal.Recovery{}
	}
	return s.journalRecovery
}

// Subagents reports the child-agent control plane, or nil when this host runs without
// tools. A host needs it to show what child agents exist: the parent's thread
// history says a child was spawned, not what became of it.
func (s *Session) Subagents() *subagent.AgentControl {
	if s == nil {
		return nil
	}
	return s.subagents
}

func (s *Session) Security() *policy.Runtime { return s.security }

func (s *Session) Processes() *process.SessionManager { return s.processes }

func (s *Session) WorkspaceQuery() *workspacequery.Service { return s.workspaceQuery }

func (s *Session) RepositoryIndex() *repoindex.Index { return s.repositoryIndex }

func (s *Session) MCPHealth() []mcp.HealthSnapshot {
	if s == nil || s.mcpPool == nil {
		return nil
	}
	return s.mcpPool.HealthSnapshots()
}

func (s *Session) SetPermission(permission policy.Permission) {
	// In-flight turns use a frozen policy; changes apply to the next turn.
	if s != nil && s.security != nil {
		s.security.SetPermission(permission)
		if s.threads != nil {
			s.threads.SetPermission(permission)
		}
	}
}

func (s *Session) SetGranular(granular policy.Granular) {
	// Applies to the next turn's frozen policy.
	if s != nil && s.security != nil {
		s.security.SetGranular(granular)
		if s.threads != nil {
			s.threads.SetGranular(granular)
		}
	}
}

func (s *Session) registerResourceClosers() error {
	type resource struct {
		name  string
		close func(context.Context) error
	}
	// Registration is the reverse of shutdown dependency order. Reading fields
	// at close time makes the same stack valid for partial construction rollback.
	resources := []resource{
		{name: "exec-log", close: func(context.Context) error {
			if s.logFile == nil {
				return nil
			}
			return s.logFile.Close()
		}},
		{name: "provider-fixture", close: func(ctx context.Context) error {
			if s.fixture == nil {
				return nil
			}
			return s.fixture.Close(ctx)
		}},
		{name: "metrics", close: func(context.Context) error {
			if s.metricsPath == "" {
				return nil
			}
			return writeMetricSnapshot(s.metricsPath, s.metrics)
		}},
		{name: "finish-log", close: func(context.Context) error {
			if s.logger != nil {
				s.logger.Info(
					"exec finished",
					"provider", s.providerID,
					"model", s.modelID,
				)
			}
			return nil
		}},
		{name: "sandbox", close: func(context.Context) error {
			if s.sandbox == nil {
				return nil
			}
			err := sandbox.CloseBackend(s.sandbox)
			s.sandbox = nil
			return err
		}},
		{name: "ephemeral-state", close: func(context.Context) error {
			var closeErr error
			if s.ephemeralState != nil {
				closeErr = s.ephemeralState.Close()
				s.ephemeralState = nil
			}
			var cleanupErr error
			if s.ephemeralStateDir != "" {
				cleanupErr = os.RemoveAll(s.ephemeralStateDir)
				s.ephemeralStateDir = ""
			}
			return errors.Join(closeErr, cleanupErr)
		}},
		{name: "content-store", close: func(ctx context.Context) error {
			if s.content == nil {
				return nil
			}
			return s.content.Close(ctx)
		}},
		{name: "workspace-journal", close: func(ctx context.Context) error {
			if s.journal == nil {
				return nil
			}
			return s.journal.Close(ctx)
		}},
		{name: "mcp-pool", close: func(ctx context.Context) error {
			if s.mcpPool == nil {
				return nil
			}
			return s.mcpPool.ShutdownAll(ctx)
		}},
		{name: "mcp-prewarm", close: func(context.Context) error {
			if s.mcpPrewarm != nil {
				s.mcpPrewarm.Stop()
			}
			return nil
		}},
		{name: "child-toolsets", close: func(context.Context) error {
			if s.childTools != nil {
				s.childTools.closeAll()
			}
			return nil
		}},
		{name: "job-logs", close: func(context.Context) error {
			if s.jobLogs == nil {
				return nil
			}
			return s.jobLogs.Close()
		}},
		{name: "processes", close: func(context.Context) error {
			if s.processes != nil {
				return errors.Join(
					s.processes.CloseAllWithError(),
					s.processes.JournalError(),
				)
			}
			return nil
		}},
		{name: "turn-coordinators", close: func(ctx context.Context) error {
			if s.turnCoordinators == nil {
				return nil
			}
			err := s.turnCoordinators.Close(ctx)
			s.turnCoordinators = nil
			return err
		}},
		{name: "runtime", close: func(ctx context.Context) error {
			if s.Runtime == nil {
				return nil
			}
			return s.Runtime.Close(ctx)
		}},
		{name: "child-runtime", close: func(context.Context) error {
			if s.children != nil {
				s.children.close()
			}
			return nil
		}},
	}
	for _, resource := range resources {
		if err := s.resources.Add(resource.name, resource.close); err != nil {
			return err
		}
	}
	return nil
}
