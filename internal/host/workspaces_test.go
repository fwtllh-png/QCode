package host

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type workspaceControllerFixture struct {
	catalog   WorkspaceCatalog
	removedID *string
}

func (f workspaceControllerFixture) List(context.Context) (WorkspaceCatalog, error) {
	return f.catalog, nil
}

func (workspaceControllerFixture) Add(
	context.Context,
	string,
) (WorkspaceDescriptor, error) {
	return WorkspaceDescriptor{}, nil
}

func (f workspaceControllerFixture) Remove(
	_ context.Context,
	workspaceID string,
) (WorkspaceCatalog, error) {
	if f.removedID != nil {
		*f.removedID = workspaceID
	}
	return f.catalog, nil
}

func TestWorkspaceRemoveAllowsSelectedWorkspaceAndDelegates(t *testing.T) {
	const host = "127.0.0.1:43210"
	var removedID string
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Token:        "token",
		Workspaces: workspaceControllerFixture{
			catalog: WorkspaceCatalog{
				Version:    1,
				Workspaces: []WorkspaceDescriptor{{ID: "default", Ready: true}},
			},
			removedID: &removedID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selectedRequest := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/remove",
		strings.NewReader(`{"workspace_id":"secondary"}`),
	)
	selectedRequest.Host = host
	selectedRequest.Header.Set("Content-Type", "application/json")
	selectedRequest.Header.Set("Authorization", "Bearer token")
	selectedRequest.Header.Set("Idempotency-Key", "remove-selected")
	selectedRequest.Header.Set(workspaceHeader, "secondary")
	selectedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(selectedResponse, selectedRequest)
	if selectedResponse.Code != http.StatusOK || removedID != "secondary" {
		t.Fatalf(
			"selected removal status=%d removed=%q body=%s",
			selectedResponse.Code,
			removedID,
			selectedResponse.Body.String(),
		)
	}

	missingIdentity := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/remove",
		strings.NewReader(`{"workspace_id":"secondary"}`),
	)
	missingIdentity.Host = host
	missingIdentity.Header.Set("Content-Type", "application/json")
	missingIdentity.Header.Set("Authorization", "Bearer token")
	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, missingIdentity)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("missing idempotency status=%d body=%s", rejected.Code, rejected.Body)
	}
}

func TestServerAllowsRemovingEveryWorkspace(t *testing.T) {
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: "127.0.0.1:43210",
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeA := app.NewRuntime(app.Options{})
	runtimeB := app.NewRuntime(app.Options{})
	t.Cleanup(func() {
		_ = runtimeA.Close(context.Background())
		_ = runtimeB.Close(context.Background())
	})
	identityA, err := protocol.NewWorkspaceIdentity(
		"file:///workspace/a", "/workspace/a", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	identityB, err := protocol.NewWorkspaceIdentity(
		"file:///workspace/b", "/workspace/b", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(Dependencies{
		Runtime: runtimeA, WorkspaceRoot: "/workspace/a",
		WorkspaceIdentity: identityA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.AddWorkspace(Dependencies{
		Runtime: runtimeB, WorkspaceRoot: "/workspace/b",
		WorkspaceIdentity: identityB,
	}); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range server.workspaceCatalog().Workspaces {
		if !workspace.Removable {
			t.Fatalf("Workspace is not removable: %+v", workspace)
		}
	}

	if err := server.RemoveWorkspace(identityA.RootID); err != nil {
		t.Fatal(err)
	}
	if _, _, found := server.workspaceSnapshot(identityA.RootID); found {
		t.Fatal("removed initial Workspace remained routable")
	}
	if dependencies, _, found := server.workspaceSnapshot(identityB.RootID); !found || dependencies.WorkspaceRoot != "/workspace/b" {
		t.Fatalf("replacement Workspace = %+v, found=%v", dependencies, found)
	}
	if err := server.RemoveWorkspace(identityB.RootID); err != nil {
		t.Fatal(err)
	}
	if catalog := server.workspaceCatalog(); len(catalog.Workspaces) != 0 {
		t.Fatalf("Workspace catalog after final removal = %+v", catalog)
	}
	if _, _, found := server.workspaceSnapshot(""); found {
		t.Fatal("empty Workspace identity resolved implicitly")
	}
}

func TestWorkspaceSelectDirectoryUsesNativePicker(t *testing.T) {
	const host = "127.0.0.1:43210"
	var initialPath string
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Token:        "token",
		Workspaces: workspaceControllerFixture{catalog: WorkspaceCatalog{
			Version: 1,
			Workspaces: []WorkspaceDescriptor{{
				ID: "current", Root: "/workspace/current", Ready: true,
			}},
		}},
		PickDirectory: func(
			_ context.Context,
			initial string,
		) (string, bool, error) {
			initialPath = initial
			return "/workspace/selected", false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/select-directory",
		strings.NewReader(`{}`),
	)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"path":"/workspace/selected"`) ||
		initialPath != "/workspace/current" {
		t.Fatalf(
			"status=%d initial=%q body=%s",
			response.Code,
			initialPath,
			response.Body.String(),
		)
	}
}

func TestWorkspaceSelectDirectoryReportsCancellation(t *testing.T) {
	const host = "127.0.0.1:43210"
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Token:        "token",
		Workspaces:   workspaceControllerFixture{},
		PickDirectory: func(
			context.Context,
			string,
		) (string, bool, error) {
			return "", true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/select-directory",
		strings.NewReader(`{}`),
	)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"cancelled":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkspaceSelectDirectoryRejectsConcurrentPicker(t *testing.T) {
	const host = "127.0.0.1:43210"
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host,
		Token:        "token",
		Workspaces:   workspaceControllerFixture{},
		PickDirectory: func(
			context.Context,
			string,
		) (string, bool, error) {
			return "", true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.directoryPickerMu.Lock()
	defer server.directoryPickerMu.Unlock()
	request := httptest.NewRequest(
		http.MethodPost,
		"http://"+host+"/api/v1/workspace/select-directory",
		strings.NewReader(`{}`),
	)
	request.Host = host
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
