package policy

import (
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type EffectKind string

const (
	EffectWorkspaceRead    EffectKind = "workspace.read"
	EffectWorkspaceEdit    EffectKind = "workspace.edit"
	EffectProcessReadOnly  EffectKind = "process.read_only"
	EffectProcessMutating  EffectKind = "process.mutating"
	EffectNetworkRead      EffectKind = "network.read"
	EffectNetworkMutating  EffectKind = "network.mutating"
	EffectSessionMutation  EffectKind = "session.mutation"
	EffectAgentMessage     EffectKind = "agent.message"
	EffectAgentLifecycle   EffectKind = "agent.lifecycle"
	EffectExternalMutation EffectKind = "external.mutation"
)

type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

type Effect struct {
	Kind          EffectKind
	Risk          RiskLevel
	Reversibility string
}

func NormalizeEffect(invocation Invocation) Effect {
	if readOnlySpawn(invocation) {
		return effect(EffectAgentLifecycle, RiskLow, "reversible")
	}
	if invocation.Effect.Mode == tool.EffectFixed {
		return effect(
			EffectKind(invocation.Effect.Kind),
			RiskLevel(invocation.Effect.Risk),
			string(invocation.Effect.Reversibility),
		)
	}
	if invocation.Access == "" || invocation.Sandbox == "" {
		switch invocation.Capability {
		case tool.CapabilityRead:
			return effect(EffectWorkspaceRead, RiskLow, "reversible")
		case tool.CapabilityWrite:
			return effect(EffectExternalMutation, RiskMedium, "bounded")
		case tool.CapabilityProcess, tool.CapabilityNetwork, tool.CapabilityExternal:
			return effect(EffectExternalMutation, RiskHigh, "irreversible")
		default:
			return effect(EffectExternalMutation, RiskCritical, "irreversible")
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
		return effect(EffectWorkspaceRead, RiskLow, "reversible")
	case resources == 16 && invocation.Capability == tool.CapabilityWrite:
		return effect(EffectSessionMutation, RiskLow, "reversible")
	case resources&8 != 0:
		return effect(EffectAgentLifecycle, RiskHigh, "bounded")
	case strongProcessUsesOnlyLoopback(invocation):
		// Local fixture servers stay inside the Strong Sandbox boundary. Treat
		// their exact localhost grant like bounded network read so auto posture
		// can review it without repeatedly asking for human approval.
		return effect(EffectNetworkRead, RiskMedium, "bounded")
	case invocation.Capability == tool.CapabilityNetwork || resources&4 != 0:
		if invocation.Access == tool.AccessRead && resources&1 == 0 &&
			!processEgressCanWrite(invocation) {
			return effect(EffectNetworkRead, RiskMedium, "bounded")
		}
		return effect(EffectNetworkMutating, RiskHigh, "irreversible")
	case invocation.Capability == tool.CapabilityProcess || resources&2 != 0:
		if invocation.Sandbox == tool.SandboxStrong && resources&1 == 0 {
			return effect(EffectProcessReadOnly, RiskLow, "reversible")
		}
		return effect(EffectProcessMutating, RiskHigh, "bounded")
	case invocation.Capability == tool.CapabilityWrite && resources&1 != 0 &&
		invocation.Journaled:
		return effect(EffectWorkspaceEdit, RiskLow, "reversible")
	default:
		return effect(EffectExternalMutation, RiskHigh, "irreversible")
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
			if resource.Protocol != "loopback" ||
				resource.ID != "localhost" ||
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
			if protocol == "loopback" {
				continue
			}
		case "url":
			target, ok := ParseNetworkTarget(resource.ID)
			if !ok {
				return true
			}
			protocol = target.Protocol
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

func effect(kind EffectKind, risk RiskLevel, reversibility string) Effect {
	return Effect{kind, risk, reversibility}
}
