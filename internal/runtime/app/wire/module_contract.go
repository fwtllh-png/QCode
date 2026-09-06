package wire

import (
	"fmt"
)

// buildDomain names one ownership area of buildState. The module sequence is
// an explicit dependency contract: every module declares the domains it owns
// (writes) and consumes (reads), and validateModuleContracts fails before any
// module runs when the declared order is inconsistent — a domain read before
// its owning module ran, or two modules claiming the same domain.
//
// options is an immutable input and session is the shared output accumulator;
// neither participates in ordering.
type buildDomain string

const (
	domainConfig        buildDomain = "config"
	domainProvider      buildDomain = "provider"
	domainPersistence   buildDomain = "persistence"
	domainPlatform      buildDomain = "platform"
	domainTools         buildDomain = "tools"
	domainCapabilities  buildDomain = "capabilities"
	domainSecurity      buildDomain = "security"
	domainOrchestration buildDomain = "orchestration"
	domainAgent         buildDomain = "agent"
	domainRuntime       buildDomain = "runtime"
)

// ModuleContract declares the buildState domains one module writes and reads.
// Writes are exclusive: exactly one module owns each domain. Reads must be
// satisfied by a module earlier in the sequence.
type ModuleContract struct {
	Writes []buildDomain
	Reads  []buildDomain
}

func validateModuleContracts(modules []buildModule) error {
	owners := make(map[buildDomain]string)
	for _, module := range modules {
		contract := module.Contract()
		for _, domain := range contract.Writes {
			if owner, exists := owners[domain]; exists {
				return fmt.Errorf(
					"module %s writes domain %s already owned by %s",
					module.Name(), domain, owner,
				)
			}
			owners[domain] = module.Name()
		}
		for _, domain := range contract.Reads {
			if _, exists := owners[domain]; !exists {
				return fmt.Errorf(
					"module %s reads domain %s before any module writes it",
					module.Name(), domain,
				)
			}
		}
	}
	return nil
}

// The contract declarations below are the single place where the module
// sequence's data dependencies are explicit. They must stay in sync with what
// each Build actually touches; reordering defaultBuildModules in a way that
// violates a declared dependency fails construction with the module and
// domain named.

func (configModule) Contract() ModuleContract {
	return ModuleContract{Writes: []buildDomain{domainConfig}}
}

func (providerModule) Contract() ModuleContract {
	return ModuleContract{Writes: []buildDomain{domainProvider}, Reads: []buildDomain{domainConfig}}
}

func (persistenceModule) Contract() ModuleContract {
	return ModuleContract{Writes: []buildDomain{domainPersistence}, Reads: []buildDomain{domainConfig}}
}

func (platformModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainPlatform},
		Reads:  []buildDomain{domainConfig, domainPersistence, domainProvider},
	}
}

func (builtinToolsModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainTools},
		Reads:  []buildDomain{domainConfig, domainPersistence, domainPlatform},
	}
}

func (capabilityToolsModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainCapabilities},
		Reads:  []buildDomain{domainConfig, domainPlatform, domainTools},
	}
}

func (securityModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainSecurity},
		Reads:  []buildDomain{domainConfig, domainPlatform, domainProvider, domainTools},
	}
}

func (orchestrationModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainOrchestration},
		Reads:  []buildDomain{domainConfig, domainPersistence, domainPlatform, domainSecurity},
	}
}

func (observabilityModule) Contract() ModuleContract {
	return ModuleContract{}
}

func (agentModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainAgent},
		Reads: []buildDomain{
			domainConfig, domainProvider, domainPlatform, domainTools,
			domainCapabilities, domainSecurity, domainOrchestration,
		},
	}
}

func (runtimeModule) Contract() ModuleContract {
	return ModuleContract{
		Writes: []buildDomain{domainRuntime},
		Reads: []buildDomain{
			domainConfig, domainTools, domainCapabilities, domainSecurity,
			domainOrchestration, domainAgent,
		},
	}
}

func (backgroundModule) Contract() ModuleContract {
	return ModuleContract{Reads: []buildDomain{domainCapabilities, domainRuntime}}
}
