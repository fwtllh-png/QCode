package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func startWorkspaceSupervisor(t *testing.T, args []string, readyPrefix string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	lines := make(chan string, 16)
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	go func() {
		done <- RunContext(ctx, args, writer, &stderr)
		_ = writer.Close()
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("Supervisor exit=%d stderr=%s", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Error("Supervisor did not stop")
			}
			_ = reader.Close()
		})
	}
	t.Cleanup(stop)
	return waitForOutputURL(t, lines, readyPrefix), stop
}

func assertNoWorkspaceBootstrap(t *testing.T, url string, ready bool) {
	t.Helper()
	response, err := http.Get(url + "api/v1/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value struct {
		Ready         bool                        `json:"ready"`
		SetupRequired bool                        `json:"setup_required"`
		Workspace     *protocol.WorkspaceIdentity `json:"workspace"`
		WorkspaceRoot string                      `json:"workspace_root"`
		Catalog       webhost.WorkspaceCatalog    `json:"workspace_catalog"`
	}
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value.Ready != ready || value.SetupRequired == ready ||
		value.Workspace != nil || value.WorkspaceRoot != "" ||
		len(value.Catalog.Workspaces) != 0 {
		t.Fatalf("unexpected workspace bootstrap: %+v", value)
	}
}

func TestSupervisorDoesNotRegisterCWDOrRestoreRemovedWorkspace(t *testing.T) {
	fixture, err := filepath.Abs("../../../testdata/providers/openai")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	args := []string{
		"--data-dir", dataDir, "--provider-fixture", fixture,
		"--provider", "openai", "--model", "fixture-model",
		"--enable-tools=false", "--port", "0", "--no-open",
	}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, true)
	var stdout, stderr bytes.Buffer
	if code := RunContext(t.Context(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("owner reuse exit=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "?workspace=") {
		t.Fatal("plain startup selected a Workspace")
	}
	assertNoWorkspaceBootstrap(t, url, true)
	root := t.TempDir()
	token := fetchSupervisorToken(t, url)
	id, err := registerWorkspaceWithOwner(t.Context(), url, token, root)
	if err != nil {
		t.Fatal(err)
	}
	catalog := fetchWorkspaceCatalog(t, url, token)
	if len(catalog.Workspaces) != 1 || catalog.Workspaces[0].ID != id || !catalog.Workspaces[0].Ready {
		t.Fatalf("explicit Workspace not ready: %+v", catalog)
	}
	stop()
	url, stop = startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	token = fetchSupervisorToken(t, url)
	catalog = fetchWorkspaceCatalog(t, url, token)
	if len(catalog.Workspaces) != 1 || catalog.Workspaces[0].ID != id || !catalog.Workspaces[0].Ready {
		t.Fatalf("explicit Workspace was not restored: %+v", catalog)
	}
	removeWorkspace(t, url, token, id)
	stop()
	url, _ = startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, true)
}

func TestSupervisorSetupWithoutWorkspacePersistsConnection(t *testing.T) {
	dataDir := t.TempDir()
	args := []string{"--data-dir", dataDir, "--port", "0", "--no-open"}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Setup Ready: ")
	assertNoWorkspaceBootstrap(t, url, false)
	token := fetchSupervisorToken(t, url)
	request, err := http.NewRequest(http.MethodPost, url+"api/v1/setup/apply", strings.NewReader(
		`{"model":"local-model","api_key":"secret-value",`+
			`"base_url":"http://127.0.0.1:1/v1","protocol":"openai_chat",`+
			`"model_metadata":{"canonical_id":"local-model","wire_id":"local-model",`+
			`"context_tokens":8192,"max_output_tokens":1024,`+
			`"capabilities":{"streaming":true,"tool_calls":true,"reasoning":false,`+
			`"native_search":false,"vision":false,"incremental_responses":false,`+
			`"image_input":false,"prompt_cache":false,"automatic_prompt_cache":false,`+
			`"thinking_toggle":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "empty-workspace-setup")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("setup HTTP status=%d", response.StatusCode)
	}
	assertNoWorkspaceBootstrap(t, url, true)
	if _, err := os.Stat(filepath.Join(dataDir, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("setup created Workspace runtime state: %v", err)
	}
	if _, err := registerWorkspaceWithOwner(t.Context(), url, token, dataDir); err == nil {
		t.Fatal("state directory was accepted as a Workspace")
	}
	assertNoWorkspaceBootstrap(t, url, true)
	stop()
	url, _ = startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, true)
}

// TestAddConnectionKeepsDefaultCredentialOwnership pins the credential
// ownership rule: staging a key for a new connection must update only that
// connection's reference. The default connection keeps its own credential —
// the bug this guards against sent the new connection's key with every
// request of the default connection's models (provider HTTP 401).
func TestAddConnectionKeepsDefaultCredentialOwnership(t *testing.T) {
	workspace := t.TempDir()
	dataDir := t.TempDir()
	args := []string{
		"--workspace", workspace, "--data-dir", dataDir,
		"--port", "0", "--no-open",
	}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Setup Ready: ")
	token := fetchSupervisorToken(t, url)
	apply := func(idempotencyKey, model, baseURL string) {
		request, err := http.NewRequest(http.MethodPost, url+"api/v1/setup/apply",
			strings.NewReader(`{"model":"`+model+`","api_key":"sk-`+idempotencyKey+`",`+
				`"base_url":"`+baseURL+`","protocol":"openai_chat",`+
				`"model_metadata":{"canonical_id":"`+model+`","wire_id":"`+model+`",`+
				`"context_tokens":8192,"max_output_tokens":1024,`+
				`"capabilities":{"streaming":true,"tool_calls":true,"reasoning":false,`+
				`"native_search":false,"vision":false,"incremental_responses":false,`+
				`"image_input":false,"prompt_cache":false,"automatic_prompt_cache":false,`+
				`"thinking_toggle":false}}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", idempotencyKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", idempotencyKey, response.StatusCode)
		}
	}
	apply("first-connection", "model-a", "http://127.0.0.1:1/v1")
	waitSupervisorReady(t, url)
	add := func(idempotencyKey, model, baseURL string) {
		request, err := http.NewRequest(http.MethodPost, url+"api/v1/connection/add",
			strings.NewReader(`{"model":"`+model+`","api_key":"sk-`+idempotencyKey+`",`+
				`"base_url":"`+baseURL+`","protocol":"openai_chat",`+
				`"model_metadata":{"canonical_id":"`+model+`","wire_id":"`+model+`",`+
				`"context_tokens":8192,"max_output_tokens":1024,`+
				`"capabilities":{"streaming":true,"tool_calls":true,"reasoning":false,`+
				`"native_search":false,"vision":false,"incremental_responses":false,`+
				`"image_input":false,"prompt_cache":false,"automatic_prompt_cache":false,`+
				`"thinking_toggle":false}}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", idempotencyKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", idempotencyKey, response.StatusCode)
		}
	}
	add("second-connection", "model-b", "http://127.0.0.1:2/v1")

	selection, found, err := loadWebSetupSelection(dataDir, "")
	if err != nil || !found {
		t.Fatalf("selection found=%v err=%v", found, err)
	}
	if len(selection.Connections) != 2 {
		t.Fatalf("connections = %+v", selection.Connections)
	}
	if selection.DefaultConnection != selection.Connections[0].ID {
		t.Fatalf("default = %+v", selection)
	}
	first := selection.Connections[0]
	second := selection.Connections[1]
	if first.Model != "model-a" || second.Model != "model-b" {
		t.Fatalf("connections = %+v", selection.Connections)
	}
	if first.Credential == nil || second.Credential == nil {
		t.Fatalf("credentials = %+v / %+v", first.Credential, second.Credential)
	}
	if *first.Credential == *second.Credential {
		t.Fatalf("default connection credential was hijacked: %+v", *first.Credential)
	}
	if first.Credential.Kind != "keyring" || second.Credential.Kind != "keyring" {
		t.Fatalf("credential kinds = %+v / %+v", first.Credential, second.Credential)
	}
	stop()
}

func waitSupervisorReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(url + "healthz")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && strings.Contains(string(body), `"status":"ready"`) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("supervisor did not become ready after first configuration")
}
