package envprobe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeCollectsPlatformShellAndToolVersions(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	runner := func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "git":
			return "git version 2.39.5\n", nil
		case "go":
			return "go version go1.26.3 darwin/arm64\n", nil
		case "node":
			return "v22.14.0\n", nil
		default:
			return "", errors.New("not found")
		}
	}
	lines := Probe(runner)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"os: ", " (",
		"shell: /bin/zsh",
		"go go version go1.26.3 darwin/arm64",
		"node v22.14.0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("fingerprint missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "python") {
		t.Errorf("missing tool was reported:\n%s", joined)
	}
}

func TestProbeUsesOnlyTheFirstOutputLine(t *testing.T) {
	runner := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "git" {
			return "\n\ngit version 2.39.5\nwarning: extra noise\n", nil
		}
		return "", errors.New("missing")
	}
	lines := Probe(runner)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "git git version 2.39.5") {
		t.Fatalf("first line not selected: %q", joined)
	}
	if strings.Contains(joined, "warning: extra noise") {
		t.Fatalf("trailing noise leaked: %q", joined)
	}
}

func TestProbeTimeoutIsABoundedContract(t *testing.T) {
	if probeTimeout != 2*time.Second {
		t.Fatalf("probeTimeout = %s", probeTimeout)
	}
	deadline := time.Time{}
	runner := func(ctx context.Context, _ string, _ ...string) (string, error) {
		deadline, _ = ctx.Deadline()
		return "", context.DeadlineExceeded
	}
	Probe(runner)
	if deadline.IsZero() {
		t.Fatal("probe ran without a deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > probeTimeout {
		t.Fatalf("probe deadline = %v", remaining)
	}
}
