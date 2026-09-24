package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// ConnectionListVersion 是连接列表投影的协议版本。
const ConnectionListVersion = 1

// ConnectionEntry 是一条已配置模型连接的公开形态（不含密钥）。
type ConnectionEntry struct {
	ID               string   `json:"id"`
	Provider         string   `json:"provider"`
	DisplayName      string   `json:"display_name,omitempty"`
	BaseURL          string   `json:"base_url,omitempty"`
	Protocol         string   `json:"protocol,omitempty"`
	Model            string   `json:"model"`
	Models           []string `json:"models,omitempty"`
	Default          bool     `json:"default,omitempty"`
	CredentialPresent bool    `json:"credential_present"`
}

// ConnectionListResult 是 connection/list 与连接变更操作的统一响应。
type ConnectionListResult struct {
	Version           int                `json:"version"`
	DefaultConnection string             `json:"default_connection"`
	Connections       []ConnectionEntry  `json:"connections"`
}

// ConnectionIDRequest 寻址一条连接。
type ConnectionIDRequest struct {
	ConnectionID string `json:"connection_id"`
}

// ConnectionOptions 由宿主注入的连接管理回调；nil 表示路由不可用。
type ConnectionOptions struct {
	List       func(context.Context) (ConnectionListResult, error)
	Add        func(context.Context, SetupRequest) (ConnectionListResult, error)
	Remove     func(context.Context, string) (ConnectionListResult, error)
	SetDefault func(context.Context, string) (ConnectionListResult, error)
}

func (s *Server) connectionList(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil ||
		s.setup.Connections.List == nil {
		return nil, unavailable("connection management is unavailable")
	}
	return s.setup.Connections.List(r.Context())
}

func (s *Server) connectionAdd(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil ||
		s.setup.Connections.Add == nil {
		return nil, unavailable("connection management is unavailable")
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"Idempotency-Key header is required",
			false,
			nil,
		)
	}
	var request SetupRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.Add(r.Context(), request)
	if err != nil {
		var problem *protocol.Problem
		if errors.As(err, &problem) {
			return nil, err
		}
		return nil, protocol.NewProblem(protocol.CodeUnavailable, err.Error(), true, err)
	}
	return result, nil
}

func (s *Server) connectionRemove(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil ||
		s.setup.Connections.Remove == nil {
		return nil, unavailable("connection management is unavailable")
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"Idempotency-Key header is required",
			false,
			nil,
		)
	}
	var request ConnectionIDRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.Remove(r.Context(), request.ConnectionID)
	if err != nil {
		var problem *protocol.Problem
		if errors.As(err, &problem) {
			return nil, err
		}
		return nil, protocol.NewProblem(protocol.CodeUnavailable, err.Error(), true, err)
	}
	return result, nil
}

func (s *Server) connectionDefault(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil ||
		s.setup.Connections.SetDefault == nil {
		return nil, unavailable("connection management is unavailable")
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument,
			"Idempotency-Key header is required",
			false,
			nil,
		)
	}
	var request ConnectionIDRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.SetDefault(r.Context(), request.ConnectionID)
	if err != nil {
		var problem *protocol.Problem
		if errors.As(err, &problem) {
			return nil, err
		}
		return nil, protocol.NewProblem(protocol.CodeUnavailable, err.Error(), true, err)
	}
	return result, nil
}
