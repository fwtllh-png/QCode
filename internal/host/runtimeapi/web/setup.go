package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// SetupRequest 声明一条 OpenAI-compatible 模型连接的四要素：Base URL、
// Protocol、Model ID、API Key（外加探测或手填的模型元数据）。
type SetupRequest struct {
	Model         string              `json:"model"`
	APIKey        string              `json:"api_key,omitempty"`
	BaseURL       string              `json:"base_url"`
	Protocol      string              `json:"protocol,omitempty"`
	ModelMetadata *SetupModelMetadata `json:"model_metadata,omitempty"`
}

type SetupModelMetadata struct {
	CanonicalID     string                 `json:"canonical_id"`
	WireID          string                 `json:"wire_id"`
	ContextTokens   uint64                 `json:"context_tokens"`
	MaxOutputTokens uint64                 `json:"max_output_tokens"`
	Capabilities    SetupModelCapabilities `json:"capabilities"`
}

type SetupModelCapabilities struct {
	Streaming              *bool    `json:"streaming"`
	Reasoning              *bool    `json:"reasoning"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
	ToolCalls              *bool    `json:"tool_calls"`
	NativeSearch           *bool    `json:"native_search"`
	IncrementalResponses   *bool    `json:"incremental_responses"`
	Vision                 *bool    `json:"vision"`
	ImageInput             *bool    `json:"image_input"`
	PromptCache            *bool    `json:"prompt_cache"`
	AutomaticPromptCache   *bool    `json:"automatic_prompt_cache"`
	ThinkingToggle         *bool    `json:"thinking_toggle"`
}

type SetupResult struct {
	Ready bool `json:"ready"`
}

type SetupProbeRequest struct {
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
}

type SetupProbeResult struct {
	Models       []SetupDiscoveredModel `json:"models,omitempty"`
	Capabilities SetupModelCapabilities `json:"capabilities"`
	Warning      string                 `json:"warning,omitempty"`
}

type SetupDiscoveredModel struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	ContextTokens   uint64 `json:"context_tokens,omitempty"`
	MaxOutputTokens uint64 `json:"max_output_tokens,omitempty"`
}

type SetupOptions struct {
	WorkspaceRoot     string
	WorkspaceIdentity protocol.WorkspaceIdentity
	Apply             func(context.Context, SetupRequest) error
	Probe             func(context.Context, SetupProbeRequest) (SetupProbeResult, error)
	Connections       *ConnectionOptions
}

func (o SetupOptions) validate() error {
	if o.WorkspaceRoot != "" || o.WorkspaceIdentity != (protocol.WorkspaceIdentity{}) {
		if strings.TrimSpace(o.WorkspaceRoot) == "" {
			return errors.New("setup workspace root is required with a workspace identity")
		}
		if err := o.WorkspaceIdentity.Validate(); err != nil {
			return err
		}
	}
	if o.Apply == nil {
		return errors.New("setup apply handler is required")
	}
	return nil
}

func (s *Server) setupProbe(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil || s.setup.Probe == nil {
		return nil, unavailable("Runtime setup probe is unavailable")
	}
	var request SetupProbeRequest
	if err := s.decodeRequest(r, &request); err != nil {
		return nil, err
	}
	result, err := s.setup.Probe(r.Context(), request)
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

func (s *Server) setupApply(r *http.Request, _ Dependencies) (any, error) {
	if s.setup == nil {
		return nil, unavailable("Runtime setup is unavailable")
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
	if err := s.setup.Apply(r.Context(), request); err != nil {
		var problem *protocol.Problem
		if errors.As(err, &problem) {
			return nil, err
		}
		return nil, protocol.NewProblem(
			protocol.CodeUnavailable,
			err.Error(),
			true,
			err,
		)
	}
	return SetupResult{Ready: s.ready.Load()}, nil
}
