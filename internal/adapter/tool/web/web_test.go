package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func boundTool(t testing.TB, executor *Tool) *Tool {
	t.Helper()
	if err := executor.bindContract(); err != nil {
		t.Fatal(err)
	}
	return executor
}

func TestWebSearchFetchScrapeAndLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/search":
			if request.URL.Query().Get("q") != "fixture query" || request.URL.Query().Get("limit") != "2" {
				http.Error(writer, "bad query", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"results":[{"title":"Fixture","url":"http://%s/page","snippet":"answer"}]}`, request.Host)
		case request.URL.Path == "/page":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(`<html><head><title>Fixture Page</title><style>hidden</style></head><body><main>Hello <b>Web</b></main><script>ignored()</script></body></html>`))
		case strings.HasPrefix(request.URL.Path, "/redirect/"):
			index, _ := strconv.Atoi(strings.TrimPrefix(request.URL.Path, "/redirect/"))
			http.Redirect(writer, request, "/redirect/"+strconv.Itoa(index+1), http.StatusFound)
		case request.URL.Path == "/slow":
			time.Sleep(100 * time.Millisecond)
			_, _ = writer.Write([]byte("late"))
		case request.URL.Path == "/large":
			_, _ = writer.Write([]byte(strings.Repeat("x", 128)))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	search, err := boundTool(t, &Tool{
		kind: "web_search", searchURL: server.URL + "/search",
	}).Execute(
		t.Context(), json.RawMessage(`{"query":"fixture query","limit":2}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if search.IsError || !strings.Contains(search.Content, `"citations":[{"id":"web-1"`) {
		t.Fatalf("search = %+v", search)
	}

	scrape, err := boundTool(t, &Tool{kind: "web_scrape"}).Execute(
		t.Context(), json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL+"/page")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if scrape.IsError || !strings.Contains(scrape.Content, `"title":"Fixture Page"`) ||
		!strings.Contains(scrape.Content, `"text":"Hello Web"`) ||
		strings.Contains(scrape.Content, "ignored") {
		t.Fatalf("scrape = %+v", scrape)
	}

	cases := []struct {
		name      string
		arguments string
		category  string
	}{
		{"redirect", fmt.Sprintf(`{"url":%q}`, server.URL+"/redirect/0"), "redirect_limit"},
		{"timeout", fmt.Sprintf(`{"url":%q,"timeout_ms":10}`, server.URL+"/slow"), "timeout"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := boundTool(t, &Tool{kind: "web_fetch"}).
				Execute(t.Context(), json.RawMessage(test.arguments))
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || result.Metadata["error_category"] != test.category {
				t.Fatalf("result = %+v", result)
			}
		})
	}

	// Tiny max_bytes used to hard-fail every real page; floor to the default instead.
	tiny, err := boundTool(t, &Tool{kind: "web_fetch"}).Execute(
		t.Context(), json.RawMessage(fmt.Sprintf(`{"url":%q,"max_bytes":32}`, server.URL+"/large")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if tiny.IsError || tiny.Content != strings.Repeat("x", 128) {
		t.Fatalf("tiny max_bytes should succeed after floor, got %+v", tiny)
	}
}

func TestResolveBodyLimit(t *testing.T) {
	if got := resolveBodyLimit(0); got != defaultBodyLimit {
		t.Fatalf("default: got %d", got)
	}
	if got := resolveBodyLimit(32); got != defaultBodyLimit {
		t.Fatalf("tiny floor: got %d", got)
	}
	if got := resolveBodyLimit(minBodyLimit); got != minBodyLimit {
		t.Fatalf("min accepted: got %d", got)
	}
	if got := resolveBodyLimit(maxBodyLimit + 1); got != maxBodyLimit {
		t.Fatalf("hard cap: got %d", got)
	}
}

func TestWebFetchTruncatesOverLimit(t *testing.T) {
	payload := strings.Repeat("y", int(minBodyLimit)+64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(payload))
	}))
	defer server.Close()

	result, err := boundTool(t, &Tool{kind: "web_fetch"}).Execute(
		t.Context(),
		json.RawMessage(fmt.Sprintf(`{"url":%q,"max_bytes":%d}`, server.URL, minBodyLimit)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !result.Truncated || len(result.Content) != int(minBodyLimit) {
		t.Fatalf("expected truncated success, got %+v (len=%d)", result, len(result.Content))
	}
}

func TestWebFetchRateLimitStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"message":"API rate limit exceeded for 1.2.3.4"}`))
	}))
	defer server.Close()

	result, err := boundTool(t, &Tool{kind: "web_fetch"}).Execute(
		t.Context(), json.RawMessage(fmt.Sprintf(`{"url":%q}`, server.URL)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Metadata["error_category"] != "rate_limited" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "do not immediately re-fetch") {
		t.Fatalf("content = %q", result.Content)
	}
	if result.Metadata["url"] == nil || result.Metadata["final_url"] == nil {
		t.Fatalf("missing url metadata: %+v", result.Metadata)
	}
	if !strings.Contains(result.Content, server.URL) {
		t.Fatalf("content missing url: %q", result.Content)
	}
}

func TestWebSearchDefaultBackendFallbackAndReceipts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/duckduckgo":
			if request.URL.Query().Get("q") != "fixture site:example.com" ||
				request.URL.Query().Get("df") != "w" {
				http.Error(writer, "filters not applied", http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`<html><body>Verify you are human with this captcha</body></html>`))
		case "/bing":
			if request.URL.Query().Get("q") != "fixture site:example.com" {
				http.Error(writer, "domain not applied", http.StatusBadRequest)
				return
			}
			_, _ = writer.Write([]byte(`<html><body><ol>
				<li class="b_algo"><h2><a href="https://example.com/result">Fallback Result</a></h2>
				<div><p>Fallback snippet</p></div></li>
			</ol></body></html>`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	result, err := boundTool(t, &Tool{
		kind: "web_search", primaryURL: server.URL + "/duckduckgo",
		fallbackURL: server.URL + "/bing",
	}).Execute(t.Context(), json.RawMessage(
		`{"query":"fixture","domains":["example.com"],"recency":"week"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Metadata["backend"] != "bing" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, `"url":"https://example.com/result"`) ||
		!strings.Contains(result.Content, `"outcome":"challenge"`) ||
		!strings.Contains(result.Content, `"recency_unsupported":true`) {
		t.Fatalf("content = %s", result.Content)
	}
	receipts, ok := result.Metadata["receipts"].([]map[string]any)
	if !ok || len(receipts) != 2 {
		t.Fatalf("receipts = %#v", result.Metadata["receipts"])
	}
	if receipts[0]["recency_applied"] != true || receipts[1]["recency_applied"] != false {
		t.Fatalf("receipts = %#v", receipts)
	}
}

func TestParseDuckDuckGoSearchHTML(t *testing.T) {
	results, err := parseSearchHTML([]byte(`<div class="result">
		<a class="result__a" href="/l/?uddg=https%3A%2F%2Fexample.com%2Fanswer">Primary Result</a>
		<a class="result__snippet">Primary snippet</a>
	</div>`), "duckduckgo")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].URL != "https://example.com/answer" ||
		results[0].Title != "Primary Result" || results[0].Snippet != "Primary snippet" {
		t.Fatalf("results = %#v", results)
	}
}

func TestEgressDeniedClassified(t *testing.T) {
	gate := egress.NewStaticGate()
	client := egress.WrapClient(&http.Client{}, gate)
	result, err := boundTool(t, &Tool{kind: "web_fetch", httpClient: client}).Execute(
		t.Context(), json.RawMessage(`{"url":"https://example.com/page"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Metadata["error_category"] != "egress_denied" {
		t.Fatalf("result = %+v", result)
	}
	if result.Metadata["host"] != "example.com" {
		t.Fatalf("host = %#v", result.Metadata["host"])
	}
	if result.Outcome == nil || result.Outcome.Security == nil ||
		result.Outcome.Security.EgressDenied == nil ||
		result.Outcome.Security.EgressDenied.Host != "example.com" {
		t.Fatalf("typed egress outcome = %+v", result.Outcome)
	}
	if !strings.Contains(result.Content, "egress denied · host=example.com") {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestWebSearchResourcesUseBackendHosts(t *testing.T) {
	search := &Tool{
		kind:        "web_search",
		primaryURL:  "https://html.duckduckgo.com/html/",
		fallbackURL: "https://www.bing.com/search",
	}
	descriptor := search.Descriptor()
	templates := descriptor.ResourceResolver.Templates
	if len(templates) != 3 {
		t.Fatalf("templates = %#v", templates)
	}
	ids := map[string]struct{}{}
	for _, template := range templates {
		if template.Field != "" {
			t.Fatalf("web_search must not resolve the query field: %#v", template)
		}
		if template.Kind != "url" || template.ID == "" {
			t.Fatalf("template = %#v", template)
		}
		ids[template.ID] = struct{}{}
	}
	if _, ok := ids["https://html.duckduckgo.com/html/"]; !ok {
		t.Fatalf("missing primary backend: %#v", ids)
	}
	if _, ok := ids["https://www.bing.com/search"]; !ok {
		t.Fatalf("missing fallback backend: %#v", ids)
	}
	if _, ok := ids["https://cn.bing.com/search"]; !ok {
		t.Fatalf("missing cn.bing redirect host: %#v", ids)
	}
}
