package web

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

func TestWebSetupRequiresFourExplicitFields(t *testing.T) {
	valid := webhost.SetupRequest{
		Model: "vendor/model-v1", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
		ModelMetadata: testSetupMetadata("vendor/model-v1"),
	}
	selection, reference, err := resolveWebSetup(valid)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Provider != connectionID("https://models.example.com/v1") ||
		selection.ID != selection.Provider ||
		selection.Model != "vendor/model-v1" ||
		selection.BaseURL != "https://models.example.com/v1" ||
		selection.Protocol != "openai_chat" ||
		selection.MetadataProvenance != model.ProvenanceOperatorConfig {
		t.Fatalf("resolved selection = %+v", selection)
	}
	if reference.Kind != "" || reference.Name != "" {
		t.Fatalf("connection carries no credential reference: %+v", reference)
	}

	for name, mutate := range map[string]func(*webhost.SetupRequest){
		"missing base URL":   func(r *webhost.SetupRequest) { r.BaseURL = "" },
		"missing model":      func(r *webhost.SetupRequest) { r.Model = "" },
		"missing api key":    func(r *webhost.SetupRequest) { r.APIKey = "" },
		"missing metadata":   func(r *webhost.SetupRequest) { r.ModelMetadata = nil },
		"plain http base":    func(r *webhost.SetupRequest) { r.BaseURL = "http://example.com/v1" },
		"unknown protocol": func(r *webhost.SetupRequest) {
			r.Protocol = "anthropic"
		},
		"invalid model id": func(r *webhost.SetupRequest) {
			r.Model = "not a model id"
		},
	} {
		request := valid
		mutate(&request)
		if _, _, err := resolveWebSetup(request); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestLegacyPresetConnectionsMaterializeOnLoad(t *testing.T) {
	reference := &credential.Reference{
		Kind: "keyring",
		Name: "web/setup/00000000000000000000000000000000",
	}
	for _, test := range []struct {
		provider   string
		model      string
		endpoint   string
		context    uint64
		reasoning  bool
	}{
		{"openai", "gpt-4.1", "https://api.openai.com/v1", 1_047_576, false},
		{"deepseek", "deepseek-chat", "https://api.deepseek.com/v1", 131_072, false},
		{"glm", "glm-5.3", "https://open.bigmodel.cn/api/coding/paas/v4", 1_000_000, true},
		// 旧别名归属：deepseek 连接引用 deepseek-v4-flash provider 的模型。
		{"deepseek", "deepseek-v4-flash", "https://api.deepseek.com", 1_048_576, true},
	} {
		t.Run(test.provider+"/"+test.model, func(t *testing.T) {
			stored := webSetupConnection{
				ID: test.provider, Provider: test.provider, Model: test.model,
				MetadataProvenance: model.ProvenanceBundled,
				Credential:         reference,
			}
			materialized, ok := materializeLegacyConnection(stored)
			if !ok {
				t.Fatalf("legacy connection %s/%s did not materialize", test.provider, test.model)
			}
			if materialized.BaseURL != test.endpoint ||
				materialized.Protocol != string(model.ProtocolOpenAIChat) ||
				materialized.ID != test.provider ||
				materialized.Provider != test.provider ||
				materialized.Credential != reference {
				t.Fatalf("materialized = %+v", materialized)
			}
			descriptor := setupModelMetadata(materialized).Descriptor
			if descriptor == nil ||
				descriptor.Limits.ContextTokens != test.context ||
				descriptor.Capabilities.Reasoning != test.reasoning {
				t.Fatalf("materialized metadata = %+v", descriptor)
			}
			if !isMaterializedLegacyConnection(materialized) {
				t.Fatal("materialized connection is not recognized as canonical legacy form")
			}

			dataDir := t.TempDir()
			if err := saveWebSetupSelection(
				dataDir, "workspace", wrapConnection(stored),
			); err != nil {
				t.Fatal(err)
			}
			loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
			if err != nil || !found {
				t.Fatalf("loaded materialized selection: found=%v err=%v", found, err)
			}
			if !reflect.DeepEqual(loaded.Active(), &materialized) {
				t.Fatalf("loaded = %+v want %+v", loaded.Active(), materialized)
			}
			// 物化结果必须可以再次加载（canonical 稳定）。
			reloaded, found, err := loadWebSetupSelection(dataDir, "workspace")
			if err != nil || !found || !reflect.DeepEqual(reloaded, loaded) {
				t.Fatalf("reloaded = %+v err=%v", reloaded, err)
			}
		})
	}
}

func TestUnresolvableLegacyConnectionsAreDropped(t *testing.T) {
	selection := webSetupSelection{
		Version: webSetupVersion,
		Connections: []webSetupConnection{
			{
				ID: "openrouter", Provider: "openrouter", Model: "openrouter/auto",
				MetadataProvenance: model.ProvenanceBundled,
			},
		},
		DefaultConnection: "openrouter",
	}
	dataDir := t.TempDir()
	if err := saveWebSetupSelection(dataDir, "workspace", selection); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadWebSetupSelection(dataDir, "workspace"); err != nil || found {
		t.Fatalf("unresolvable legacy selection found=%v err=%v", found, err)
	}
}

func TestLegacyCustomProviderFormNormalizesToConnectionID(t *testing.T) {
	// 旧自定义连接：Provider 为 "openai-compatible"，ID 为端点摘要。
	connection := webSetupConnection{
		ID: connectionID("https://models.example.com/v1"),
		Provider: customProviderID, Model: "vendor/model-v1",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
		Metadata: testSetupMetadata("vendor/model-v1"),
		MetadataProvenance: model.ProvenanceOperatorConfig,
		Credential: &credential.Reference{
			Kind: "keyring",
			Name: "web/setup/00000000000000000000000000000000",
		},
	}
	dataDir := t.TempDir()
	if err := saveWebSetupSelection(dataDir, "workspace", wrapConnection(connection)); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
	if err != nil || !found {
		t.Fatalf("legacy custom selection found=%v err=%v", found, err)
	}
	active := loaded.Active()
	if active.Provider != connection.ID || active.ID != connection.ID {
		t.Fatalf("normalized connection = %+v", active)
	}
	// 规范化结果回写后再加载保持稳定。
	if _, _, err := loadWebSetupSelection(dataDir, "workspace"); err != nil {
		t.Fatal(err)
	}
}

func TestWebSetupPersistsOnlyNonSecretSelection(t *testing.T) {
	inputMetadata := testSetupMetadata("vendor/model-v1")
	inputMetadata.WireID = "wire-model-v1"
	selection, reference, err := resolveWebSetup(webhost.SetupRequest{
		Model: "vendor/model-v1",
		BaseURL:  "https://models.example.com/v1/",
		Protocol: "openai_responses", APIKey: "secret-value",
		ModelMetadata: inputMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reference.Kind != "" || reference.Name != "" ||
		selection.BaseURL != "https://models.example.com/v1" ||
		selection.MetadataProvenance != model.ProvenanceOperatorConfig {
		t.Fatalf("custom setup = %+v reference=%+v", selection, reference)
	}
	metadata := setupModelMetadata(selection).Descriptor
	if metadata == nil ||
		metadata.Limits.ContextTokens != 65_536 ||
		metadata.Limits.MaxOutputTokens != 8_192 ||
		metadata.WireID != "wire-model-v1" ||
		!metadata.Capabilities.Reasoning ||
		metadata.Capabilities.DefaultReasoningEffort != "high" ||
		metadata.MetadataProvenance.Limits != model.ProvenanceOperatorConfig {
		t.Fatalf("resolved custom metadata = %+v", metadata)
	}
	if wireID := setupWireModelID(selection, selection.Model); wireID != "wire-model-v1" {
		t.Fatalf("wire model id = %q", wireID)
	}
	t.Run("no registered models", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := saveWebSetupSelection(dataDir, "workspace", wrapConnection(selection)); err != nil {
			t.Fatal(err)
		}
		loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
		if err != nil || !found || !reflect.DeepEqual(loaded, wrapConnection(selection)) {
			t.Fatalf("loaded setup = %+v found=%v err=%v", loaded, found, err)
		}
	})
	selection.Credential = &credential.Reference{
		Kind: "keyring",
		Name: "web/setup/00000000000000000000000000000000",
	}
	selection.Models = []webSetupModel{{
		ID:       "vendor/model-v2",
		Metadata: *testSetupMetadata("vendor/model-v2"),
	}}
	dataDir := t.TempDir()
	if err := saveWebSetupSelection(dataDir, "workspace", wrapConnection(selection)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(setupSelectionPath(dataDir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("setup selection persisted the API key")
	}
	if !strings.Contains(string(data), `"context_tokens":65536`) ||
		!strings.Contains(string(data), `"reasoning_efforts":["off","high"]`) {
		t.Fatalf("setup selection did not persist model metadata: %s", data)
	}
	loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
	if err != nil || !found || !reflect.DeepEqual(loaded, wrapConnection(selection)) {
		t.Fatalf("loaded setup = %+v found=%v err=%v", loaded, found, err)
	}
	restored := setupModelMetadata(*loaded.Active()).Descriptor
	if restored == nil ||
		restored.Limits != metadata.Limits ||
		!reflect.DeepEqual(restored.Capabilities, metadata.Capabilities) ||
		restored.MetadataProvenance != metadata.MetadataProvenance {
		t.Fatalf("restored custom metadata = %+v", restored)
	}
	additional := setupModelMetadata(*loaded.Active()).AdditionalDescriptors
	if descriptor, ok := additional["vendor/model-v2"]; !ok ||
		descriptor.ID != "vendor/model-v2" {
		t.Fatalf("restored additional models = %+v", additional)
	}
}

func TestWebSetupRejectsMissingOrInvalidCustomMetadata(t *testing.T) {
	base := webhost.SetupRequest{
		Model: "custom-model", APIKey: "secret-value",
		BaseURL: "https://models.example.com/v1", Protocol: "openai_chat",
	}
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup without metadata was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.Vision = nil
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with an omitted capability was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.ContextTokens = 0
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with zero context was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.ToolCalls = boolPointer(false)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup without tool calls was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.MaxOutputTokens = base.ModelMetadata.ContextTokens + 1
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with output above context was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.DefaultReasoningEffort = "medium"
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with undeclared default effort was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.Reasoning = boolPointer(false)
	base.ModelMetadata.Capabilities.ReasoningEfforts = nil
	base.ModelMetadata.Capabilities.DefaultReasoningEffort = ""
	base.ModelMetadata.Capabilities.ThinkingToggle = boolPointer(true)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("custom setup with thinking toggle but no reasoning was accepted")
	}
	base.ModelMetadata = testSetupMetadata(base.Model)
	base.ModelMetadata.Capabilities.IncrementalResponses = boolPointer(true)
	if _, _, err := resolveWebSetup(base); err == nil {
		t.Fatal("chat setup with incremental responses was accepted")
	}
}

func TestLegacyCustomSelectionRequiresSetup(t *testing.T) {
	dataDir := t.TempDir()
	legacyJSON := `{"version":1,"provider":"openai-compatible","model":"legacy-model",` +
		`"base_url":"https://models.example.com/v1","protocol":"openai_chat"}` + "\n"
	if err := os.MkdirAll(filepath.Join(dataDir, "web-setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dataDir, "web-setup", "selection.json"),
		[]byte(legacyJSON), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadWebSetupSelection(dataDir, "workspace"); err != nil || found {
		t.Fatalf("legacy custom selection found=%t err=%v", found, err)
	}
}

func TestLegacyFlatPresetSelectionUpgrades(t *testing.T) {
	// v1 扁平格式（无连接集）的预设连接无损升级并物化。
	dataDir := t.TempDir()
	legacyJSON := `{"version":1,"provider":"deepseek","model":"deepseek-chat",` +
		`"metadata_provenance":"bundled"}` + "\n"
	if err := os.MkdirAll(filepath.Join(dataDir, "web-setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dataDir, "web-setup", "selection.json"),
		[]byte(legacyJSON), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := loadWebSetupSelection(dataDir, "workspace")
	if err != nil || !found {
		t.Fatalf("upgraded selection found=%v err=%v", found, err)
	}
	active := loaded.Active()
	if active.ID != "deepseek" || active.Provider != "deepseek" ||
		active.Model != "deepseek-chat" ||
		active.BaseURL != "https://api.deepseek.com/v1" ||
		active.Protocol != string(model.ProtocolOpenAIChat) ||
		active.Metadata == nil {
		t.Fatalf("upgraded selection = %+v", active)
	}
}

func wrapConnection(connection webSetupConnection) webSetupSelection {
	return webSetupSelection{
		Version:           webSetupVersion,
		Connections:       []webSetupConnection{cloneWebSetupConnection(connection)},
		DefaultConnection: connection.ID,
	}
}

func testSetupMetadata(modelID string) *webhost.SetupModelMetadata {
	return &webhost.SetupModelMetadata{
		CanonicalID:     modelID,
		WireID:          modelID,
		ContextTokens:   65_536,
		MaxOutputTokens: 8_192,
		Capabilities: webhost.SetupModelCapabilities{
			Streaming:              boolPointer(true),
			Reasoning:              boolPointer(true),
			ReasoningEfforts:       []string{"off", "high"},
			DefaultReasoningEffort: "high",
			ToolCalls:              boolPointer(true),
			NativeSearch:           boolPointer(false),
			IncrementalResponses:   boolPointer(false),
			Vision:                 boolPointer(false),
			ImageInput:             boolPointer(false),
			PromptCache:            boolPointer(true),
			AutomaticPromptCache:   boolPointer(false),
			ThinkingToggle:         boolPointer(false),
		},
	}
}

func boolPointer(value bool) *bool {
	return &value
}
