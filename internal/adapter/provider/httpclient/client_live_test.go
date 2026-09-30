package httpclient

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerrouter "github.com/fwtllh-png/QCode/internal/adapter/provider/router"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func TestDeepSeekCrossTurnCacheDiagnostic(t *testing.T) {
	if os.Getenv(deepSeekLiveControlEnv) != "1" {
		t.Skipf("DeepSeek live control disabled; set %s=1", deepSeekLiveControlEnv)
	}
	for _, protocol := range []model.WireProtocol{
		model.ProtocolOpenAIResponses,
		model.ProtocolOpenAIChat,
	} {
		t.Run(string(protocol), func(t *testing.T) {
			runtime, route, _ := deepSeekLiveRuntimeForProtocol(t, protocol)
			nonce := fmt.Sprintf(
				"qcode-cross-turn-%s-%d ",
				protocol,
				time.Now().UnixNano(),
			)
			common := nonce + strings.Repeat(
				"Stable QCode repository context preserved across user turns. ",
				23_000,
			)
			tails := []string{
				strings.Repeat("Turn one transient analysis evidence alpha. ", 5_500),
				strings.Repeat("Turn two transient implementation evidence beta. ", 5_500),
				strings.Repeat("Turn three transient verification evidence gamma. ", 5_500),
			}
			for index, tail := range tails {
				usage := deepSeekDiagnosticUsage(t, runtime, provider.ModelRequest{
					Route: route,
					Messages: []provider.Message{
						provider.TextMessage(provider.RoleSystem, common),
						provider.TextMessage(provider.RoleUser,
							tail+"\nReply with exactly ok."),
					},
					MaxOutputTokens: 32, ReasoningEffort: "low", Idempotent: true,
				})
				t.Logf("cross sample=%d input=%d cached=%d share=%.2f%%",
					index+1, usage.InputTokens, usage.CachedTokens,
					float64(usage.CachedTokens)*100/float64(usage.InputTokens))
			}
			commonUsage := deepSeekDiagnosticUsage(t, runtime, provider.ModelRequest{
				Route: route,
				Messages: []provider.Message{
					provider.TextMessage(provider.RoleSystem, common),
					provider.TextMessage(provider.RoleUser, "Reply with exactly ok."),
				},
				MaxOutputTokens: 32, ReasoningEffort: "low", Idempotent: true,
			})
			t.Logf("common-only input=%d cached=%d share=%.2f%%",
				commonUsage.InputTokens, commonUsage.CachedTokens,
				float64(commonUsage.CachedTokens)*100/float64(commonUsage.InputTokens))
		})
	}
}

func TestDeepSeekChatAppendOnlyCache(t *testing.T) {
	if os.Getenv(deepSeekLiveControlEnv) != "1" {
		t.Skipf(
			"DeepSeek live diagnostic disabled; set %s=1",
			deepSeekLiveControlEnv,
		)
	}
	runtime, route, _ := deepSeekLiveRuntime(t)
	if route.Protocol() != model.ProtocolOpenAIChat {
		t.Fatalf("default DeepSeek protocol = %q", route.Protocol())
	}
	nonce := fmt.Sprintf("qcode-append-only-%d ", time.Now().UnixNano())
	messages := []provider.Message{
		provider.TextMessage(
			provider.RoleSystem,
			nonce+strings.Repeat(
				"Stable append-only repository context. ",
				12_000,
			),
		),
		provider.TextMessage(
			provider.RoleUser,
			"Turn one. Reply with exactly alpha.",
		),
	}
	first := deepSeekDiagnosticUsage(t, runtime, provider.ModelRequest{
		Route: route, Messages: append([]provider.Message(nil), messages...),
		MaxOutputTokens: 32, ReasoningEffort: "low", Idempotent: true,
	})
	messages = append(
		messages,
		provider.Message{
			Role: provider.RoleAssistant,
			Blocks: []provider.ContentBlock{
				{Type: provider.ContentReasoning, Text: "Return the requested token."},
				{Type: provider.ContentText, Text: "alpha"},
			},
		},
		provider.TextMessage(
			provider.RoleUser,
			"Turn two. Reply with exactly beta.",
		),
	)
	second := deepSeekDiagnosticUsage(t, runtime, provider.ModelRequest{
		Route: route, Messages: append([]provider.Message(nil), messages...),
		MaxOutputTokens: 32, ReasoningEffort: "low", Idempotent: true,
	})
	messages = append(
		messages,
		provider.TextMessage(provider.RoleAssistant, "beta"),
		provider.TextMessage(
			provider.RoleUser,
			"Turn three. Reply with exactly gamma.",
		),
	)
	third := deepSeekDiagnosticUsage(t, runtime, provider.ModelRequest{
		Route: route, Messages: append([]provider.Message(nil), messages...),
		MaxOutputTokens: 32, ReasoningEffort: "low", Idempotent: true,
	})
	t.Logf(
		"append-only input/cached: first=%d/%d second=%d/%d third=%d/%d",
		first.InputTokens,
		first.CachedTokens,
		second.InputTokens,
		second.CachedTokens,
		third.InputTokens,
		third.CachedTokens,
	)
	if second.CachedTokens <= first.CachedTokens {
		t.Fatalf(
			"second Turn cache did not improve: first=%+v second=%+v",
			first,
			second,
		)
	}
	if third.CachedTokens < second.CachedTokens {
		t.Fatalf(
			"third Turn cache regressed: second=%+v third=%+v",
			second,
			third,
		)
	}
}

func TestDeepSeekP0LiveControl(t *testing.T) {
	if os.Getenv(deepSeekLiveControlEnv) != "1" {
		t.Skipf("DeepSeek live control disabled; set %s=1", deepSeekLiveControlEnv)
	}
	runtime, route, metrics := deepSeekLiveRuntime(t)
	stream, err := runtime.Stream(t.Context(), provider.ModelRequest{
		Route: route,
		Messages: []provider.Message{
			provider.TextMessage(
				provider.RoleUser,
				"Reply with exactly qcode-provider-p0-live-ok.",
			),
		},
		MaxOutputTokens: 4096,
		ReasoningEffort: "max",
		Idempotent:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	var meaningful, usage, stopped bool
	for _, event := range events {
		switch event.Type {
		case provider.EventTextDelta, provider.EventReasoningDelta,
			provider.EventToolCallDelta, provider.EventSearchResult,
			provider.EventCitation:
			meaningful = true
		case provider.EventUsage:
			usage = true
		case provider.EventMessageStop:
			stopped = true
		}
	}
	if !meaningful || !usage || !stopped {
		t.Fatalf(
			"DeepSeek live control events: meaningful=%t usage=%t stopped=%t",
			meaningful, usage, stopped,
		)
	}
	if requests := metrics.Snapshot().ProviderRequests; requests != 1 {
		t.Fatalf("DeepSeek live control provider requests = %d, want 1", requests)
	}
}

func TestDeepSeekCE7LiveCacheShare(t *testing.T) {
	if os.Getenv(deepSeekLiveControlEnv) != "1" {
		t.Skipf("DeepSeek live control disabled; set %s=1", deepSeekLiveControlEnv)
	}
	runtime, route, _ := deepSeekLiveRuntime(t)
	prefix := strings.Repeat(
		"QCode cache continuity fixture with stable deterministic text. ",
		800,
	)
	var last provider.Usage
	for sample := 1; sample <= 3; sample++ {
		stream, err := runtime.Stream(t.Context(), provider.ModelRequest{
			Route: route,
			Messages: []provider.Message{
				provider.TextMessage(provider.RoleSystem, prefix),
				provider.TextMessage(
					provider.RoleUser,
					"Reply with exactly ok. sample="+string(rune('0'+sample)),
				),
			},
			MaxOutputTokens: 32,
			ReasoningEffort: "low",
			Idempotent:      true,
		})
		if err != nil {
			t.Fatal(err)
		}
		events, err := provider.Drain(stream)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == provider.EventUsage && event.Usage != nil {
				last = *event.Usage
			}
		}
	}
	if last.InputTokens == 0 {
		t.Fatal("DeepSeek cache probe returned no input usage")
	}
	shareBasisPoints := last.CachedTokens * 10_000 / last.InputTokens
	t.Logf(
		"DeepSeek sample-3 cache share: cached=%d input=%d share_bps=%d",
		last.CachedTokens,
		last.InputTokens,
		shareBasisPoints,
	)
	if shareBasisPoints < 9_500 {
		t.Fatalf(
			"DeepSeek sample-3 cache share = %d basis points, want at least 9500",
			shareBasisPoints,
		)
	}
}

func deepSeekDiagnosticUsage(
	t *testing.T,
	runtime provider.Provider,
	request provider.ModelRequest,
) provider.Usage {
	t.Helper()
	stream, err := runtime.Stream(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	events, err := provider.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	var usage provider.Usage
	for _, event := range events {
		if event.Type == provider.EventUsage && event.Usage != nil {
			usage = *event.Usage
		}
	}
	if usage.InputTokens == 0 {
		t.Fatal("DeepSeek diagnostic returned no input usage")
	}
	return usage
}

const deepSeekLiveControlEnv = "QCODE_DEEPSEEK_LIVE_CONTROL"

func deepSeekLiveRuntime(
	t *testing.T,
) (provider.Provider, model.ReadyRoute, *telemetry.Metrics) {
	t.Helper()
	route := bundledRoute(t, "deepseek-v4-flash", "deepseek-v4-flash")
	return deepSeekLiveRuntimeWithRoute(t, route)
}

func deepSeekLiveRuntimeForProtocol(
	t *testing.T,
	protocol model.WireProtocol,
) (provider.Provider, model.ReadyRoute, *telemetry.Metrics) {
	t.Helper()
	descriptor := deepSeekLiveProvider()
	descriptor.ID += "-" + string(protocol)
	descriptor.Protocol = protocol
	catalog, err := model.NewCatalog(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: descriptor.ID,
		ModelID:    "deepseek-v4-flash",
		Provenance: model.ProvenanceFixture,
	})
	if err != nil {
		t.Fatal(err)
	}
	return deepSeekLiveRuntimeWithRoute(t, route)
}

func deepSeekLiveRuntimeWithRoute(
	t *testing.T,
	route model.ReadyRoute,
) (provider.Provider, model.ReadyRoute, *telemetry.Metrics) {
	t.Helper()
	credential, err := DefaultCredentials().Resolve(t.Context(), route.Credential())
	if err != nil {
		t.Skipf("DeepSeek live control skipped: configured credential is unavailable: %v", err)
	}

	gate := egress.NewStaticGate()
	if !gate.AllowURL(route.Endpoint()) {
		t.Fatalf("cannot grant DeepSeek endpoint %q", route.Endpoint())
	}
	metrics := telemetry.NewMetrics()
	client := New()
	client.HTTP = &http.Client{Timeout: 3 * time.Minute}
	client.Credentials = p0LiveCredential(credential)
	client.Egress = gate
	client.Metrics = metrics
	client.IdleTimeout = 2 * time.Minute
	adapter, err := openai.NewAdapter(route.Adapter())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := providerrouter.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := model.NewRouteSet(route, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := providerrouter.New(registry, routes, client)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, route, metrics
}

// deepSeekLiveProvider 内联 DeepSeek V4 Flash 连接声明（端点、凭证引用、
// 模型元数据），替代已删除的内置目录条目；live 运行手册见
// docs/DEEPSEEK-LIVE.zh-CN.md。
func deepSeekLiveProvider() model.Provider {
	return model.Provider{
		ID: "deepseek-v4-flash", Adapter: model.AdapterOpenAICompatible,
		Endpoint: "https://api.deepseek.com", Protocol: model.ProtocolOpenAIChat,
		Credential: model.CredentialRef{Kind: "keyring", Name: "deepseek/default"},
		Provenance: model.ProvenanceBundled,
		Models: map[string]model.Model{
			"deepseek-v4-flash": {
				ID: "deepseek-v4-flash", CanonicalID: "deepseek-v4-flash",
				WireID: "deepseek-v4-flash",
				Limits: model.Limits{
					ContextTokens: 1_048_576, MaxOutputTokens: 384_000,
				},
				Capabilities: model.Capabilities{
					Streaming: true, Reasoning: true, ToolCalls: true,
					PromptCache: true, AutomaticPromptCache: true, ThinkingToggle: true,
					ReasoningEfforts:       []string{"off", "low", "high", "max"},
					DefaultReasoningEffort: "high",
				},
				Pricing:    model.Pricing{Provenance: model.ProvenanceBundled},
				Provenance: model.ProvenanceBundled,
			},
		},
	}
}

func bundledRoute(t *testing.T, providerID, modelID string) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(deepSeekLiveProvider())
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: providerID,
		ModelID:    modelID,
		Provenance: model.ProvenanceBundled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

type p0LiveCredential string

func (c p0LiveCredential) Resolve(
	context.Context,
	model.CredentialRef,
) (string, error) {
	return string(c), nil
}
