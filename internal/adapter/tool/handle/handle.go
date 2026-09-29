// Package handle owns var_handle storage and the model-visible handle_read tool.
// Producers such as subagent transcripts insert payloads; the model retrieves
// bounded projections without copying the full body into the parent transcript.
package handle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

const (
	KindVarHandle   = "var_handle"
	DefaultMaxBytes = 12 << 10
	HardMaxBytes    = 50 << 10
	previewBytes    = 160
)

type VarHandle struct {
	Kind        string `json:"kind"`
	SessionID   string `json:"session_id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Length      int    `json:"length"`
	ReprPreview string `json:"repr_preview"`
	SHA256      string `json:"sha256"`
}

type Key struct {
	SessionID string
	Name      string
}

func (h VarHandle) Key() Key {
	return Key{SessionID: h.SessionID, Name: h.Name}
}

type valueKind int

const (
	valueText valueKind = iota
	valueJSON
)

type record struct {
	handle VarHandle
	kind   valueKind
	text   string
	raw    json.RawMessage
}

// Store holds session-scoped symbolic payloads for handle_read.
type Store struct {
	mu      sync.RWMutex
	records map[Key]record
}

func NewStore() *Store {
	return &Store{records: make(map[Key]record)}
}

func (s *Store) PutText(sessionID, name, text string) (VarHandle, error) {
	sessionID = strings.TrimSpace(sessionID)
	name = strings.TrimSpace(name)
	if sessionID == "" || name == "" {
		return VarHandle{}, errors.New("session_id and name are required")
	}
	handle := makeHandle(sessionID, name, "str", utf8.RuneCountInString(text), text, []byte(text))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[handle.Key()] = record{handle: handle, kind: valueText, text: text}
	return handle, nil
}

func (s *Store) PutJSON(sessionID, name string, value any) (VarHandle, error) {
	sessionID = strings.TrimSpace(sessionID)
	name = strings.TrimSpace(name)
	if sessionID == "" || name == "" {
		return VarHandle{}, errors.New("session_id and name are required")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return VarHandle{}, err
	}
	typeName, length := jsonMeta(value)
	handle := makeHandle(sessionID, name, typeName, length, string(raw), raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[handle.Key()] = record{handle: handle, kind: valueJSON, text: string(raw), raw: raw}
	return handle, nil
}

func (s *Store) Get(handle VarHandle) (VarHandle, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.records[handle.Key()]
	if !ok {
		return VarHandle{}, "", false
	}
	return item.handle, item.text, true
}

func makeHandle(sessionID, name, typeName string, length int, previewSource string, digest []byte) VarHandle {
	sum := sha256.Sum256(digest)
	return VarHandle{
		Kind: KindVarHandle, SessionID: sessionID, Name: name, Type: typeName,
		Length: length, ReprPreview: truncateRunes(previewSource, previewBytes),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

func jsonMeta(value any) (string, int) {
	switch typed := value.(type) {
	case nil:
		return "null", 0
	case []any:
		return "list", len(typed)
	case map[string]any:
		return "dict", len(typed)
	case string:
		return "str", utf8.RuneCountInString(typed)
	case bool:
		return "bool", 1
	case float64, json.Number, int, int64:
		return "number", 1
	default:
		encoded, _ := json.Marshal(value)
		return "json", utf8.RuneCountInString(string(encoded))
	}
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

// ReadTool is the model-visible handle_read executor.
type ReadTool struct {
	Store *Store
}

type readInput struct {
	Handle    json.RawMessage `json:"handle"`
	Mode      string          `json:"mode"`
	StartLine int             `json:"start_line"`
	MaxLines  int             `json:"max_lines"`
	Query     string          `json:"query"`
	Offset    int             `json:"offset"`
	MaxBytes  int             `json:"max_bytes"`
}

func Register(registry *tool.Registry, store *Store) error {
	if registry == nil {
		return errors.New("handle_read registry is required")
	}
	if store == nil {
		store = NewStore()
	}
	instance := &ReadTool{Store: store}
	executor, err := instance.typedExecutor()
	if err != nil {
		return err
	}
	return registry.Register(executor)
}

func (*ReadTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "handle_read",
		Description: "Read a bounded projection from a var_handle returned by tools such as " +
			"sub-agents. This does not read artifact ids, tool-call ids, SHA refs, " +
			"or files; use result_get for spilled tool results and file_read for workspace files.",
		Visibility: tool.VisibleModel,
		Capability: tool.CapabilityRead, AccessMode: tool.AccessRead,
		ParallelPolicy:     tool.ParallelConcurrent,
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxNone,
		Availability:       tool.AvailabilityAvailable,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "var_handle", Field: "handle", Access: tool.AccessRead,
		}}},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"handle": map[string]any{
					"description": "A var_handle object (preferred), or compact session_id/name string; " +
						"name may contain additional '/' path separators",
				},
				"mode": map[string]any{
					"type": "string",
					"enum": []any{"metadata", "summary", "head", "tail", "lines", "query", "bytes", "count"},
				},
				"start_line": map[string]any{"type": "integer"},
				"max_lines":  map[string]any{"type": "integer"},
				"query":      map[string]any{"type": "string"},
				"offset":     map[string]any{"type": "integer"},
				"max_bytes":  map[string]any{"type": "integer"},
			},
			"required":             []string{"handle"},
			"additionalProperties": false,
		},
	}
}

func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	executor, err := t.typedExecutor()
	if err != nil {
		return tool.Result{}, err
	}
	return executor.Execute(ctx, raw)
}

func (t *ReadTool) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[readInput, tool.Result]{
		Descriptor:  t.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run: func(_ context.Context, value readInput) (tool.Result, error) {
			return t.read(value)
		},
		Encode: func(value tool.Result) (tool.Result, error) {
			return value, nil
		},
	})
}

func (t *ReadTool) read(input readInput) (tool.Result, error) {
	if t == nil || t.Store == nil {
		return tool.Result{}, errors.New("handle store is required")
	}
	handle, err := parseHandle(input.Handle)
	if err != nil {
		return tool.Result{}, err
	}
	stored, body, ok := t.Store.Get(handle)
	if !ok {
		return tool.Result{}, fmt.Errorf("handle_read: no payload for %s/%s", handle.SessionID, handle.Name)
	}
	if handle.SHA256 != "" && handle.SHA256 != stored.SHA256 {
		return tool.Result{}, errors.New("handle_read: handle sha256 does not match stored payload")
	}
	mode := input.Mode
	if mode == "" {
		mode = "summary"
	}
	if input.Offset < 0 || input.StartLine < 0 || input.MaxLines < 0 || input.MaxBytes < 0 {
		return tool.Result{}, errors.New("offset and limits must not be negative")
	}
	limit := DefaultMaxBytes
	if input.MaxBytes > 0 {
		limit = min(HardMaxBytes, input.MaxBytes)
	} else {
		limit = min(HardMaxBytes, DefaultMaxBytes)
	}
	metadata := map[string]any{
		"mode": mode, "kind": stored.Kind, "session_id": stored.SessionID, "name": stored.Name,
		"type": stored.Type, "length": stored.Length, "sha256": stored.SHA256,
		"total_bytes": len(body), "hard_cap_bytes": HardMaxBytes,
	}
	if mode == "count" || mode == "metadata" {
		content, err := json.Marshal(metadata)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.Result{Content: string(content), Metadata: metadata}, nil
	}
	if mode == "query" && input.Query == "" {
		return tool.Result{}, errors.New("query mode requires query")
	}
	page, err := tool.ProjectPage(body, tool.PageRequest{
		Mode: mode, StartLine: input.StartLine, MaxLines: input.MaxLines,
		Query: input.Query, Offset: input.Offset, Limit: limit,
	})
	if err != nil {
		return tool.Result{}, fmt.Errorf("handle_read: %w", err)
	}
	for key, value := range page.Cursor {
		metadata[key] = value
	}
	metadata["excerpt_bytes"] = len(page.Excerpt)
	metadata["truncated"] = page.More
	return tool.Result{
		Content: page.Excerpt, Metadata: metadata, Truncated: page.More,
		OriginalBytes: len(body),
	}, nil
}

func parseHandle(raw json.RawMessage) (VarHandle, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return VarHandle{}, errors.New("handle is required")
	}
	if raw[0] == '"' {
		var compact string
		if err := json.Unmarshal(raw, &compact); err != nil {
			return VarHandle{}, err
		}
		sessionID, name, ok := strings.Cut(compact, "/")
		if !ok || sessionID == "" || name == "" {
			return VarHandle{}, errors.New("handle string must be session_id/name")
		}
		return VarHandle{Kind: KindVarHandle, SessionID: sessionID, Name: name}, nil
	}
	var handle VarHandle
	if err := json.Unmarshal(raw, &handle); err != nil {
		return VarHandle{}, err
	}
	if handle.Kind != "" && handle.Kind != KindVarHandle {
		return VarHandle{}, fmt.Errorf("unsupported handle kind %q", handle.Kind)
	}
	handle.Kind = KindVarHandle
	if handle.SessionID == "" || handle.Name == "" {
		return VarHandle{}, errors.New("handle requires session_id and name")
	}
	return handle, nil
}

// Keys returns sorted session/name keys for tests.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		keys = append(keys, key.SessionID+"/"+key.Name)
	}
	sort.Strings(keys)
	return keys
}
