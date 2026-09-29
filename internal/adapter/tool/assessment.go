package tool

import (
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

// AssessResources resolves adapter declarations into the immutable security
// snapshot. Guard calls it after argument and resource resolution; policy,
// approval, grants and authority only consume the returned assessment.
func AssessResources(binding TrustedBinding, declared securitymodel.Declared, resources []Resource) securitymodel.Assessment {
	resolved := make([]securitymodel.Resource, 0, len(resources))
	for _, item := range resources {
		if typed, ok := item.Security(); ok {
			resolved = append(resolved, typed)
		}
	}
	var fixed *securitymodel.Effect
	if binding.Effect.Mode == EffectFixed {
		fixed = &securitymodel.Effect{
			Kind: binding.Effect.Kind, Risk: binding.Effect.Risk,
			Reversibility: binding.Effect.Reversibility,
		}
	}
	return securitymodel.Assess(securitymodel.AssessmentInput{
		Binding: securitymodel.AssessmentBinding{
			Capability:      binding.Capability,
			Access:          binding.AccessMode,
			SandboxDeclared: binding.SandboxRequirement != "",
			StrongSandbox:   binding.SandboxRequirement == SandboxStrong,
			Journaled:       binding.Journaled(),
			Fixed:           fixed,
			PlanningExempt:  binding.Effect.Planning == PlanningExempt,
		},
		Declared:  declared,
		Resources: resolved,
	})
}
