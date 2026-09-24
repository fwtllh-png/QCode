package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type ModelMutationRequest struct {
	Model         string             `json:"model"`
	ModelMetadata SetupModelMetadata `json:"model_metadata"`
}

type ModelRemoveRequest struct {
	Model string `json:"model"`
}

type ModelController interface {
	ProbeModel(context.Context, string) (SetupProbeResult, error)
	AddModel(context.Context, ModelMutationRequest) (protocol.ModelCatalog, error)
	UpdateModel(context.Context, ModelMutationRequest) (protocol.ModelCatalog, error)
	RemoveModel(context.Context, string) (protocol.ModelCatalog, error)
}

func (s *Server) modelProbe(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if s.modelControl == nil {
		return nil, unavailable("model management is unavailable")
	}
	var request ModelRemoveRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := s.modelControl.ProbeModel(r.Context(), request.Model)
	if err != nil {
		var problem *protocol.Problem
		if errors.As(err, &problem) {
			return nil, err
		}
		// 探测失败（网络、凭证、端点响应）面向用户展示真实原因，
		// 不落成不可读的 internal Web API error。
		return nil, protocol.NewProblem(protocol.CodeUnavailable, err.Error(), true, err)
	}
	return result, nil
}

type ModelTestResult struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Status   string    `json:"status"`
	Detail   string    `json:"detail"`
	TestedAt time.Time `json:"tested_at"`
}

type WorkspaceConnection struct {
	Provider      string              `json:"provider"`
	Endpoint      string              `json:"endpoint"`
	Protocol      string              `json:"protocol"`
	ModelMetadata *SetupModelMetadata `json:"model_metadata,omitempty"`
}

func (s *Server) providerList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	return dependencies.ProviderCatalog, nil
}

func (s *Server) modelList(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	result := dependencies.ModelCatalog
	sessions, err := dependencies.Runtime.ListSessions(
		r.Context(),
		protocol.SessionListQuery{
			WorkspaceRoot:   dependencies.WorkspaceRoot,
			IncludeArchived: true,
			Limit:           s.capacity.MaxActiveSessions,
		},
	)
	if err != nil {
		return protocol.ModelCatalog{}, err
	}
	seen := make(map[string]bool, len(result.Models))
	for _, entry := range result.Models {
		seen[model.RouteKey(entry.Provider, entry.ID)] = true
	}
	for _, session := range sessions.Sessions {
		snapshot, profileErr := dependencies.Runtime.SessionProfile(
			r.Context(),
			session.SessionID,
		)
		if profileErr != nil {
			continue
		}
		key := model.RouteKey(snapshot.Profile.Provider, snapshot.Profile.Model)
		if seen[key] {
			continue
		}
		seen[key] = true
		result.Models = append(result.Models, protocol.ModelCatalogEntry{
			Provider:     snapshot.Profile.Provider,
			ID:           snapshot.Profile.Model,
			Source:       "connection_baseline",
			Capabilities: snapshot.Capabilities.ModelCapabilities,
		})
	}
	sort.Slice(result.Models, func(i, j int) bool {
		if result.Models[i].Provider == result.Models[j].Provider {
			return result.Models[i].ID < result.Models[j].ID
		}
		return result.Models[i].Provider < result.Models[j].Provider
	})
	return result, nil
}

func (s *Server) modelTest(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	var request struct {
		Model string `json:"model"`
	}
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" || len(request.Model) > 256 ||
		strings.ContainsAny(request.Model, "\x00\r\n\t ") {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"model id is invalid",
			false,
			nil,
		)
	}
	if dependencies.ModelProbe == nil {
		return nil, unavailable("model testing is unavailable")
	}
	available, err := dependencies.ModelProbe(r.Context(), request.Model)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			err.Error(),
			true,
			err,
		)
	}
	result := ModelTestResult{
		Provider: dependencies.DefaultProfile.Provider,
		Model:    request.Model,
		Status:   "not_listed",
		Detail:   "Connection succeeded, but the provider did not list this model",
		TestedAt: time.Now().UTC(),
	}
	if available {
		result.Status = "available"
		result.Detail = "Connection succeeded and the provider listed this model"
	}
	return result, nil
}

func (s *Server) modelAdd(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if s.modelControl == nil {
		return nil, unavailable("model management is unavailable")
	}
	var request ModelMutationRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return s.modelControl.AddModel(r.Context(), request)
}

func (s *Server) modelUpdate(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if s.modelControl == nil {
		return nil, unavailable("model management is unavailable")
	}
	var request ModelMutationRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return s.modelControl.UpdateModel(r.Context(), request)
}

func (s *Server) modelRemove(
	r *http.Request,
	_ Dependencies,
) (any, error) {
	if s.modelControl == nil {
		return nil, unavailable("model management is unavailable")
	}
	var request ModelRemoveRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	return s.modelControl.RemoveModel(r.Context(), request.Model)
}

func (s *Server) connectionStatus(
	r *http.Request,
	dependencies Dependencies,
) (any, error) {
	if err := s.decodeRequest(r, &struct{}{}); err != nil {
		return nil, err
	}
	return dependencies.Connection, nil
}
