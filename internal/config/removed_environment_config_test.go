package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestRemovedAuthServicesRejectedAtEveryTrustLevel(t *testing.T) {
	for _, declaration := range []string{
		"[execution.environment]\nauth_services = []\n",
		"[[execution.environment.auth_services]]\nprotocol = \"goproxy\"\ncredential = { kind = \"env\", name = \"FIXTURE_UNPRINTED_VALUE\" }\n",
	} {
		path := writeConfig(t, declaration)
		for _, options := range []LoadOptions{
			{Path: path}, {RepoPath: path}, {RepoPath: path, TrustRepo: true},
		} {
			options.LookupEnv = envLookup(nil)
			_, err := Load(options)
			var unknown *toml.StrictMissingError
			if !errors.As(err, &unknown) {
				t.Fatalf("removed configuration must fail strict decoding: %v", err)
			}
			if !strings.Contains(err.Error(), "execution.environment.auth_services") ||
				strings.Contains(err.Error(), "FIXTURE_UNPRINTED_VALUE") {
				t.Fatalf("diagnostic must identify removed fields without printing values: %v", err)
			}
		}
	}
}
