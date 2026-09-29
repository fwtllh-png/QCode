package host

import (
	"context"
	"net/http"
)

// ConnectionListVersion 是连接列表投影的协议版本。
const ConnectionListVersion = 1

// ConnectionEntry 是一条已配置模型连接的公开形态（不含密钥）。
type ConnectionEntry struct {
	ID                string   `json:"id"`
	Provider          string   `json:"provider"`
	DisplayName       string   `json:"display_name,omitempty"`
	BaseURL           string   `json:"base_url,omitempty"`
	Protocol          string   `json:"protocol,omitempty"`
	Model             string   `json:"model"`
	Models            []string `json:"models,omitempty"`
	Default           bool     `json:"default,omitempty"`
	CredentialPresent bool     `json:"credential_present"`
}

// ConnectionListResult 是 connection/list 与连接变更操作的统一响应。
type ConnectionListResult struct {
	Version           int               `json:"version"`
	DefaultConnection string            `json:"default_connection"`
	Connections       []ConnectionEntry `json:"connections"`
}

// ConnectionIDRequest 寻址一条连接。
type ConnectionIDRequest struct {
	ConnectionID string `json:"connection_id"`
}

// ConnectionController 由 Supervisor 直接提供连接管理；nil 表示路由不可用。
type ConnectionController interface {
	ConnectionList() ConnectionListResult
	AddConnection(context.Context, SetupRequest) (ConnectionListResult, error)
	RemoveConnection(context.Context, string) (ConnectionListResult, error)
	SetDefaultConnection(context.Context, string) (ConnectionListResult, error)
}

func (s *Server) connectionList(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil {
		return nil, unavailable("connection management is unavailable")
	}
	return s.setup.Connections.ConnectionList(), nil
}

func (s *Server) connectionAdd(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil {
		return nil, unavailable("connection management is unavailable")
	}
	var request SetupRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.AddConnection(r.Context(), request)
	return result, hostControlError(err)
}

func (s *Server) connectionRemove(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil {
		return nil, unavailable("connection management is unavailable")
	}
	var request ConnectionIDRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.RemoveConnection(r.Context(), request.ConnectionID)
	return result, hostControlError(err)
}

func (s *Server) connectionDefault(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Connections == nil {
		return nil, unavailable("connection management is unavailable")
	}
	var request ConnectionIDRequest
	if err := s.decodeIdempotentRequest(r, &request); err != nil {
		return nil, err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	result, err := s.setup.Connections.SetDefaultConnection(r.Context(), request.ConnectionID)
	return result, hostControlError(err)
}
