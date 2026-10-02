package wire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/testutil/tooltest"
)

func TestChatWorkspacesProvisionMergeAndRestore(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace)
	manager := trackedChatWorkspaces(t, session)
	if manager == nil {
		t.Fatal("SessionWorkspaces is nil")
	}

	first, err := manager.Provision(
		t.Context(), "session-chat-one", protocol.ThreadID("thread-chat-one"),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Provision(
		t.Context(), "session-chat-two", protocol.ThreadID("thread-chat-two"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Root == second.Root || first.Root == workspace || second.Root == workspace {
		t.Fatalf("worktrees are not isolated: first=%q second=%q", first.Root, second.Root)
	}
	if err := os.WriteFile(
		filepath.Join(first.Root, "chat-note.txt"), []byte("from isolated Chat\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanMerge(
		t.Context(), "session-chat-one", protocol.ThreadID("thread-chat-one"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != "chat-note.txt" ||
		len(plan.ID) != 64 {
		t.Fatalf("merge plan = %+v", plan)
	}
	applied, err := manager.ApplyMerge(
		t.Context(), "session-chat-one", protocol.ThreadID("thread-chat-one"), plan.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied.ID != plan.ID {
		t.Fatalf("applied plan = %q, want %q", applied.ID, plan.ID)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "chat-note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "from isolated Chat\n" {
		t.Fatalf("merged body = %q", body)
	}

	closeSession(t, session)
	restoredSession := openChatWorkspaceSession(t, workspace)
	restored, err := trackedChatWorkspaces(t, restoredSession).Restore(
		t.Context(), "session-chat-one", protocol.ThreadID("thread-chat-one"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Root != first.Root {
		t.Fatalf("restored root = %q, want %q", restored.Root, first.Root)
	}
}

func TestChatWorkspaceMergeBatchesLargeChangeSet(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace)
	manager := trackedChatWorkspaces(t, session)
	isolated, err := manager.Provision(
		t.Context(), "session-chat-large", protocol.ThreadID("thread-chat-large"),
	)
	if err != nil {
		t.Fatal(err)
	}
	const fileCount = 170
	for index := range fileCount {
		name := fmt.Sprintf("generated/file-%03d.txt", index)
		path := filepath.Join(isolated.Root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := manager.PlanMerge(
		t.Context(), "session-chat-large", protocol.ThreadID("thread-chat-large"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != fileCount || len(plan.ID) != 64 {
		t.Fatalf("large merge plan: files=%d id=%q", len(plan.Files), plan.ID)
	}
	for _, file := range plan.Files {
		if file.Before != "" || file.After != "" {
			t.Fatalf("merge plan retained full file content for %s", file.Path)
		}
	}
	if _, err := manager.ApplyMerge(
		t.Context(), "session-chat-large",
		protocol.ThreadID("thread-chat-large"), plan.ID,
	); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 63, 64, 127, 128, 169} {
		name := fmt.Sprintf("generated/file-%03d.txt", index)
		body, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != name+"\n" {
			t.Fatalf("%s body = %q", name, body)
		}
	}
	if _, err := manager.PlanMerge(
		t.Context(), "session-chat-large", protocol.ThreadID("thread-chat-large"),
	); !errors.Is(err, app.ErrSessionWorkspaceClean) {
		t.Fatalf("post-merge PlanMerge error = %v", err)
	}
}

func TestIsolatedChatSandboxCanReadWorktreeGitMetadata(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace)
	isolated, err := trackedChatWorkspaces(t, session).Provision(
		t.Context(), "session-chat-git", protocol.ThreadID("thread-chat-git"),
	)
	if err != nil {
		t.Fatal(err)
	}
	toolset, err := session.childTools.open(isolated.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := toolset.registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Lookup(toolsearch.ToolName); !ok {
		t.Fatal("isolated Chat toolset has no bounded tool_search surface")
	}
	toolContext := tool.WithInvocationIdentity(
		t.Context(),
		tool.InvocationIdentity{ThreadID: "thread-chat-git"},
	)
	shown, err := tooltest.Execute(toolContext, toolset.registry, tool.Call{
		Name: "git_show", Arguments: json.RawMessage(`{"revision":"HEAD"}`),
	})
	if err != nil || shown.IsError ||
		!strings.Contains(shown.Content, "qcode chat baseline") {
		t.Fatalf("git_show result=%+v err=%v", shown, err)
	}
	shell, err := tooltest.Execute(toolContext, toolset.registry, tool.Call{
		Name: "exec_command",
		Arguments: json.RawMessage(
			`{"command":"git rev-parse --is-inside-work-tree && git log -1 --format=%s"}`,
		),
	})
	if err != nil || shell.IsError ||
		!strings.Contains(shell.Content, "true") ||
		!strings.Contains(shell.Content, "qcode chat baseline") ||
		strings.Contains(shell.Content, "xcrun_db") {
		t.Fatalf("shell Git result=%+v err=%v", shell, err)
	}
	escaped := filepath.Join(workspace, "sandbox-escape.txt")
	arguments, err := json.Marshal(map[string]string{
		"command": fmt.Sprintf(
			"printf escaped > '%s'",
			strings.ReplaceAll(escaped, "'", "'\"'\"'"),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	write, err := tooltest.Execute(toolContext, toolset.registry, tool.Call{
		Name: "exec_command", Arguments: arguments,
	})
	if err == nil && !write.IsError {
		t.Fatalf("main workspace write unexpectedly succeeded: %+v", write)
	}
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("main workspace escape exists: %v", err)
	}
}

func TestIsolatedChatInteractionToolsPauseAndResume(t *testing.T) {
	workspace := newGitWorkspace(t)
	fixture := isolatedChatInputFixture(t)
	tools := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: fixture,
		Permission:  "bypass",
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		discardChatWorkspaces(t, session)
		closeSession(t, session)
	})
	const sessionID = "session-chat-input"
	const threadID = protocol.ThreadID("thread-chat-input")
	isolated, err := trackedChatWorkspaces(t, session).Provision(
		t.Context(), sessionID, threadID,
	)
	if err != nil {
		t.Fatal(err)
	}
	toolset, err := session.childTools.open(isolated.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := toolset.registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"request_user_input", "update_plan", "project_map",
	} {
		if _, ok := snapshot.Lookup(name); !ok {
			t.Fatalf("isolated Chat toolset has no %s", name)
		}
	}
	if toolset.inputHost == nil || toolset.inputHost == session.inputHost {
		t.Fatal("isolated Chat does not own a dedicated input host")
	}

	events, err := session.Runtime.Events(
		t.Context(), session.Runtime.Snapshot(t.Context()).LastSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := protocol.NewTurnID()
	if err != nil {
		t.Fatal(err)
	}
	itemID, err := protocol.NewItemID()
	if err != nil {
		t.Fatal(err)
	}
	start, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: itemID,
		Prompt: "confirm isolated chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Runtime.Submit(t.Context(), start); err != nil {
		t.Fatal(err)
	}

	var requestID string
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for requestID == "" {
		select {
		case event := <-events:
			if event.ThreadID != threadID {
				continue
			}
			switch data := event.Data.(type) {
			case *protocol.InputRequiredData:
				requestID = data.RequestID
			case *protocol.TurnFailedData:
				t.Fatalf("isolated Chat failed before input: %+v", data)
			}
		case <-deadline.C:
			t.Fatal("isolated Chat did not request input")
		}
	}
	replyItemID, err := protocol.NewItemID()
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.NewOperation(&protocol.InputReplyPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: replyItemID,
		RequestID: requestID, Answer: "yes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Runtime.Submit(t.Context(), reply); err != nil {
		t.Fatal(err)
	}

	var resolved, completed int
	deadline.Reset(5 * time.Second)
	for completed == 0 {
		select {
		case event := <-events:
			if event.ThreadID != threadID {
				continue
			}
			switch data := event.Data.(type) {
			case *protocol.InputResolvedData:
				resolved++
			case *protocol.TurnCompletedData:
				completed++
			case *protocol.TurnFailedData:
				t.Fatalf("isolated Chat failed after input: %+v", data)
			}
		case <-deadline.C:
			t.Fatal("isolated Chat did not complete after input")
		}
	}
	if resolved != 1 || completed != 1 {
		t.Fatalf("input lifecycle resolved=%d completed=%d", resolved, completed)
	}
}

func isolatedChatInputFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"fixture.json": `{
  "protocol": "openai_chat",
  "path": "/chat/completions",
  "model": "fixture-model",
  "expected_prompt": "confirm isolated chat",
  "streams": ["input.sse", "complete.sse"],
  "expected_request_fragments": [
    ["\"name\":\"request_user_input\""],
    ["\"tool_call_id\":\"call_input\""]
  ]
}
`,
		"input.sse": `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_input","function":{"name":"request_user_input","arguments":"{\"prompt\":\"May I continue?\",\"options\":[\"yes\",\"no\"]}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5}}

data: [DONE]
`,
		"complete.sse": `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_complete","function":{"name":"turn_complete","arguments":"{\"status\":\"complete\",\"summary\":\"isolated input resumed\",\"pending_actions\":[]}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":13,"completion_tokens":6}}

data: [DONE]
`,
	}
	for name, body := range files {
		if err := os.WriteFile(
			filepath.Join(root, name), []byte(body), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestChatWorkspaceMergeRejectsParentDrift(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace)
	manager := trackedChatWorkspaces(t, session)
	isolated, err := manager.Provision(
		t.Context(), "session-chat-conflict", protocol.ThreadID("thread-chat-conflict"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(isolated.Root, "README.md"), []byte("Chat version\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "README.md"), []byte("editor version\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	_, err = manager.PlanMerge(
		t.Context(), "session-chat-conflict", protocol.ThreadID("thread-chat-conflict"),
	)
	if err == nil || !strings.Contains(err.Error(), "main workspace drifted") {
		t.Fatalf("PlanMerge error = %v", err)
	}
	body, readErr := os.ReadFile(filepath.Join(workspace, "README.md"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "editor version\n" {
		t.Fatalf("conflict changed main workspace: %q", body)
	}
}

func TestChatWorkspaceMergeCombinesNonOverlappingParentDrift(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace)
	manager := trackedChatWorkspaces(t, session)
	base := "first\nsecond\nthird\n"
	if err := os.WriteFile(
		filepath.Join(workspace, "README.md"), []byte(base), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	runChatGit(t, workspace, "add", "README.md")
	runChatGit(
		t, workspace,
		"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"commit", "--no-gpg-sign", "-m", "three-way base",
	)
	isolated, err := manager.Provision(
		t.Context(), "session-chat-three-way", protocol.ThreadID("thread-chat-three-way"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "README.md"),
		[]byte("first from main\nsecond\nthird\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(isolated.Root, "README.md"),
		[]byte("first\nsecond\nthird from chat\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	baseline := runChatGit(t, isolated.Root, "show", "HEAD:README.md")
	if baseline != base {
		t.Fatalf("baseline=%q", baseline)
	}

	plan, err := manager.PlanMerge(
		t.Context(), "session-chat-three-way", protocol.ThreadID("thread-chat-three-way"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || !strings.Contains(plan.Diff, "third from chat") {
		t.Fatalf("three-way plan = %+v", plan)
	}
	if _, err := manager.ApplyMerge(
		t.Context(), "session-chat-three-way",
		protocol.ThreadID("thread-chat-three-way"), plan.ID,
	); err != nil {
		t.Fatal(err)
	}
	want := "first from main\nsecond\nthird from chat\n"
	for _, root := range []string{workspace, isolated.Root} {
		body, err := os.ReadFile(filepath.Join(root, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("%s README = %q, want %q", root, body, want)
		}
	}
}

func TestChatWorkspaceMergeApplyRejectsReadOnlyPosture(t *testing.T) {
	workspace := newGitWorkspace(t)
	session := openChatWorkspaceSession(t, workspace, "never")
	manager := trackedChatWorkspaces(t, session)
	isolated, err := manager.Provision(
		t.Context(), "session-chat-readonly", protocol.ThreadID("thread-chat-readonly"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(isolated.Root, "readonly-note.txt"), []byte("blocked\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	plan, err := manager.PlanMerge(
		t.Context(), "session-chat-readonly", protocol.ThreadID("thread-chat-readonly"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyMerge(
		t.Context(), "session-chat-readonly",
		protocol.ThreadID("thread-chat-readonly"), plan.ID,
	); err == nil || !strings.Contains(err.Error(), "read-only workspace") {
		t.Fatalf("ApplyMerge error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "readonly-note.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only merge wrote main workspace: %v", err)
	}
}

func TestIsolatedChatTurnsStartConcurrently(t *testing.T) {
	workspace := newGitWorkspace(t)
	tools := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "slow"),
		Permission:  "bypass",
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSession(t, session) })
	manager := trackedChatWorkspaces(t, session)
	threads := []protocol.ThreadID{"thread-parallel-one", "thread-parallel-two"}
	sessions := []string{"session-parallel-one", "session-parallel-two"}
	for index := range threads {
		if _, err := manager.Provision(
			t.Context(), sessions[index], threads[index],
		); err != nil {
			t.Fatal(err)
		}
	}
	events, err := session.Runtime.Events(
		t.Context(), session.Runtime.Snapshot(t.Context()).LastSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	for index, threadID := range threads {
		turnID, err := protocol.NewTurnID()
		if err != nil {
			t.Fatal(err)
		}
		itemID, err := protocol.NewItemID()
		if err != nil {
			t.Fatal(err)
		}
		operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
			ThreadID: threadID, TurnID: turnID, ItemID: itemID,
			Prompt: "wait for interrupt",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Runtime.Submit(t.Context(), operation); err != nil {
			t.Fatalf("submit Chat %d: %v", index, err)
		}
	}
	started := make(map[protocol.ThreadID]bool)
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	for len(started) < len(threads) {
		select {
		case event := <-events:
			if event.Kind == protocol.EventTurnCompleted ||
				event.Kind == protocol.EventTurnFailed ||
				event.Kind == protocol.EventTurnCanceled {
				t.Fatalf(
					"turn %s became terminal before both isolated Chats started", event.ThreadID,
				)
			}
			if event.Kind == protocol.EventTurnStarted {
				started[event.ThreadID] = true
			}
		case <-timer.C:
			t.Fatalf("isolated Chat starts were serialized: started=%v", started)
		}
	}
}

func openChatWorkspaceSession(t *testing.T, workspace string, posture ...string) *Session {
	t.Helper()
	permission := "bypass"
	if len(posture) != 0 {
		permission = posture[0]
	}
	tools := true
	session, err := NewExec(t.Context(), withNonDurableTestJournal(t, ExecOptions{
		FixturePath: subagentFixture(t, "subagent"),
		Permission:  permission,
		ConfigOverrides: config.Overrides{
			Tools: &tools, Workspace: &workspace,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if session.Runtime == nil {
			return
		}
		discardChatWorkspaces(t, session)
		closeSession(t, session)
	})
	return session
}

// workspaceTracker records only successful interface calls for fixture cleanup.
type workspaceTracker struct {
	app.SessionWorkspaceManager
	sessions map[string]protocol.ThreadID
}

func trackedChatWorkspaces(t *testing.T, session *Session) *workspaceTracker {
	t.Helper()
	if tracker, ok := session.chatWorkspaces.(*workspaceTracker); ok {
		return tracker
	}
	if session.chatWorkspaces == nil {
		t.Fatal("SessionWorkspaces is nil")
	}
	tracker := &workspaceTracker{
		SessionWorkspaceManager: session.chatWorkspaces,
		sessions:                make(map[string]protocol.ThreadID),
	}
	session.chatWorkspaces = tracker
	return tracker
}

func (w *workspaceTracker) Provision(ctx context.Context, sessionID string, threadID protocol.ThreadID) (app.SessionWorkspace, error) {
	value, err := w.SessionWorkspaceManager.Provision(ctx, sessionID, threadID)
	if err == nil {
		w.sessions[sessionID] = threadID
	}
	return value, err
}

func (w *workspaceTracker) Restore(ctx context.Context, sessionID string, threadID protocol.ThreadID) (app.SessionWorkspace, error) {
	value, err := w.SessionWorkspaceManager.Restore(ctx, sessionID, threadID)
	if err == nil {
		w.sessions[sessionID] = threadID
	}
	return value, err
}

func discardChatWorkspaces(t *testing.T, session *Session) {
	t.Helper()
	manager, ok := session.chatWorkspaces.(*workspaceTracker)
	if !ok {
		return
	}
	for sessionID, threadID := range manager.sessions {
		if err := manager.Discard(context.Background(), sessionID, threadID); err != nil {
			t.Errorf("discard Chat worktree %s: %v", sessionID, err)
		}
	}
}

func closeSession(t *testing.T, session *Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	session.Runtime = nil
}

func runChatGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}

// Workspace lifecycles live with their state owners; wire only constructs them.
func TestWireOnlyConstructsWorkspaceServices(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			method, ok := decl.(*ast.FuncDecl)
			if !ok || method.Recv == nil {
				continue
			}
			switch method.Name.Name {
			case "Provision", "Discard", "PlanMerge", "ApplyMerge":
				t.Errorf("%s: wire owns workspace lifecycle method %s", fset.Position(method.Pos()), method.Name.Name)
			case "Restore":
				ast.Inspect(method.Type.Results, func(node ast.Node) bool {
					if name, ok := node.(*ast.SelectorExpr); ok && name.Sel.Name == "SessionWorkspace" {
						t.Errorf("%s: wire restores session workspaces", fset.Position(method.Pos()))
					}
					return true
				})
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch method.Sel.Name {
			case "AddWorktree", "RemoveWorktree", "PruneWorktrees":
				t.Errorf("%s: wire drives worktree mutation %s", fset.Position(call.Pos()), method.Sel.Name)
			}
			return true
		})
	}
}
