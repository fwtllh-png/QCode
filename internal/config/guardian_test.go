package config

import (
	"testing"
	"time"
)

func TestGuardianTrustedConfigurationAndJudgeRoute(t *testing.T) {
	path := writeConfig(t, `[security.guardian]
enabled = true
timeout = "7s"
max_output_tokens = 512
[route.judge]
provider = "openai"
model = "gpt-4.1"
`)
	for _, trusted := range []bool{false, true} {
		snapshot, err := Load(LoadOptions{RepoPath: path, TrustRepo: trusted})
		if err != nil {
			t.Fatal(err)
		}
		g := snapshot.Config.Security.Guardian
		if !trusted {
			if g.Enabled || g.Timeout != 0 || g.MaxOutputTokens != 0 || len(snapshot.Config.Route.Slots) != 0 {
				t.Fatal("repository enabled Guardian or changed judge")
			}
			continue
		}
		if !g.Enabled || g.Timeout != 7*time.Second || g.MaxOutputTokens != 512 || snapshot.Config.Route.Slots["judge"].Provider != "openai" {
			t.Fatalf("config=%+v", snapshot.Config)
		}
		for _, field := range []string{fieldGuardianEnabled, fieldGuardianTimeout, fieldGuardianMaxOutput, fieldRouteProvider("judge"), fieldRouteModel("judge")} {
			if snapshot.Provenance[field] != SourceRepo {
				t.Fatalf("provenance %s", field)
			}
		}
	}
	snapshot, err := Load(LoadOptions{Path: path})
	if err != nil || !snapshot.Config.Security.Guardian.Enabled {
		t.Fatal("operator config ignored", err)
	}
	defaults := Defaults().Security.Guardian
	if defaults.Enabled || defaults.Timeout != 0 || defaults.MaxOutputTokens != 0 {
		t.Fatal("hidden Guardian defaults")
	}
}

func TestGuardianConfigurationRejectsInvalidAndObsoleteFields(t *testing.T) {
	for _, body := range []string{`enabled = true`, `timeout = "-1s"`, `timeout = "invalid"`, `max_output_tokens = -1`, `model = "judge"`, `cache_ttl = "1m"`, `max_consecutive_denials = 3`, `denial_window = "1m"`} {
		t.Run(body, func(t *testing.T) {
			if _, err := Load(LoadOptions{Path: writeConfig(t, "[security.guardian]\n"+body)}); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
