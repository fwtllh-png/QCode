package policy

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/effect"
	"github.com/fwtllh-png/QCode/internal/security/netpolicy"
	securityresource "github.com/fwtllh-png/QCode/internal/security/resource"
)

func NormalizeEffect(invocation Invocation) effect.Effect {
	if readOnlySpawn(invocation) {
		return classified(effect.AgentLifecycle, effect.RiskLow, effect.Reversible)
	}
	if invocation.Effect.Mode == tool.EffectFixed {
		return classified(
			invocation.Effect.Kind, invocation.Effect.Risk,
			invocation.Effect.Reversibility,
		)
	}
	if invocation.Access == "" || invocation.Sandbox == "" {
		switch invocation.Capability {
		case tool.CapabilityRead:
			return classified(effect.WorkspaceRead, effect.RiskLow, effect.Reversible)
		case tool.CapabilityWrite:
			return classified(effect.ExternalMutation, effect.RiskMedium, effect.Bounded)
		case tool.CapabilityProcess, tool.CapabilityNetwork, tool.CapabilityExternal:
			return classified(effect.ExternalMutation, effect.RiskHigh, effect.Irreversible)
		default:
			return classified(effect.ExternalMutation, effect.RiskCritical, effect.Irreversible)
		}
	}
	var resources uint8
	masks := map[string]uint8{
		"process": 2, "host": 4, "url": 4, "agent": 8, "plan": 16,
	}
	for _, resource := range invocation.Resources {
		resources |= masks[resource.Kind]
		if (resource.Kind == "file" || resource.Kind == "directory") &&
			resource.Access != tool.AccessRead {
			resources |= 1
		}
	}
	switch {
	case invocation.Capability == tool.CapabilityRead:
		return classified(effect.WorkspaceRead, effect.RiskLow, effect.Reversible)
	case resources == 16 && invocation.Capability == tool.CapabilityWrite:
		return classified(effect.SessionMutation, effect.RiskLow, effect.Reversible)
	case resources&8 != 0:
		return classified(effect.AgentLifecycle, effect.RiskHigh, effect.Bounded)
	case strongProcessUsesOnlyLoopback(invocation):
		// Local fixture servers run inside the Strong Sandbox, but the
		// loopback grant reaches every local port; auto posture never
		// auto-reviews it (see targetsHostLocal).
		return classified(effect.NetworkRead, effect.RiskMedium, effect.Bounded)
	case invocation.Capability == tool.CapabilityNetwork || resources&4 != 0:
		if invocation.Access == tool.AccessRead && resources&1 == 0 &&
			!processEgressCanWrite(invocation) {
			return classified(effect.NetworkRead, effect.RiskMedium, effect.Bounded)
		}
		return classified(effect.NetworkMutating, effect.RiskHigh, effect.Irreversible)
	case invocation.Capability == tool.CapabilityProcess || resources&2 != 0:
		if invocation.Sandbox == tool.SandboxStrong && resources&1 == 0 {
			return classified(effect.ProcessReadOnly, effect.RiskLow, effect.Reversible)
		}
		return classified(effect.ProcessMutating, effect.RiskHigh, effect.Bounded)
	case invocation.Capability == tool.CapabilityWrite && resources&1 != 0 &&
		invocation.Journaled:
		return classified(effect.WorkspaceEdit, effect.RiskLow, effect.Reversible)
	default:
		return classified(effect.ExternalMutation, effect.RiskHigh, effect.Irreversible)
	}
}

func strongProcessUsesOnlyLoopback(invocation Invocation) bool {
	if invocation.Capability != tool.CapabilityProcess ||
		invocation.Sandbox != tool.SandboxStrong {
		return false
	}
	found := false
	for _, resource := range invocation.Resources {
		switch resource.Kind {
		case "host", "url":
			if !securityresource.IsLoopback(resource.Kind, resource.Protocol) ||
				resource.ID != securityresource.LoopbackHost ||
				!resource.AllowPrivate {
				return false
			}
			found = true
		case "file", "directory":
			if resource.Access != tool.AccessRead {
				return false
			}
		}
	}
	return found
}

// processEgressCanWrite reports whether a process network grant can carry
// data outward. The managed proxy sees only the CONNECT endpoint of an HTTPS
// tunnel and enforces methods only for plaintext HTTP, so a process target is
// a read only when it is plaintext HTTP restricted to safe methods.
func processEgressCanWrite(invocation Invocation) bool {
	if invocation.Capability != tool.CapabilityProcess {
		return false
	}
	for _, resource := range invocation.Resources {
		protocol := resource.Protocol
		switch resource.Kind {
		case "host":
			if securityresource.IsLoopback(resource.Kind, protocol) {
				continue
			}
		case "url":
			target, err := netpolicy.ParseTarget(resource.ID)
			if err != nil {
				return true
			}
			protocol = target.Scheme
		default:
			continue
		}
		if protocol != "http" || len(resource.Methods) == 0 {
			return true
		}
		for _, method := range resource.Methods {
			switch strings.ToUpper(method) {
			case "GET", "HEAD", "OPTIONS":
			default:
				return true
			}
		}
	}
	return false
}

func classified(
	kind effect.Kind, risk effect.Risk, reversibility effect.Reversibility,
) effect.Effect {
	return effect.Effect{Kind: kind, Risk: risk, Reversibility: reversibility}
}
