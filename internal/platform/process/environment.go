package process

import (
	"errors"
	"os"

	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Environment is an immutable selection from a prepared policy. It carries
// values, not filesystem or network authority, and never captures host state.
type Environment struct {
	values []string
}

func EnvironmentFromPolicy(policy sandbox.Policy) (*Environment, error) {
	for _, values := range [][]string{policy.Toolchains.Environment, policy.EnvironmentValues} {
		if err := envpolicy.ValidatePreparedEnvironment(values); err != nil {
			return nil, err
		}
	}
	values, err := envpolicy.Merge(policy.Toolchains.Environment, policy.EnvironmentValues)
	if err != nil {
		return nil, err
	}
	if policy.PrivateTemp != "" {
		if policy.EnvironmentProfile == "isolated" {
			values = setEnvironmentValue(values, "HOME", policy.PrivateTemp)
		}
		if !policy.SharedUserTemp {
			for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
				values = setEnvironmentValue(values, name, policy.PrivateTemp)
			}
		}
	}
	return &Environment{values: values}, nil
}

// BoundEnvironment is the environment material covered by a managed process
// lease. Per-command declarations retain their usual restrictions, including
// the prohibition on overriding policy-owned HOME and temporary directories.
func (o Options) BoundEnvironment() ([]string, error) {
	if err := ValidateDeclaredEnvironment(o.Env); err != nil {
		return nil, err
	}
	if o.Environment == nil {
		return o.Env, nil
	}
	if o.Sandbox != nil || o.TrustedRuntimeHelper {
		return nil, errors.New("prepared process environment cannot replace a sandbox or runtime helper environment")
	}
	return envpolicy.Merge(o.Environment.values, o.Env)
}

// SanitizedEnvironment captures settings for trusted, unsandboxed helpers.
// Controlled processes use the environment already bound to their policy.
func SanitizedEnvironment(extra []string) ([]string, error) {
	if err := ValidateDeclaredEnvironment(extra); err != nil {
		return nil, err
	}
	return envpolicy.Merge(envpolicy.Baseline(os.Environ()), extra)
}

func ValidateDeclaredEnvironment(extra []string) error {
	return envpolicy.ValidateDeclaredEnvironment(extra)
}

func SecretEnvironmentName(name string) bool {
	return envpolicy.SecretEnvironmentName(name)
}
