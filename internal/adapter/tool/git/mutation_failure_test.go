package git

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolresult "github.com/fwtllh-png/QCode/internal/adapter/tool/result"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestGitCommitFailurePreservesDiagnosticAndRequestsStatus(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	runGit(t, root, "config", "user.useConfigOnly", "true")
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "note.txt")
	broker, err := workspacebroker.New(root, authority.NewLeaseAuthority(authority.LeaseAuthorityOptions{}), time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterMutations(registry, root, broker); err != nil {
		t.Fatal(err)
	}
	_, err = tooltest.Execute(t.Context(), registry, tool.Call{
		Name: "git_commit", Arguments: json.RawMessage(`{"message":"fixture"}`),
	})
	if err == nil {
		t.Fatal("commit without configured identity succeeded")
	}
	content, ok := toolresult.RecoverableFailure(err)
	hint, hinted := tool.RecoveryHintFromError(err)
	if !ok || !hinted || hint.RequiredAction != "git_status" || hint.RetryOriginal ||
		hint.ErrorCategory != "git_operation_failed" ||
		!strings.Contains(content, "identity unknown") || !strings.Contains(content, "code 128") {
		t.Fatalf("failure content=%q hint=%+v err=%v", content, hint, err)
	}
}
