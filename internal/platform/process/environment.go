package process

import (
	"os"

	"github.com/fwtllh-png/QCode/internal/security/envpolicy"
)

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
