package host

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/app/workspacequery"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const WorkspaceCatalogVersion = 1

const workspaceHeader = "X-QCode-Workspace-ID"

type WorkspaceDescriptor struct {
	ID           string                   `json:"id"`
	Root         string                   `json:"root"`
	Label        string                   `json:"label"`
	Ready        bool                     `json:"ready"`
	Removable    bool                     `json:"removable"`
	SessionCount int                      `json:"session_count"`
	Problem      string                   `json:"problem,omitempty"`
	Git          *workspacequery.GitState `json:"git,omitempty"`
}

type WorkspaceCatalog struct {
	Version    int                   `json:"version"`
	Workspaces []WorkspaceDescriptor `json:"workspaces"`
}

type WorkspaceController interface {
	List(context.Context) (WorkspaceCatalog, error)
	Add(context.Context, string) (WorkspaceDescriptor, error)
	Remove(context.Context, string) (WorkspaceCatalog, error)
}

type WorkspaceAddRequest struct {
	Path string `json:"path"`
}

type WorkspaceAddResult struct {
	Workspace WorkspaceDescriptor `json:"workspace"`
}

type WorkspaceRemoveRequest struct {
	WorkspaceID string `json:"workspace_id"`
}

type WorkspaceDirectoryResult struct {
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

func (s *Server) workspaceList(r *http.Request, _ Dependencies) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if s.workspaceControl == nil {
		return nil, unavailable("workspace management is unavailable")
	}
	return s.workspaceControl.List(r.Context())
}

func (s *Server) workspaceAdd(r *http.Request, _ Dependencies) (any, error) {
	if s.workspaceControl == nil {
		return nil, unavailable("workspace management is unavailable")
	}
	var request WorkspaceAddRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	workspace, err := s.workspaceControl.Add(r.Context(), request.Path)
	return WorkspaceAddResult{Workspace: workspace}, hostControlError(err)
}

func (s *Server) workspaceRemove(r *http.Request, _ Dependencies) (any, error) {
	if s.workspaceControl == nil {
		return nil, unavailable("workspace management is unavailable")
	}
	var request WorkspaceRemoveRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	request.WorkspaceID = strings.TrimSpace(request.WorkspaceID)
	if request.WorkspaceID == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"workspace_id is required",
			false,
			nil,
		)
	}
	catalog, err := s.workspaceControl.Remove(r.Context(), request.WorkspaceID)
	return catalog, hostControlError(err)
}

func (s *Server) workspaceSelectDirectory(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	if s.pickDirectory == nil {
		return nil, unavailable("native directory selection is unavailable")
	}
	if !s.directoryPickerMu.TryLock() {
		return nil, protocol.NewProblem(
			protocol.CodeConflict,
			"a directory selection is already open",
			false,
			nil,
		)
	}
	defer s.directoryPickerMu.Unlock()
	initialPath, _ := os.UserHomeDir()
	if s.workspaceControl != nil {
		catalog, err := s.workspaceControl.List(r.Context())
		if err != nil {
			return nil, err
		}
		if len(catalog.Workspaces) > 0 {
			initialPath = catalog.Workspaces[0].Root
		}
	}
	selected, cancelled, err := s.pickDirectory(r.Context(), initialPath)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			err.Error(),
			true,
			err,
		)
	}
	return WorkspaceDirectoryResult{
		Path: selected, Cancelled: cancelled,
	}, nil
}

func (s *Server) workspaceCatalog() WorkspaceCatalog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := WorkspaceCatalog{
		Version:    WorkspaceCatalogVersion,
		Workspaces: make([]WorkspaceDescriptor, 0, len(s.workspaces)),
	}
	for id, dependencies := range s.workspaces {
		result.Workspaces = append(result.Workspaces, WorkspaceDescriptor{
			ID: id, Root: dependencies.WorkspaceRoot,
			Label: filepath.Base(dependencies.WorkspaceRoot), Ready: true,
			Removable: true,
		})
	}
	sort.Slice(result.Workspaces, func(i, j int) bool {
		return result.Workspaces[i].Root < result.Workspaces[j].Root
	})
	return result
}

func (s *Server) workspaceSnapshot(
	workspaceID string,
) (Dependencies, *protocol.Problem, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return Dependencies{}, s.bootProblem, false
	}
	dependencies, found := s.workspaces[workspaceID]
	return dependencies, s.bootProblem, found
}
