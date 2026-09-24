package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
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

func TestRunContextExposesOnlyWebStartupFlags(t *testing.T) {
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
	if !strings.Contains(stdout.String(), "Run the local QCode Web workspace") {
		t.Fatalf("--help output = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `-posture string`) ||
		!strings.Contains(stdout.String(), `(default "auto")`) {
		t.Fatalf("--help defaults = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `-port int`) ||
		!strings.Contains(stdout.String(), `(default 6732)`) {
		t.Fatalf("--help port default = %q", stdout.String())
	}
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
		filepath.Join("..", "..", "..", "testdata", "providers", "openai"),
	)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "state")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outputReader, outputWriter := io.Pipe()
	exitCode := make(chan int, 1)
	go func() {
		exitCode <- RunContext(ctx, []string{
			"--workspace", workspace,
			"--data-dir", dataDir,
			"--provider-fixture", fixture,
			"--provider", "openai",
			"--model", "fixture-model",
			"--enable-tools=false",
			"--port", "0",
			"--no-open",
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
		"--workspace", secondWorkspace,
		"--data-dir", dataDir,
		"--provider-fixture", fixture,
		"--provider", "openai",
		"--model", "fixture-model",
		"--enable-tools=false",
		"--port", "0",
		"--no-open",
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
	token := fetchSupervisorToken(t, url)
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
			"--workspace", workspace,
			"--data-dir", dataDir,
			"--provider-fixture", fixture,
			"--provider", "openai",
			"--model", "fixture-model",
			"--enable-tools=false",
			"--port", "0",
			"--no-open",
		}, restartWriter, io.Discard)
		_ = restartWriter.Close()
	}()
	restartURL := waitForReadyURL(t, restartReader)
	restartCatalog := fetchWorkspaceCatalog(
		t,
		restartURL,
		fetchSupervisorToken(t, restartURL),
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
		fetchSupervisorToken(t, restartURL),
		secondIdentity.RootID,
	)
	removedCatalog := fetchWorkspaceCatalog(
		t,
		restartURL,
		fetchSupervisorToken(t, restartURL),
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
			"--workspace", workspace,
			"--data-dir", dataDir,
			"--port", "0",
			"--no-open",
		}, outputWriter, io.Discard)
		_ = outputWriter.Close()
	}()

	setupURL := waitForOutputURL(t, outputLines, "QCode Setup Ready: ")
	bootstrapResponse, err := http.Get(strings.TrimSuffix(setupURL, "/") + "/api/v1/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap struct {
		Token         string `json:"token"`
		SetupRequired bool   `json:"setup_required"`
	}
	if err := json.NewDecoder(bootstrapResponse.Body).Decode(&bootstrap); err != nil {
		_ = bootstrapResponse.Body.Close()
		t.Fatal(err)
	}
	_ = bootstrapResponse.Body.Close()
	if !bootstrap.SetupRequired || bootstrap.Token == "" {
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
	setupRequest.Header.Set("Authorization", "Bearer "+bootstrap.Token)
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
		"--workspace", workspace,
		"--data-dir", dataDir,
		"--port", "0",
		"--no-open",
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
	) {
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
				return value
			}
		case <-timer.C:
			t.Fatal("timed out waiting for " + prefix)
		}
	}
}

func fetchSupervisorToken(t *testing.T, rawURL string) string {
	t.Helper()
	response, err := http.Get(
		strings.TrimSuffix(rawURL, "/") + "/api/v1/bootstrap",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var bootstrap struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	return bootstrap.Token
}

func fetchWorkspaceCatalog(
	t *testing.T,
	rawURL string,
	token string,
) webhost.WorkspaceCatalog {
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
		Result webhost.WorkspaceCatalog `json:"result"`
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
	body, err := json.Marshal(webhost.WorkspaceRemoveRequest{
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
		return ready.url
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
