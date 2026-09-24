package engine

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
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerrouter "github.com/fwtllh-png/QCode/internal/adapter/provider/router"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

const deepSeekEngineLiveControlEnv = "QCODE_DEEPSEEK_LIVE_CONTROL"

func TestDeepSeekEngineCrossTurnAppendOnlyCache(t *testing.T) {
	if os.Getenv(deepSeekEngineLiveControlEnv) != "1" {
		t.Skipf(
			"DeepSeek Engine live control disabled; set %s=1",
			deepSeekEngineLiveControlEnv,
		)
	}
	runtime, route := deepSeekEngineLiveRuntime(t)
	stable := fmt.Sprintf(
		"qcode-engine-append-only-%d %s",
		time.Now().UnixNano(),
		strings.Repeat("Stable repository context preserved across turns. ", 12_000),
	)
	engine, err := newTestEngine(Options{
		ProviderConfig: ProviderConfig{
			Provider: runtime, Route: route, MaxOutputTokens: 128,
			ReasoningEffort: "off",
		},
		ContextConfig: ContextConfig{StaticContext: []provider.Message{
			provider.TextMessage(provider.RoleSystem, stable),
		}},
		ToolConfig: ToolConfig{Tools: tool.NewRegistry(nil, nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var firstSamples []protocol.SampleContextData
	firstEmit := func(event Event) error {
		if event.SampleContext != nil {
			firstSamples = append(firstSamples, *event.SampleContext)
		}
		return nil
	}
	first, err := engine.Run(
		t.Context(),
		"Turn one. Reply with exactly alpha and no other text.",
		firstEmit,
	)
	if err != nil {
		t.Fatal(err)
	}
	var secondSamples []protocol.SampleContextData
	secondEmit := func(event Event) error {
		if event.SampleContext != nil {
			secondSamples = append(secondSamples, *event.SampleContext)
		}
		return nil
	}
	second, err := engine.Run(
		t.Context(),
		"Turn two. Reply with exactly beta and no other text.",
		secondEmit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Usage.InputTokens == 0 || second.Usage.InputTokens == 0 {
		t.Fatalf("missing Engine usage: first=%+v second=%+v", first.Usage, second.Usage)
	}
	if second.Usage.CachedTokens <= first.Usage.CachedTokens {
		t.Fatalf(
			"cross-Turn cache did not improve: first=%+v second=%+v",
			first.Usage,
			second.Usage,
		)
	}
	if len(firstSamples) == 0 || len(secondSamples) == 0 {
		t.Fatalf(
			"missing Engine samples: first=%d second=%d",
			len(firstSamples),
			len(secondSamples),
		)
	}
	firstTail := firstSamples[len(firstSamples)-1]
	secondHead := secondSamples[0]
	if !secondHead.PrefixCompared ||
		!secondHead.PrefixMonotonic ||
		secondHead.PreviousContextDigest != firstTail.ContextDigest {
		t.Fatalf(
			"Engine cross-Turn prefix continuity: first=%+v second=%+v",
			firstTail,
			secondHead,
		)
	}
	t.Logf(
		"Engine append-only input/cached: first=%d/%d second=%d/%d (%.2f%%)",
		first.Usage.InputTokens,
		first.Usage.CachedTokens,
		second.Usage.InputTokens,
		second.Usage.CachedTokens,
		float64(second.Usage.CachedTokens)*100/float64(second.Usage.InputTokens),
	)
}

func deepSeekEngineLiveRuntime(
	t *testing.T,
) (provider.Provider, model.ReadyRoute) {
	t.Helper()
	resolver, err := model.NewResolver(testCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "deepseek-v4-flash",
		ModelID:    "deepseek-v4-flash",
		Provenance: model.ProvenanceBundled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.Protocol() != model.ProtocolOpenAIChat ||
		route.Adapter() != model.AdapterOpenAICompatible {
		t.Fatalf(
			"DeepSeek route = adapter %q protocol %q",
			route.Adapter(),
			route.Protocol(),
		)
	}
	credential, err := httpclient.DefaultCredentials().Resolve(
		t.Context(),
		route.Credential(),
	)
	if err != nil {
		t.Skipf("DeepSeek credential is unavailable: %v", err)
	}
	gate := &egress.Gate{Enforce: true}
	if !gate.AllowURL(route.Endpoint()) {
		t.Fatalf("cannot grant DeepSeek endpoint %q", route.Endpoint())
	}
	client := httpclient.New()
	client.HTTP = &http.Client{Timeout: 3 * time.Minute}
	client.Credentials = deepSeekEngineCredential(credential)
	client.Egress = gate
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
	return runtime, route
}

type deepSeekEngineCredential string

func (c deepSeekEngineCredential) Resolve(
	context.Context,
	model.CredentialRef,
) (string, error) {
	return string(c), nil
}
