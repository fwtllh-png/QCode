package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	buildinfo "github.com/fwtllh-png/QCode/internal"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	securitycredential "github.com/fwtllh-png/QCode/internal/security/credential"
)

func TestMain(m *testing.M) {
	loadWebAssets = func() (fs.FS, error) {
		return fstest.MapFS{
			"index.html": {
				Data: []byte("<main>QCode</main>"),
				Mode: fs.FileMode(0o444),
			},
		}, nil
	}
	os.Exit(m.Run())
}

func TestRunContextExposesOnlyRuntimeStartupFlags(t *testing.T) {
	for _, legacyCommand := range []string{"web", "exec", "tui", "doctor"} {
		var stdout, stderr bytes.Buffer
		if code := RunContext(
			t.Context(),
			[]string{legacyCommand},
			&stdout,
			&stderr,
		); code != 2 {
			t.Fatalf("%s exit = %d, stderr = %q", legacyCommand, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "unexpected arguments") {
			t.Fatalf("%s stderr = %q", legacyCommand, stderr.String())
		}
	}

	var stdout, stderr bytes.Buffer
	if code := RunContext(
		t.Context(),
		[]string{"--version"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf("--version exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "qcode") {
		t.Fatalf("--version output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunContext(
		t.Context(),
		[]string{"--help"},
		&stdout,
		&stderr,
	); code != 0 {
		t.Fatalf("--help exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Normally launched by QCode.app") {
		t.Fatalf("--help output = %q", stdout.String())
	}
	for _, name := range []string{"config", "data-dir", "mcp-config", "provider-fixture", "version"} {
		if !strings.Contains(stdout.String(), "-"+name) {
			t.Fatalf("--help missing %s: %q", name, stdout.String())
		}
	}
	if !strings.Contains(stdout.String(), `-port int`) ||
		!strings.Contains(stdout.String(), `(default 6732)`) {
		t.Fatalf("--help port default = %q", stdout.String())
	}
	help := stdout.String()
	for _, name := range []string{
		"workspace", "host", "open", "no-open", "replace-owner",
		"enable-tools", "posture", "provider", "model", "api-key-env",
	} {
		if strings.Contains(help, "  -"+name+"\n") ||
			strings.Contains(help, "  -"+name+" ") {
			t.Fatalf("--help exposes removed flag %s", name)
		}
		stderr.Reset()
		if code := RunContext(t.Context(), []string{"--" + name}, io.Discard, &stderr); code != 2 ||
			!strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("removed flag %s exit=%d stderr=%q", name, code, stderr.String())
		}
	}
}

func TestLoadWebConfigToolDefaultsAndExplicitSettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		env     string
		want    bool
		source  config.Source
	}{
		{"no-config", "", "", true, config.SourceDefault},
		{"config-without-tools", "[execution]\n", "", true, config.SourceDefault},
		{"config-disabled", "[execution]\ntools = false\n", "", false, config.SourceFile},
		{"config-enabled", "[execution]\ntools = true\n", "", true, config.SourceFile},
		{"env-disabled", "[execution]\ntools = true\n", "false", false, config.SourceEnv},
		{"env-enabled", "[execution]\ntools = false\n", "true", true, config.SourceEnv},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("QCODE_TOOLS", test.env)
			if test.env == "" {
				if err := os.Unsetenv("QCODE_TOOLS"); err != nil {
					t.Fatal(err)
				}
			}
			options := webCommandOptions{}
			if test.content != "" {
				options.configPath = writeLauncherConfig(t, test.content)
			}
			loaded, err := loadWebConfig(options)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Config.Execution.Tools != test.want ||
				loaded.Provenance["execution.tools"] != test.source {
				t.Fatalf("tools=%v source=%s", loaded.Config.Execution.Tools, loaded.Provenance["execution.tools"])
			}
			selection := webSetupSelection{Connections: []webSetupConnection{
				{ID: "openai", Model: "fixture-model", Protocol: "openai_chat"},
			}}
			reloaded, err := loadWebSetupConfig(options, selection, securitycredential.Reference{})
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Config.Execution.Tools != test.want {
				t.Fatalf("setup reload tools=%v", reloaded.Config.Execution.Tools)
			}
		})
	}
}

func TestWebOwnerBuildIncludesBuildDate(t *testing.T) {
	got := webOwnerBuild(buildinfo.Info{
		Version: "dev", Commit: "commit", BuildDate: "date",
	})
	if got != "dev+commit@date" {
		t.Fatalf("build identity = %q", got)
	}
}

func writeLauncherConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureLauncherConfig(t *testing.T, workspace string) string {
	t.Helper()
	content := "[execution]\nprovider = \"openai\"\nmodel = \"fixture-model\"\ntools = false\n"
	if workspace != "" {
		content += fmt.Sprintf("workspace = %q\n", workspace)
	}
	return writeLauncherConfig(t, content)
}

func TestLoadWebConfigLeavesProviderAndModelForGuidedSetup(t *testing.T) {
	loaded, err := loadWebConfig(webCommandOptions{workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Execution.Provider != "" ||
		loaded.Config.Execution.Model != "" ||
		!loaded.Config.Credential.Empty() {
		t.Fatalf("unexpected default route or credential: %+v", loaded.Config)
	}
}

func TestRunContextStartsAndStopsWebHost(t *testing.T) {
	workspace := t.TempDir()
	if err := exec.Command("git", "-C", workspace, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(
		filepath.Join("..", "..", "testdata", "providers", "openai"),
	)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "state")
	configPath := fixtureLauncherConfig(t, workspace)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outputReader, outputWriter := io.Pipe()
	exitCode := make(chan int, 1)
	go func() {
		exitCode <- RunContext(ctx, []string{
			"--config", configPath,
			"--data-dir", dataDir,
			"--provider-fixture", fixture,
			"--port", "0",
		}, outputWriter, io.Discard)
		_ = outputWriter.Close()
	}()

	url := waitForReadyURL(t, outputReader)
	response, err := http.Get(strings.TrimSuffix(url, "/") + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}

	secondWorkspace := t.TempDir()
	if err := exec.Command("git", "-C", secondWorkspace, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	var secondOutput, secondError bytes.Buffer
	if code := RunContext(t.Context(), []string{
		"--config", fixtureLauncherConfig(t, secondWorkspace),
		"--data-dir", dataDir,
		"--provider-fixture", fixture,
		"--port", "0",
	}, &secondOutput, &secondError); code != 0 {
		t.Fatalf(
			"second Workspace start exit=%d stderr=%q",
			code,
			secondError.String(),
		)
	}
	secondRoot, secondIdentity, err := normalizeWorkspaceRoot(secondWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		secondOutput.String(),
		"?workspace="+secondIdentity.RootID,
	) {
		t.Fatalf("second Workspace output = %q", secondOutput.String())
	}
	token := fetchSupervisorToken(t, dataDir)
	catalog := fetchWorkspaceCatalog(t, url, token)
	if len(catalog.Workspaces) != 2 {
		t.Fatalf("Workspace catalog = %+v", catalog)
	}
	foundSecond := false
	for _, descriptor := range catalog.Workspaces {
		if descriptor.ID == secondIdentity.RootID &&
			descriptor.Root == secondRoot &&
			descriptor.Ready {
			foundSecond = true
		}
	}
	if !foundSecond {
		t.Fatalf("second Workspace is not ready: %+v", catalog)
	}
	reconfigureBody := strings.NewReader(
		`{"model":"deepseek-chat","api_key":"fixture-key",` +
			`"base_url":"http://127.0.0.1:1/v1","protocol":"openai_chat",` +
			`"model_metadata":{"canonical_id":"deepseek-chat","wire_id":"deepseek-chat",` +
			`"context_tokens":8192,"max_output_tokens":1024,` +
			`"capabilities":{"streaming":true,"tool_calls":true,"reasoning":false,` +
			`"native_search":false,"vision":false,"incremental_responses":false,` +
			`"image_input":false,"prompt_cache":false,"automatic_prompt_cache":false,` +
			`"thinking_toggle":false}}}`,
	)
	reconfigureRequest, err := http.NewRequest(
		http.MethodPost,
		strings.TrimSuffix(url, "/")+"/api/v1/setup/apply",
		reconfigureBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	reconfigureRequest.Header.Set("Authorization", "Bearer "+token)
	reconfigureRequest.Header.Set("Content-Type", "application/json")
	reconfigureRequest.Header.Set("X-QCode-Request-ID", "provider-change")
	reconfigureRequest.Header.Set("Idempotency-Key", "provider-change")
	reconfigureResponse, err := http.DefaultClient.Do(reconfigureRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer reconfigureResponse.Body.Close()
	if reconfigureResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(reconfigureResponse.Body)
		t.Fatalf(
			"provider reconfiguration status=%d body=%s",
			reconfigureResponse.StatusCode,
			body,
		)
	}
	reconfiguredCatalog := fetchWorkspaceCatalog(t, url, token)
	for _, descriptor := range reconfiguredCatalog.Workspaces {
		if !descriptor.Ready {
			t.Fatalf("reconfigured Workspace is not ready: %+v", descriptor)
		}
	}

	cancel()
	select {
	case code := <-exitCode:
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Web host did not stop")
	}

	restartContext, stopRestart := context.WithCancel(t.Context())
	restartReader, restartWriter := io.Pipe()
	restartExit := make(chan int, 1)
	go func() {
		restartExit <- RunContext(restartContext, []string{
			"--config", configPath,
			"--data-dir", dataDir,
			"--provider-fixture", fixture,
			"--port", "0",
		}, restartWriter, io.Discard)
		_ = restartWriter.Close()
	}()
	restartURL := waitForReadyURL(t, restartReader)
	restartCatalog := fetchWorkspaceCatalog(
		t,
		restartURL,
		fetchSupervisorToken(t, dataDir),
	)
	if len(restartCatalog.Workspaces) != 2 {
		t.Fatalf("restored Workspace catalog = %+v", restartCatalog)
	}
	for _, descriptor := range restartCatalog.Workspaces {
		if !descriptor.Ready {
			t.Fatalf("restored Workspace is not ready: %+v", descriptor)
		}
	}
	removeWorkspace(
		t,
		restartURL,
		fetchSupervisorToken(t, dataDir),
		secondIdentity.RootID,
	)
	removedCatalog := fetchWorkspaceCatalog(
		t,
		restartURL,
		fetchSupervisorToken(t, dataDir),
	)
	if len(removedCatalog.Workspaces) != 1 {
		t.Fatalf("Workspace catalog after removal = %+v", removedCatalog)
	}
	stopRestart()
	select {
	case code := <-restartExit:
		if code != 0 {
			t.Fatalf("restart exit = %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("restarted Web host did not stop")
	}
}

func TestRunContextStartsWithoutAConfigFile(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("QCODE_WORKSPACE", workspace)
	if err := exec.Command("git", "-C", workspace, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outputReader, outputWriter := io.Pipe()
	outputLines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(outputReader)
		for scanner.Scan() {
			outputLines <- scanner.Text()
		}
		close(outputLines)
	}()
	exitCode := make(chan int, 1)
	go func() {
		exitCode <- RunContext(ctx, []string{
			"--data-dir", dataDir,
			"--port", "0",
		}, outputWriter, io.Discard)
		_ = outputWriter.Close()
	}()

	setupURL := waitForOutputURL(t, outputLines, "QCode Setup Ready: ")
	token := fetchSupervisorToken(t, dataDir)
	bootstrapRequest, err := http.NewRequest(
		http.MethodGet, strings.TrimSuffix(setupURL, "/")+"/api/v1/bootstrap", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRequest.Header.Set("Authorization", "Bearer "+token)
	bootstrapResponse, err := http.DefaultClient.Do(bootstrapRequest)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap struct {
		Authenticated bool `json:"authenticated"`
		SetupRequired bool `json:"setup_required"`
	}
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrap); err != nil {
		_ = bootstrapResponse.Body.Close()
		t.Fatal(err)
	}
	_ = bootstrapResponse.Body.Close()
	if !bootstrap.SetupRequired || !bootstrap.Authenticated {
		t.Fatalf("bootstrap = %+v", bootstrap)
	}
	setupBody := strings.NewReader(
		`{"model":"local-model","api_key":"secret-value",` +
			`"base_url":"http://127.0.0.1:1/v1","protocol":"openai_chat",` +
			`"model_metadata":{"canonical_id":"local-model","wire_id":"local-model",` +
			`"context_tokens":8192,"max_output_tokens":1024,` +
			`"capabilities":{"streaming":true,"tool_calls":true,` +
			`"reasoning":false,"native_search":false,"vision":false,` +
			`"incremental_responses":false,"image_input":false,` +
			`"prompt_cache":false,"automatic_prompt_cache":false,` +
			`"thinking_toggle":false}}}`,
	)
	setupRequest, err := http.NewRequest(
		http.MethodPost, strings.TrimSuffix(setupURL, "/")+"/api/v1/setup/apply", setupBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	setupRequest.Header.Set("Authorization", "Bearer "+token)
	setupRequest.Header.Set("Content-Type", "application/json")
	setupRequest.Header.Set("X-QCode-Request-ID", "setup-request")
	setupRequest.Header.Set("Idempotency-Key", "setup-idempotency")
	setupResponse, err := http.DefaultClient.Do(setupRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer setupResponse.Body.Close()
	if setupResponse.StatusCode != http.StatusOK {
		t.Fatalf("setup status = %d", setupResponse.StatusCode)
	}
	readyURL := waitForOutputURL(t, outputLines, "QCode Runtime Ready: ")
	if readyURL != setupURL {
		t.Fatalf("ready URL = %q, want %q", readyURL, setupURL)
	}
	var repeatedOutput, repeatedError bytes.Buffer
	if code := RunContext(t.Context(), []string{
		"--data-dir", dataDir,
		"--port", "0",
	}, &repeatedOutput, &repeatedError); code != 0 {
		t.Fatalf(
			"repeated start exit = %d, stderr = %q",
			code,
			repeatedError.String(),
		)
	}
	if !strings.Contains(
		repeatedOutput.String(),
		"QCode Runtime Ready: "+readyURL,
	) || !strings.Contains(repeatedOutput.String(), "&launch=") {
		t.Fatalf("repeated start output = %q", repeatedOutput.String())
	}

	cancel()
	select {
	case code := <-exitCode:
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Web host did not stop")
	}
}

func TestSupervisorDoesNotRegisterCWDOrRestoreRemovedWorkspace(t *testing.T) {
	fixture, err := filepath.Abs("../../testdata/providers/openai")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	args := []string{
		"--config", fixtureLauncherConfig(t, ""),
		"--data-dir", dataDir, "--provider-fixture", fixture,
		"--port", "0",
	}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
	var stdout, stderr bytes.Buffer
	if code := RunContext(t.Context(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("owner reuse exit=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "?workspace=") {
		t.Fatal("plain startup selected a Workspace")
	}
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
	root := t.TempDir()
	token := fetchSupervisorToken(t, dataDir)
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
	token = fetchSupervisorToken(t, dataDir)
	catalog = fetchWorkspaceCatalog(t, url, token)
	if len(catalog.Workspaces) != 1 || catalog.Workspaces[0].ID != id || !catalog.Workspaces[0].Ready {
		t.Fatalf("explicit Workspace was not restored: %+v", catalog)
	}
	removeWorkspace(t, url, token, id)
	stop()
	url, _ = startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
}

func TestSupervisorSetupWithoutWorkspacePersistsConnection(t *testing.T) {
	dataDir := t.TempDir()
	args := []string{"--data-dir", dataDir, "--port", "0"}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Setup Ready: ")
	assertNoWorkspaceBootstrap(t, url, dataDir, false)
	token := fetchSupervisorToken(t, dataDir)
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
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
	if _, err := os.Stat(filepath.Join(dataDir, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("setup created Workspace runtime state: %v", err)
	}
	if _, err := registerWorkspaceWithOwner(t.Context(), url, token, dataDir); err == nil {
		t.Fatal("state directory was accepted as a Workspace")
	}
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
	stop()
	url, _ = startWorkspaceSupervisor(t, args, "QCode Runtime Ready: ")
	assertNoWorkspaceBootstrap(t, url, dataDir, true)
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
		"--config", writeLauncherConfig(t, fmt.Sprintf("[execution]\nworkspace = %q\n", workspace)),
		"--data-dir", dataDir, "--port", "0",
	}
	url, stop := startWorkspaceSupervisor(t, args, "QCode Setup Ready: ")
	token := fetchSupervisorToken(t, dataDir)
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
	// A key-less probe reopens the connection's credential control with no
	// selected reference; an un-activated staged key would be reaped here.
	_, recovered, err := securitycredential.OpenControl(
		t.Context(), dataDir, webSupervisorScope, second.Provider,
		securitycredential.Reference{}, securitycredential.Reference{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != *second.Credential {
		t.Fatalf("recovered credential = %+v, want activated %+v", recovered, *second.Credential)
	}
	if _, err := securitycredential.NewKeyringStore().Lookup(t.Context(), second.Credential.Name); err != nil {
		t.Fatalf("added connection key was removed: %v", err)
	}
	stop()
}

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

func assertNoWorkspaceBootstrap(t *testing.T, url, dataDir string, ready bool) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url+"api/v1/bootstrap", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fetchSupervisorToken(t, dataDir))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value struct {
		Ready         bool                        `json:"ready"`
		SetupRequired bool                        `json:"setup_required"`
		Workspace     *protocol.WorkspaceIdentity `json:"workspace"`
		WorkspaceRoot string                      `json:"workspace_root"`
		Catalog       WorkspaceCatalog            `json:"workspace_catalog"`
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

func waitForOutputURL(t *testing.T, lines <-chan string, prefix string) string {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatal("Web host exited before " + prefix)
			}
			if value, found := strings.CutPrefix(line, prefix); found {
				return launchOrigin(t, value)
			}
		case <-timer.C:
			t.Fatal("timed out waiting for " + prefix)
		}
	}
}

// launchOrigin strips the one-time launch code (and Workspace selection)
// from a printed ready URL, leaving the base the API calls build on.
func launchOrigin(t *testing.T, printed string) string {
	t.Helper()
	base, query, _ := strings.Cut(printed, "?")
	if !strings.Contains(query, "launch=") {
		t.Fatalf("ready URL carries no launch code: %q", printed)
	}
	return base
}

// fetchSupervisorToken reads the capability token the way out-of-process
// owners do: from the owner lease, never from the Web host.
func fetchSupervisorToken(t *testing.T, dataDir string) string {
	t.Helper()
	data, err := os.ReadFile(ownerLeasePath(dataDir, webSupervisorScope))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 2 {
		t.Fatal("owner lease has no metadata")
	}
	var metadata ownerLeaseMetadata
	if err := json.Unmarshal(data[1:], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.CapabilityToken == "" {
		t.Fatal("owner lease has no capability token")
	}
	return metadata.CapabilityToken
}

func fetchWorkspaceCatalog(
	t *testing.T,
	rawURL string,
	token string,
) WorkspaceCatalog {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost,
		strings.TrimSuffix(rawURL, "/")+"/api/v1/workspace/list",
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Workspace list status = %d", response.StatusCode)
	}
	var envelope struct {
		Result WorkspaceCatalog `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Result
}

func removeWorkspace(
	t *testing.T,
	rawURL string,
	token string,
	workspaceID string,
) {
	t.Helper()
	body, err := json.Marshal(WorkspaceRemoveRequest{
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost,
		strings.TrimSuffix(rawURL, "/")+"/api/v1/workspace/remove",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "remove-"+workspaceID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("Workspace remove status = %d body=%s", response.StatusCode, payload)
	}
}

func waitForReadyURL(t *testing.T, reader io.Reader) string {
	t.Helper()
	result := make(chan struct {
		url string
		err error
	}, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			const prefix = "QCode Runtime Ready: "
			if url, ok := strings.CutPrefix(scanner.Text(), prefix); ok {
				result <- struct {
					url string
					err error
				}{url: url}
				return
			}
		}
		result <- struct {
			url string
			err error
		}{err: scanner.Err()}
	}()
	select {
	case ready := <-result:
		if ready.err != nil {
			t.Fatalf("read Web readiness: %v", ready.err)
		}
		if ready.url == "" {
			t.Fatal("Web host exited before readiness")
		}
		return launchOrigin(t, ready.url)
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for Web readiness")
		return ""
	}
}

func TestProbeWebReadinessRequiresTrustedReadyEndpoint(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/healthz" {
			t.Errorf("probe path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"version":1,"status":"ready"}`))
	}))
	defer ready.Close()
	if err := probeWebReadiness(t.Context(), ready.URL+"/untrusted"); err != nil {
		t.Fatalf("ready owner rejected: %v", err)
	}
	setup := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"version":1,"status":"setup_required"}`))
	}))
	defer setup.Close()
	if err := probeWebReadiness(t.Context(), setup.URL); err != nil {
		t.Fatalf("setup owner rejected: %v", err)
	}

	notReady := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		http.Error(writer, `{"version":1,"status":"initializing"}`, http.StatusServiceUnavailable)
	}))
	defer notReady.Close()
	if err := probeWebReadiness(t.Context(), notReady.URL); err == nil {
		t.Fatal("unready owner accepted")
	}

	redirect := httptest.NewServer(http.RedirectHandler(ready.URL, http.StatusFound))
	defer redirect.Close()
	if err := probeWebReadiness(t.Context(), redirect.URL); err == nil ||
		!strings.Contains(err.Error(), "redirects are forbidden") {
		t.Fatalf("redirect probe error = %v", err)
	}

	if err := probeWebReadiness(context.Background(), "http://localhost:1234/"); err == nil {
		t.Fatal("non-canonical loopback owner URL accepted")
	}
}
