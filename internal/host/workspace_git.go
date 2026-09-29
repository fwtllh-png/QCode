package host

import (
	"net/http"

	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (s *Server) workspaceGitAction(r *http.Request, dependencies Dependencies) (any, error) {
	var request app.GitRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	return dependencies.Runtime.ExecuteGit(r.Context(), request)
}

func (s *Server) workspaceGitStatus(r *http.Request, dependencies Dependencies) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace Git queries are unavailable")
	}
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	result, err := dependencies.Workspace.GitOverview(r.Context())
	if err != nil {
		return nil, workspaceQueryError(err)
	}
	return result, nil
}

func (s *Server) workspaceGitDiff(r *http.Request, dependencies Dependencies) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace Git queries are unavailable")
	}
	var request struct {
		Path   string `json:"path"`
		Staged bool   `json:"staged"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := dependencies.Workspace.GitPatch(r.Context(), request.Path, request.Staged)
	if err != nil {
		return nil, workspaceQueryError(err)
	}
	return result, nil
}

func (s *Server) workspaceGitSwitch(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if dependencies.Workspace == nil {
		return nil, unavailable("workspace Git operations are unavailable")
	}
	var request struct {
		Branch string `json:"branch"`
	}
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	activity := dependencies.Runtime.Snapshot(r.Context())
	if activity.ActiveTurns != 0 || activity.PendingOperations != 0 {
		return nil, protocol.NewProblem(
			protocol.CodeConflict,
			"finish active work before switching branches",
			true,
			nil,
		)
	}
	state, err := dependencies.Runtime.SwitchGitBranch(r.Context(), request.Branch)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeConflict,
			err.Error(),
			false,
			err,
		)
	}
	return state, nil
}
