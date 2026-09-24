package interact_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
)

// recordingProvider stands in for the shared client so the test can read the
// request the vision tool actually built.
type recordingProvider struct {
	request provider.ModelRequest
	text    string
	err     error
}

func (p *recordingProvider) Stream(
	_ context.Context, request provider.ModelRequest,
) (provider.Stream, error) {
	p.request = request
	if p.err != nil {
		return nil, p.err
	}
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: provider.EventMessageStart},
		{Type: provider.EventTextDelta, Text: p.text},
		{Type: provider.EventUsage, Usage: &provider.Usage{InputTokens: 900, OutputTokens: 12}},
		{Type: provider.EventMessageStop},
	}}, nil
}

func visionRoute(t *testing.T) model.ReadyRoute {
	t.Helper()
	catalog, err := model.NewCatalog(model.Provider{
		ID: "openai", Adapter: model.AdapterOpenAI,
		Endpoint: "https://api.openai.com/v1", Protocol: model.ProtocolOpenAIChat,
		Models: map[string]model.Model{
			"gpt-4.1": {
				ID: "gpt-4.1", CanonicalID: "gpt-4.1", WireID: "gpt-4.1",
				Limits: model.Limits{ContextTokens: 1_047_576, MaxOutputTokens: 32_768},
				Capabilities: model.Capabilities{
					Streaming: true, ToolCalls: true, Vision: true, ImageInput: true,
				},
				Provenance: model.ProvenanceOperatorConfig,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := model.NewResolver(catalog)
	if err != nil {
		t.Fatal(err)
	}
	route, err := resolver.Resolve(model.RouteRequest{
		ProviderID: "openai", ModelID: "gpt-4.1", Provenance: model.ProvenanceConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return route
}

// TestVisionSamplesThroughTheProviderAbstraction is the point of converging
// vision: the call is an ordinary model request with an image block, so
// everything that watches model requests — retry, idempotency, usage, cost,
// trace — sees it too.
func TestVisionSamplesThroughTheProviderAbstraction(t *testing.T) {
	workspace := t.TempDir()
	imagePath := filepath.Join(workspace, "shot.jpeg")
	if err := os.WriteFile(imagePath, []byte("JPEGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := &recordingProvider{text: "a login screen"}
	client := interact.RouteVision{Provider: target, Route: visionRoute(t)}

	answer, err := client.Analyze(t.Context(), imagePath, "what is on screen")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "a login screen" {
		t.Fatalf("Analyze() = %q", answer)
	}

	request := target.request
	if request.Purpose != model.PurposeVision {
		t.Fatalf("purpose = %q, want vision", request.Purpose)
	}
	if request.Route.Model().ID != "gpt-4.1" {
		t.Fatalf("route model = %q, want the vision slot's model", request.Route.Model().ID)
	}
	if !request.Idempotent {
		t.Fatal("describing an image is idempotent and should be retryable")
	}
	if request.MaxOutputTokens != request.Route.Model().Limits.MaxOutputTokens {
		t.Fatalf(
			"max output tokens = %d, want model capability %d",
			request.MaxOutputTokens,
			request.Route.Model().Limits.MaxOutputTokens,
		)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("the vision request must be a valid model request: %v", err)
	}
	if len(request.Messages) != 1 || len(request.Messages[0].Blocks) != 2 {
		t.Fatalf("messages = %+v", request.Messages)
	}
	question := request.Messages[0].Blocks[0]
	image := request.Messages[0].Blocks[1]
	if question.Type != provider.ContentText || question.Text != "what is on screen" {
		t.Fatalf("question block = %+v", question)
	}
	if image.Type != provider.ContentImage ||
		image.Attachment == nil ||
		image.Attachment.MediaType != "image/jpeg" ||
		string(image.Attachment.Data) != "JPEGBYTES" ||
		image.Attachment.Name != "shot.jpeg" {
		t.Fatalf("image block = %+v", image)
	}
}

func TestVisionWithoutAPromptStillAsksSomething(t *testing.T) {
	workspace := t.TempDir()
	imagePath := filepath.Join(workspace, "shot.png")
	if err := os.WriteFile(imagePath, []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := &recordingProvider{text: "a chart"}
	client := interact.RouteVision{Provider: target, Route: visionRoute(t)}

	if _, err := client.Analyze(t.Context(), imagePath, "   "); err != nil {
		t.Fatal(err)
	}
	if text := target.request.Messages[0].Blocks[0].Text; text == "" {
		t.Fatal("an image with no question would ask the model nothing")
	}
}

func TestVisionRefusesWithoutARouteOrAProvider(t *testing.T) {
	workspace := t.TempDir()
	imagePath := filepath.Join(workspace, "shot.png")
	if err := os.WriteFile(imagePath, []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}

	noProvider := interact.RouteVision{Route: visionRoute(t)}
	if _, err := noProvider.Analyze(t.Context(), imagePath, "what"); err == nil ||
		!strings.Contains(err.Error(), interact.VisionUnavailableReason) {
		t.Fatalf("Analyze() error = %v, want unavailable", err)
	}

	noRoute := interact.RouteVision{Provider: &recordingProvider{text: "x"}}
	if _, err := noRoute.Analyze(t.Context(), imagePath, "what"); err == nil ||
		!strings.Contains(err.Error(), interact.VisionUnavailableReason) {
		t.Fatalf("Analyze() error = %v, want unavailable", err)
	}
}

func TestVisionReportsAnEmptyImageRatherThanSendingIt(t *testing.T) {
	workspace := t.TempDir()
	imagePath := filepath.Join(workspace, "empty.png")
	if err := os.WriteFile(imagePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := &recordingProvider{text: "x"}
	client := interact.RouteVision{Provider: target, Route: visionRoute(t)}

	if _, err := client.Analyze(t.Context(), imagePath, "what"); err == nil {
		t.Fatal("an empty image would be a request the provider rejects")
	}
	if target.request.Route.Model().ID != "" {
		t.Fatal("the empty image reached the provider")
	}
}
