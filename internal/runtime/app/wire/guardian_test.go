package wire

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestGuardianConfigurationReachesProductionEngineAndKillSwitch(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "kill_switch"}[disabled], func(t *testing.T) {
			if disabled {
				t.Setenv("QCODE_DISABLE_APPROVAL_AUTO_REVIEW", "1")
			} else {
				t.Setenv("QCODE_DISABLE_APPROVAL_AUTO_REVIEW", "")
			}
			workspace := t.TempDir()
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[security.guardian]\nenabled = true\ntimeout = \"7s\"\nmax_output_tokens = 128\n"), 0600); err != nil {
				t.Fatal(err)
			}
			tools := true
			s, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{ConfigPath: path, FixturePath: subagentFixture(t, "subagent"), Permission: "auto", ConfigOverrides: config.Overrides{Workspace: &workspace, Tools: &tools}}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			if _, err := s.threads.History(protocol.ThreadID("guardian")); err != nil {
				t.Fatal(err)
			}
			e, err := s.threads.ContextEngine("guardian")
			if err != nil {
				t.Fatal(err)
			}
			seed := e.OptionsSeed()
			if !seed.Guardian.Enabled || seed.Guardian.Timeout != 7*time.Second || seed.Guardian.MaxOutputTokens != 128 || seed.SharedRateLimit == nil {
				t.Fatal("Guardian config/gate not wired")
			}
			r, err := e.PrepareGuardianReview()
			if disabled {
				if err == nil {
					t.Fatal("kill switch ignored")
				}
			} else if err != nil || r.ID() == "" {
				t.Fatal("enabled reviewer unavailable", err)
			}
		})
	}
}

func TestGuardianJudgeRouteUsesConnectionCatalog(t *testing.T) {
	secondary := customConnectionModel("judge-model")
	options := routeSetOptions{Act: bundledAct(), Extras: []ExtraConnectionSpec{{ProviderID: "judge-connection", BaseURL: "https://judge.example.com/v1", Protocol: model.ProtocolOpenAIChat, Model: secondary}}, Slots: map[string]config.RouteSlot{"judge": {Provider: "judge-connection", Model: "judge-model"}}}
	routes, err := resolveRouteSet(options)
	if err != nil {
		t.Fatal(err)
	}
	judge, err := routes.For(model.PurposeJudge)
	if err != nil || judge.ProviderID() != "judge-connection" || judge.Endpoint() != "https://judge.example.com/v1" {
		t.Fatal("judge ignored catalog connection", err)
	}
	options.Slots["judge"] = config.RouteSlot{Provider: "unknown-connection", Model: "judge-model"}
	if _, err := resolveRouteSet(options); err == nil {
		t.Fatal("unknown explicit judge silently fell back")
	}
	options.Slots["judge"] = config.RouteSlot{Provider: "judge-connection", Model: "unknown-model"}
	if _, err := resolveRouteSet(options); err == nil {
		t.Fatal("unknown explicit judge model silently fell back")
	}
}
