package policy

import (
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

type Surface string

const (
	SurfaceSandbox Surface = "sandbox"
	SurfaceRules   Surface = "rules"
	SurfaceSkills  Surface = "skills"
	SurfaceMCP     Surface = "mcp"
)

type SurfacePosture string

const (
	SurfaceInherit SurfacePosture = ""
	SurfaceAsk     SurfacePosture = "ask"
	SurfaceAllow   SurfacePosture = "allow"
	SurfaceDeny    SurfacePosture = "deny"
)

type Granular struct {
	Sandbox SurfacePosture `json:"sandbox,omitempty"`
	Rules   SurfacePosture `json:"rules,omitempty"`
	Skills  SurfacePosture `json:"skills,omitempty"`
	MCP     SurfacePosture `json:"mcp,omitempty"`
}

// ClassifySurface derives the granular surface from the catalog source kind
// the Registry assigned to the tool binding.
func ClassifySurface(source securitymodel.SourceKind, capability Capability) Surface {
	switch {
	case source == securitymodel.SourceMCP:
		return SurfaceMCP
	case source == securitymodel.SourceSkill:
		return SurfaceSkills
	case capability == CapabilityProcess:
		return SurfaceSandbox
	default:
		return SurfaceRules
	}
}

func (g Granular) postureFor(surface Surface) SurfacePosture {
	return map[Surface]SurfacePosture{
		SurfaceSandbox: g.Sandbox, SurfaceRules: g.Rules,
		SurfaceSkills: g.Skills, SurfaceMCP: g.MCP,
	}[surface]
}

func ApplySurfaceTightening(
	decision Decision, surface Surface, granular Granular, eff securitymodel.Effect,
) Decision {
	posture := granular.postureFor(surface)
	if posture == SurfaceInherit || posture == SurfaceAllow ||
		decision.Action == ActionDeny || decision.Action == ActionHold {
		return decision
	}
	if posture == SurfaceAsk && decision.Action == ActionAllow {
		// Surface ask tightens consequential work only. Read-only
		// low-risk probes (echo, ls, env) keep their frictionless allow so
		// a tightened session does not turn exploration commands into
		// approval stops. Explicit deny postures are never softened.
		if eff.Kind == securitymodel.ProcessReadOnly && eff.Risk == securitymodel.RiskLow {
			return decision
		}
		return Decision{Action: ActionAsk, Code: "granular_ask", Reason: "surface " + string(surface) + " requires approval"}
	}
	if posture == SurfaceDeny {
		return Decision{Action: ActionDeny, Code: "granular_deny", Reason: "surface " + string(surface) + " denies"}
	}
	return decision
}
