package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
)

type Config struct {
	Protocol                 model.WireProtocol `json:"protocol"`
	Path                     string             `json:"path"`
	Model                    string             `json:"model"`
	ExpectedPrompt           string             `json:"expected_prompt,omitempty"`
	ExpectedRequestFragments [][]string         `json:"expected_request_fragments,omitempty"`
	Streams                  []string           `json:"streams,omitempty"`
	Routes                   []Route            `json:"routes,omitempty"`
	StreamDelayMS            int                `json:"stream_delay_ms,omitempty"`
	CachedInputRatio         float64            `json:"cached_input_ratio,omitempty"`
}

type Route struct {
	Match   []string `json:"match"`
	Streams []string `json:"streams"`
}

type loadedRoute struct {
	match   []string
	streams [][]byte
	index   atomic.Uint64
}

type Server struct {
	URL      string
	Config   Config
	server   *http.Server
	listener net.Listener
	done     chan error
}

var (
	agentIDPattern       = regexp.MustCompile(`agent-[0-9]+`)
	previewDigestPattern = regexp.MustCompile(
		`preview_digest[^0-9a-fA-F]{1,32}([0-9a-fA-F]{64})`,
	)
)

func Start(directory string) (*Server, error) {
	configData, err := os.ReadFile(filepath.Join(directory, "fixture.json"))
	if err != nil {
		return nil, fmt.Errorf("read provider fixture config: %w", err)
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(configData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode provider fixture config: %w", err)
	}
	if config.Path == "" || config.Path[0] != '/' || config.Model == "" {
		return nil, errors.New("provider fixture path and model are required")
	}
	if config.StreamDelayMS < 0 {
		return nil, errors.New("provider fixture stream_delay_ms must not be negative")
	}
	if config.CachedInputRatio < 0 || config.CachedInputRatio > 1 {
		return nil, errors.New("provider fixture cached_input_ratio must be between zero and one")
	}
	switch config.Protocol {
	case model.ProtocolOpenAIChat, model.ProtocolOpenAIResponses:
	default:
		return nil, fmt.Errorf("unsupported provider fixture protocol %q", config.Protocol)
	}
	streamData, err := loadStreams(directory, config.Streams)
	if err != nil {
		return nil, err
	}
	routes := make([]loadedRoute, 0, len(config.Routes))
	for index, route := range config.Routes {
		if len(route.Match) == 0 || len(route.Streams) == 0 {
			return nil, fmt.Errorf(
				"provider fixture route %d requires match and streams", index,
			)
		}
		streams, loadErr := loadStreams(directory, route.Streams)
		if loadErr != nil {
			return nil, fmt.Errorf("provider fixture route %d: %w", index, loadErr)
		}
		routes = append(routes, loadedRoute{
			match: append([]string(nil), route.Match...), streams: streams,
		})
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for provider fixture: %w", err)
	}
	result := &Server{
		URL:      "http://" + listener.Addr().String(),
		Config:   config,
		listener: listener,
		done:     make(chan error, 1),
	}
	mux := http.NewServeMux()
	var requestIndex atomic.Uint64
	mux.HandleFunc(config.Path, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			http.Error(writer, "POST required", http.StatusMethodNotAllowed)
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if readErr != nil {
			http.Error(writer, "read request", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil || payload["model"] != config.Model || payload["stream"] != true {
			http.Error(writer, "unexpected provider request", http.StatusBadRequest)
			return
		}
		if config.ExpectedPrompt != "" && !strings.Contains(string(body), config.ExpectedPrompt) {
			http.Error(writer, "expected prompt missing", http.StatusBadRequest)
			return
		}
		selected, selectedIndex := streamData, &requestIndex
		for index := range routes {
			if containsAll(string(body), routes[index].match) {
				selected, selectedIndex = routes[index].streams, &routes[index].index
				break
			}
		}
		index := int(selectedIndex.Add(1) - 1)
		if index >= len(selected) {
			http.Error(writer, "provider fixture streams exhausted", http.StatusConflict)
			return
		}
		if selectedIndex == &requestIndex && index < len(config.ExpectedRequestFragments) {
			for _, fragment := range config.ExpectedRequestFragments[index] {
				if !strings.Contains(string(body), fragment) {
					http.Error(writer, "expected request fragment missing: "+fragment, http.StatusBadRequest)
					return
				}
			}
		}
		stream, expandErr := expandStream(selected[index], payload, config.CachedInputRatio)
		if expandErr != nil {
			http.Error(writer, expandErr.Error(), http.StatusConflict)
			return
		}

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, line := range strings.SplitAfter(string(stream), "\n") {
			if config.StreamDelayMS > 0 {
				time.Sleep(time.Duration(config.StreamDelayMS) * time.Millisecond)
			}
			if _, writeErr := io.WriteString(writer, line); writeErr != nil {
				return
			}
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	})
	result.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		result.done <- result.server.Serve(listener)
	}()
	return result, nil
}

func expandStream(stream []byte, payload map[string]any, cachedRatio float64) ([]byte, error) {
	rendered := string(stream)
	inputTokens := estimateInputTokens(payload)
	rendered = strings.NewReplacer(
		"{{request_input_tokens}}", fmt.Sprint(inputTokens),
		"{{request_cached_tokens}}", fmt.Sprint(uint64(float64(inputTokens)*cachedRatio)),
	).Replace(rendered)
	for token, pattern := range map[string]*regexp.Regexp{
		"{{agent_id}}":       agentIDPattern,
		"{{preview_digest}}": previewDigestPattern,
	} {
		if !strings.Contains(rendered, token) {
			continue
		}
		value, ok := latestMatch(payload["messages"], pattern)
		if !ok {
			return nil, fmt.Errorf("provider fixture placeholder %s has no request value", token)
		}
		rendered = strings.ReplaceAll(rendered, token, value)
	}
	return []byte(rendered), nil
}

func estimateInputTokens(payload map[string]any) uint64 {
	visible := make(map[string]any)
	for _, key := range []string{"input", "messages", "system", "tools"} {
		if value, ok := payload[key]; ok {
			visible[key] = value
		}
	}
	encoded, _ := json.Marshal(visible)
	return uint64((len(encoded) + 3) / 4)
}

func latestMatch(value any, pattern *regexp.Regexp) (string, bool) {
	switch typed := value.(type) {
	case []any:
		for index := len(typed) - 1; index >= 0; index-- {
			if match, ok := latestMatch(typed[index], pattern); ok {
				return match, true
			}
		}
	case map[string]any:
		if content, ok := typed["content"]; ok {
			if match, found := latestMatch(content, pattern); found {
				return match, true
			}
		}
		for _, item := range typed {
			if match, ok := latestMatch(item, pattern); ok {
				return match, true
			}
		}
	case string:
		matches := pattern.FindAllStringSubmatch(typed, -1)
		if len(matches) == 0 {
			return "", false
		}
		match := matches[len(matches)-1]
		return match[len(match)-1], true
	}
	return "", false
}

func loadStreams(directory string, names []string) ([][]byte, error) {
	if len(names) == 0 {
		names = []string{"stream.sse"}
	}
	streams := make([][]byte, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, fmt.Errorf("read provider fixture stream: %w", err)
		}
		streams = append(streams, data)
	}
	return streams, nil
}

func containsAll(value string, fragments []string) bool {
	for _, fragment := range fragments {
		if fragment == "" || !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func (s *Server) Close(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	shutdownErr := s.server.Shutdown(ctx)
	select {
	case serveErr := <-s.done:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return errors.Join(shutdownErr, serveErr)
		}
	default:
	}
	return shutdownErr
}
