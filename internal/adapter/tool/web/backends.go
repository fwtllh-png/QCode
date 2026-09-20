package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

const (
	defaultTavilyURL  = "https://api.tavily.com/search"
	defaultBochaURL   = "https://api.bochaai.com/v1/web-search"
	defaultSearXNGURL = "http://127.0.0.1:8080"
)

// Options configures web tools (search backends + optional browser runtime).
type Options struct {
	SearchBackend string
	SearchURL     string
	PrimaryURL    string
	FallbackURL   string
	TavilyURL     string
	TavilyAPIKey  string
	SearXNGURL    string
	BochaURL      string
	BochaAPIKey   string
	Browser       BrowserRuntime
	// HTTP, when set, is used for fetch/search RoundTrips (typically an
	// egress-wrapped client). Nil keeps the historical per-call http.Client.
	HTTP *http.Client
}

func OptionsFromEnv() Options {
	return Options{
		SearchBackend: strings.ToLower(strings.TrimSpace(os.Getenv("QCODE_WEB_SEARCH_BACKEND"))),
		SearchURL:     os.Getenv("QCODE_WEB_SEARCH_URL"),
		PrimaryURL:    envOrDefault("QCODE_WEB_SEARCH_PRIMARY_URL", defaultDDGURL),
		FallbackURL:   envOrDefault("QCODE_WEB_SEARCH_FALLBACK_URL", defaultBingURL),
		TavilyURL:     envOrDefault("QCODE_TAVILY_URL", defaultTavilyURL),
		TavilyAPIKey:  os.Getenv("QCODE_TAVILY_API_KEY"),
		SearXNGURL:    strings.TrimRight(envOrDefault("QCODE_SEARXNG_URL", defaultSearXNGURL), "/"),
		BochaURL:      envOrDefault("QCODE_BOCHA_URL", defaultBochaURL),
		BochaAPIKey:   os.Getenv("QCODE_BOCHA_API_KEY"),
		Browser:       browserRuntimeFromEnv(),
	}
}

func (t *Tool) resolveBackends() ([]searchBackend, error) {
	backend := strings.ToLower(strings.TrimSpace(t.searchBackend))
	if t.searchURL != "" || backend == "custom" {
		endpoint := t.searchURL
		if endpoint == "" {
			return nil, fmt.Errorf("custom web search requires QCODE_WEB_SEARCH_URL")
		}
		return []searchBackend{{
			name: "custom", endpoint: endpoint, format: "json", supportsRecency: true,
		}}, nil
	}
	switch backend {
	case "tavily":
		if t.tavilyAPIKey == "" {
			return nil, fmt.Errorf("tavily backend requires QCODE_TAVILY_API_KEY")
		}
		return []searchBackend{{
			name: "tavily", endpoint: t.tavilyURL, format: "tavily",
			apiKey: t.tavilyAPIKey, method: http.MethodPost, supportsRecency: true,
		}}, nil
	case "searxng":
		return []searchBackend{{
			name: "searxng", endpoint: t.searxngURL + "/search", format: "searxng",
			method: http.MethodGet, supportsRecency: false,
		}}, nil
	case "bocha":
		if t.bochaAPIKey == "" {
			return nil, fmt.Errorf("bocha backend requires QCODE_BOCHA_API_KEY")
		}
		return []searchBackend{{
			name: "bocha", endpoint: t.bochaURL, format: "bocha",
			apiKey: t.bochaAPIKey, method: http.MethodPost, supportsRecency: false,
		}}, nil
	case "duckduckgo":
		primary := t.primaryURL
		if primary == "" {
			primary = defaultDDGURL
		}
		return []searchBackend{{
			name: "duckduckgo", endpoint: primary, format: "duckduckgo", supportsRecency: true,
		}}, nil
	case "bing":
		fallback := t.fallbackURL
		if fallback == "" {
			fallback = defaultBingURL
		}
		return []searchBackend{{
			name: "bing", endpoint: fallback, format: "bing", supportsRecency: false,
		}}, nil
	case "":
		primary := t.primaryURL
		if primary == "" {
			primary = defaultDDGURL
		}
		fallback := t.fallbackURL
		if fallback == "" {
			fallback = defaultBingURL
		}
		return []searchBackend{
			{name: "duckduckgo", endpoint: primary, format: "duckduckgo", supportsRecency: true},
			{name: "bing", endpoint: fallback, format: "bing", supportsRecency: false},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported web search backend %q", backend)
	}
}

type searchBackend struct {
	name            string
	endpoint        string
	format          string
	method          string
	apiKey          string
	supportsRecency bool
}

func searchBackendRequest(
	ctx context.Context,
	backend searchBackend,
	value input,
	limit int,
	httpClient *http.Client,
) ([]searchResult, int, *tool.Result, string, error) {
	method := backend.method
	if method == "" {
		method = http.MethodGet
	}
	var (
		body       []byte
		response   *http.Response
		failure    *tool.Result
		err        error
		statusCode int
	)
	switch backend.format {
	case "tavily":
		payload := map[string]any{
			"api_key": backend.apiKey, "query": value.Query, "max_results": limit,
		}
		if len(value.Domains) > 0 {
			payload["include_domains"] = value.Domains
		}
		if value.Recency != "" {
			payload["days"] = map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}[value.Recency]
		}
		body, response, failure, err = requestJSON(ctx, method, backend.endpoint, payload, nil, value.TimeoutMS, httpClient)
	case "bocha":
		payload := map[string]any{"query": value.Query, "count": limit}
		headers := map[string]string{"Authorization": "Bearer " + backend.apiKey}
		body, response, failure, err = requestJSON(ctx, method, backend.endpoint, payload, headers, value.TimeoutMS, httpClient)
	case "searxng":
		endpoint, parseErr := url.Parse(backend.endpoint)
		if parseErr != nil {
			return nil, 0, nil, "", fmt.Errorf("invalid searxng endpoint: %w", parseErr)
		}
		query := endpoint.Query()
		q := value.Query
		for _, domain := range value.Domains {
			q += " site:" + domain
		}
		query.Set("q", q)
		query.Set("format", "json")
		endpoint.RawQuery = query.Encode()
		body, response, failure, err = request(ctx, endpoint.String(), defaultBodyLimit, value.TimeoutMS, httpClient)
	default:
		endpoint, parseErr := url.Parse(backend.endpoint)
		if parseErr != nil {
			return nil, 0, nil, "", fmt.Errorf("invalid %s search endpoint: %w", backend.name, parseErr)
		}
		query := endpoint.Query()
		var searchQuery strings.Builder
		searchQuery.WriteString(value.Query)
		for _, domain := range value.Domains {
			searchQuery.WriteString(" site:")
			searchQuery.WriteString(domain)
		}
		query.Set("q", searchQuery.String())
		query.Set("limit", strconv.Itoa(limit))
		if value.Recency != "" {
			switch backend.format {
			case "json":
				query.Set("recency", value.Recency)
			case "duckduckgo":
				query.Set("df", map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}[value.Recency])
			}
		}
		if backend.format == "json" && len(value.Domains) > 0 {
			query.Set("domains", strings.Join(value.Domains, ","))
		}
		endpoint.RawQuery = query.Encode()
		body, response, failure, err = request(ctx, endpoint.String(), defaultBodyLimit, value.TimeoutMS, httpClient)
	}
	if response != nil {
		statusCode = response.StatusCode
	}
	if err != nil || failure != nil {
		return nil, statusCode, failure, "", err
	}
	if isSearchChallenge(body) {
		return nil, statusCode, nil, "challenge", nil
	}
	results, reason, parseErr := parseSearchBody(body, backend.format)
	if parseErr != nil {
		return nil, statusCode, nil, "invalid_response", nil
	}
	if reason != "" {
		return nil, statusCode, nil, reason, nil
	}
	filtered := results[:0]
	for _, item := range results {
		if !validHTTPURL(item.URL) {
			continue
		}
		filtered = append(filtered, item)
	}
	if len(filtered) == 0 {
		return nil, statusCode, nil, "empty_results", nil
	}
	return filtered, statusCode, nil, "", nil
}

func parseSearchBody(body []byte, format string) ([]searchResult, string, error) {
	switch format {
	case "json":
		var envelope struct {
			Results []searchResult `json:"results"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, "invalid_response", nil
		}
		return envelope.Results, "", nil
	case "tavily":
		var envelope struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, "invalid_response", nil
		}
		out := make([]searchResult, 0, len(envelope.Results))
		for _, item := range envelope.Results {
			out = append(out, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Content})
		}
		return out, "", nil
	case "searxng":
		var envelope struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, "invalid_response", nil
		}
		out := make([]searchResult, 0, len(envelope.Results))
		for _, item := range envelope.Results {
			out = append(out, searchResult{Title: item.Title, URL: item.URL, Snippet: item.Content})
		}
		return out, "", nil
	case "bocha":
		var envelope struct {
			Data struct {
				WebPages struct {
					Value []struct {
						Name    string `json:"name"`
						URL     string `json:"url"`
						Snippet string `json:"snippet"`
					} `json:"value"`
				} `json:"webPages"`
			} `json:"data"`
			// Some Bocha responses flatten results.
			Results []searchResult `json:"results"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, "invalid_response", nil
		}
		if len(envelope.Results) > 0 {
			return envelope.Results, "", nil
		}
		out := make([]searchResult, 0, len(envelope.Data.WebPages.Value))
		for _, item := range envelope.Data.WebPages.Value {
			out = append(out, searchResult{Title: item.Name, URL: item.URL, Snippet: item.Snippet})
		}
		return out, "", nil
	default:
		results, err := parseSearchHTML(body, format)
		if err != nil {
			return nil, "invalid_response", nil
		}
		return results, "", nil
	}
}

func requestJSON(
	ctx context.Context,
	method, endpoint string,
	payload map[string]any,
	headers map[string]string,
	timeoutMS int,
	httpClient *http.Client,
) ([]byte, *http.Response, *tool.Result, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	timeout := 10 * time.Second
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else if client.Timeout == 0 {
		clone := *client
		clone.Timeout = timeout
		client = &clone
	}
	response, err := client.Do(req)
	if err != nil {
		failure := httpTransportFailure(err)
		return nil, nil, &failure, nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, defaultBodyLimit+1))
	if err != nil {
		return nil, nil, nil, err
	}
	if int64(len(body)) > defaultBodyLimit {
		body = body[:defaultBodyLimit]
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := webFailure("http_error", response.StatusCode, fmt.Sprintf("search backend returned HTTP %d", response.StatusCode))
		return nil, response, &failure, nil
	}
	return body, response, nil, nil
}
