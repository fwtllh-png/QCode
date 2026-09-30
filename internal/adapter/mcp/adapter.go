package mcp

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

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
)

type executor struct {
	entry      CatalogEntry
	descriptor tool.Descriptor
}

type helperExecutor struct {
	kind       string
	pool       *Pool
	descriptor tool.Descriptor
}

type helperInput struct {
	Server    string            `json:"server,omitempty"`
	URI       string            `json:"uri,omitempty"`
	Name      string            `json:"name,omitempty"`
	Arguments map[string]string `json:"arguments,omitempty"`
}

type Adapter struct {
	mu       sync.Mutex
	registry *tool.Registry
	pool     *Pool
	sources  map[string]bool
}

type registrationIdentity struct {
	Key        string
	Connection *Connection
}

func NewAdapter(registry *tool.Registry, pool *Pool) (*Adapter, error) {
	if registry == nil {
		return nil, errors.New("MCP adapter registry is required")
	}
	if pool == nil {
		return nil, errors.New("MCP adapter pool is required")
	}
	adapter := &Adapter{
		registry: registry, pool: pool, sources: make(map[string]bool),
	}
	if err := adapter.Sync(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func (a *Adapter) Sync() error {
	if a == nil || a.registry == nil || a.pool == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.syncLocked(); err != nil {
		if quarantineErr := a.quarantineLocked(); quarantineErr != nil {
			return errors.Join(err, fmt.Errorf("quarantine MCP catalog: %w", quarantineErr))
		}
		return err
	}
	return nil
}

func (a *Adapter) quarantineLocked() error {
	sources := make(map[string]bool, len(a.sources)+1)
	for source := range a.sources {
		sources[source] = true
	}
	for _, server := range a.pool.ServerNames() {
		sources[sourceForServer(server)] = true
	}
	sources["mcp:helpers"] = true
	names := make([]string, 0, len(sources))
	for source := range sources {
		names = append(names, source)
	}
	sort.Strings(names)
	var quarantineErr error
	for _, source := range names {
		if err := reconcileSource(a.registry, source, nil); err != nil {
			quarantineErr = errors.Join(quarantineErr, err)
		}
	}
	return quarantineErr
}

func (a *Adapter) syncLocked() error {
	desired := make(map[string][]CatalogEntry)
	for _, entry := range a.pool.Catalog() {
		source := sourceForServer(entry.Server)
		desired[source] = append(desired[source], entry)
	}
	nextSources := make(map[string]bool)
	for _, server := range a.pool.ServerNames() {
		nextSources[sourceForServer(server)] = true
	}
	for source := range a.sources {
		nextSources[source] = true
	}
	sources := make([]string, 0, len(nextSources))
	for source := range nextSources {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		registrations, err := a.registrationsFor(source, desired[source])
		if err != nil {
			return err
		}
		if err := reconcileSource(a.registry, source, registrations); err != nil {
			return err
		}
	}
	var helpers []string
	if len(a.pool.ResourceCatalog()) != 0 || len(a.pool.ResourceTemplateCatalog()) != 0 {
		helpers = append(helpers, "list_mcp_resources", "read_mcp_resource")
	}
	if len(a.pool.PromptCatalog()) != 0 {
		helpers = append(helpers, "mcp_get_prompt")
	}
	existingHelpers := make(map[string]tool.Registration)
	for _, registration := range a.registry.SourceRegistrations("mcp:helpers") {
		if identity, ok := registration.Payload().(registrationIdentity); ok {
			existingHelpers[identity.Key] = registration
		}
	}
	helperRegistrations := make([]tool.Registration, 0, len(helpers))
	for _, name := range helpers {
		if existing, ok := existingHelpers[name]; ok {
			helperRegistrations = append(helperRegistrations, existing)
			continue
		}
		helper := newHelperExecutor(name, a.pool)
		descriptor := helper.Descriptor()
		helperRegistrations = append(
			helperRegistrations,
			tool.NewExternalRegistration(
				tool.ExternalFromDescriptor(descriptor),
				tool.TrustedBindingFromDescriptor(descriptor),
				helper,
			).WithPayload(registrationIdentity{Key: name}),
		)
	}
	if err := reconcileSource(a.registry, "mcp:helpers", helperRegistrations); err != nil {
		return err
	}
	a.sources = make(map[string]bool)
	for source, entries := range desired {
		if len(entries) != 0 {
			a.sources[source] = true
		}
	}
	return nil
}

func (a *Adapter) registrationsFor(
	source string,
	entries []CatalogEntry,
) ([]tool.Registration, error) {
	current := a.registry.SourceRegistrations(source)
	byKey := make(map[string]tool.Registration, len(current))
	for _, registration := range current {
		if identity, ok := registration.Payload().(registrationIdentity); ok {
			byKey[identity.Key] = registration
		}
	}
	registrations := make([]tool.Registration, 0, len(entries))
	for _, entry := range entries {
		descriptor, err := descriptorFor(entry)
		if err != nil {
			return nil, err
		}
		key := catalogEntryKey(entry)
		existing, ok := byKey[key]
		identity, _ := existing.Payload().(registrationIdentity)
		if ok && identity.Connection == entry.Connection {
			registrations = append(registrations, existing)
			continue
		}
		catalogEntry := entry
		frozenDescriptor := descriptor
		binding := trustedBindingFor(frozenDescriptor)
		registrations = append(registrations, tool.NewExternalDeferredRegistration(
			externalDescriptorFor(frozenDescriptor),
			binding,
			func() (tool.Executor, error) {
				return &executor{
					entry: catalogEntry, descriptor: frozenDescriptor,
				}, nil
			},
		).WithPayload(registrationIdentity{Key: key, Connection: entry.Connection}))
	}
	return registrations, nil
}

func externalDescriptorFor(
	descriptor tool.Descriptor,
) tool.ExternalDescriptor {
	external := tool.ExternalFromDescriptor(descriptor)
	return external
}

func trustedBindingFor(
	descriptor tool.Descriptor,
) tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(descriptor)
	binding.Effect = tool.EffectContract{
		Mode:                 tool.EffectDerived,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyDefault,
	}
	return binding
}

func sourceForServer(server string) string {
	return "mcp:" + server
}

func catalogEntryKey(entry CatalogEntry) string {
	data, _ := json.Marshal(struct {
		Server     string      `json:"server"`
		RemoteName string      `json:"remote_name"`
		ModelName  string      `json:"model_name"`
		Tool       Tool        `json:"tool"`
		Binding    ToolBinding `json:"binding"`
	}{
		Server: entry.Server, RemoteName: entry.RemoteName,
		ModelName: entry.ModelName, Tool: entry.Tool, Binding: entry.Binding,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func reconcileSource(
	registry *tool.Registry,
	source string,
	registrations []tool.Registration,
) error {
	for range 8 {
		generation := registry.Generation()
		if _, err := registry.Reconcile(source, generation, registrations); err != nil {
			if errors.Is(err, tool.ErrCatalogStale) {
				continue
			}
			return fmt.Errorf("reconcile MCP source %q: %w", source, err)
		}
		return nil
	}
	return fmt.Errorf("%w: MCP source %q remained contended", tool.ErrCatalogStale, source)
}

func descriptorFor(entry CatalogEntry) (tool.Descriptor, error) {
	if entry.Connection == nil {
		return tool.Descriptor{}, errors.New("MCP catalog entry connection is required")
	}
	description := strings.TrimSpace(entry.Tool.Description)
	if description == "" {
		description = "MCP tool " + entry.RemoteName + " from " + entry.Server
	}
	if entry.HostTrusted {
		description = "[Host-trusted MCP server process] " + description
	}
	var resources []tool.ResourceTemplate
	for _, resource := range entry.Binding.Resources {
		resources = append(resources, tool.ResourceTemplate{
			Kind:   resource.Kind,
			Field:  resource.Field,
			ID:     resource.ID,
			Access: tool.AccessMode(resource.Access),
			Tree:   resource.Tree,
			Glob:   resource.Glob,
		})
	}
	descriptor := tool.Descriptor{
		Name:               entry.ModelName,
		Description:        description,
		InputSchema:        entry.Tool.InputSchema,
		Visibility:         tool.VisibleModel,
		Capability:         tool.Capability(entry.Binding.Capability),
		ResourceResolver:   tool.ResourceResolver{Templates: resources},
		AccessMode:         tool.AccessMode(entry.Binding.AccessMode),
		ParallelPolicy:     tool.ParallelPolicy(entry.Binding.ParallelPolicy),
		RepeatPolicy:       tool.RepeatExecute,
		SandboxRequirement: tool.SandboxRequirement(entry.Binding.SandboxRequirement),
		Availability:       tool.AvailabilityAvailable,
	}
	return descriptor, nil
}

func (e *executor) Descriptor() tool.Descriptor {
	return e.descriptor
}

func (e *executor) TrustedBinding() tool.TrustedBinding {
	return trustedBindingFor(e.descriptor)
}

func (e *executor) Execute(
	ctx context.Context,
	arguments json.RawMessage,
) (tool.Result, error) {
	if e.entry.Authority != nil {
		if err := e.entry.Authority(ctx); err != nil {
			return tool.Result{}, fmt.Errorf("MCP capability authority: %w", err)
		}
	}
	// typed-boundary-exception: the remote catalog owns this dynamic schema.
	// Passing validated raw JSON avoids precision loss and provider-specific
	// value normalization at a boundary with no local static input type.
	result, err := e.entry.Connection.CallTool(ctx, e.entry.RemoteName, arguments)
	if err != nil {
		return tool.Result{}, err
	}
	var content strings.Builder
	for _, item := range result.Content {
		if item.Type == "text" {
			if content.Len() != 0 {
				content.WriteByte('\n')
			}
			content.WriteString(item.Text)
		}
	}
	metadata := map[string]any{
		"mcp_server":       e.entry.Server,
		"mcp_tool":         e.entry.RemoteName,
		"mcp_host_trusted": e.entry.HostTrusted,
		"mcp_content":      result.Content,
	}
	if len(result.StructuredContent) != 0 {
		var structured any
		if json.Unmarshal(result.StructuredContent, &structured) == nil {
			metadata["structured_content"] = structured
		}
	}
	return tool.Result{
		Content:  content.String(),
		IsError:  result.IsError,
		Metadata: metadata,
	}, nil
}

func newHelperExecutor(kind string, pool *Pool) *helperExecutor {
	properties := map[string]any{
		"server": map[string]any{
			"type":        "string",
			"description": "Optional configured MCP server name",
		},
	}
	required := []string{}
	description := ""
	switch kind {
	case "list_mcp_resources":
		description = "List resources and resource templates advertised by configured MCP servers"
	case "read_mcp_resource":
		description = "Read an advertised resource from a configured MCP server"
		properties["uri"] = map[string]any{"type": "string"}
		required = append(required, "uri")
	case "mcp_get_prompt":
		description = "Get an advertised prompt from a configured MCP server"
		properties["name"] = map[string]any{"type": "string"}
		properties["arguments"] = map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
		}
		required = append(required, "name")
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) != 0 {
		schema["required"] = required
	}
	return &helperExecutor{
		kind: kind,
		pool: pool,
		descriptor: tool.Descriptor{
			Name:               kind,
			Description:        description,
			InputSchema:        schema,
			Visibility:         tool.VisibleModel,
			Capability:         tool.CapabilityNetwork,
			AccessMode:         tool.AccessRead,
			ParallelPolicy:     tool.ParallelConcurrent,
			RepeatPolicy:       tool.RepeatExecute,
			SandboxRequirement: tool.SandboxNone,
			Availability:       tool.AvailabilityAvailable,
		},
	}
}

func (e *helperExecutor) Descriptor() tool.Descriptor {
	return e.descriptor
}

func (e *helperExecutor) Execute(
	ctx context.Context,
	arguments json.RawMessage,
) (tool.Result, error) {
	executor, err := e.typedExecutor()
	if err != nil {
		return tool.Result{}, err
	}
	return executor.Execute(ctx, arguments)
}

func (e *helperExecutor) typedExecutor() (tool.Executor, error) {
	return typed.Define(typed.Spec[helperInput, tool.Result]{
		Descriptor:  e.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Run:         e.run,
		Encode: func(value tool.Result) (tool.Result, error) {
			return value, nil
		},
	})
}

func (e *helperExecutor) run(ctx context.Context, input helperInput) (tool.Result, error) {
	switch e.kind {
	case "list_mcp_resources":
		var resources []map[string]any
		for _, entry := range e.pool.ResourceCatalog() {
			if input.Server == "" || entry.Server == input.Server {
				resources = append(resources, map[string]any{
					"server":   entry.Server,
					"resource": entry.Resource,
				})
			}
		}
		var templates []map[string]any
		for _, entry := range e.pool.ResourceTemplateCatalog() {
			if input.Server == "" || entry.Server == input.Server {
				templates = append(templates, map[string]any{
					"server":   entry.Server,
					"template": entry.Template,
				})
			}
		}
		return helperResult(map[string]any{
			"resources":         resources,
			"resourceTemplates": templates,
		})
	case "read_mcp_resource":
		for _, entry := range e.pool.ResourceCatalog() {
			if (input.Server == "" || entry.Server == input.Server) &&
				entry.Resource.URI == input.URI {
				result, err := entry.Connection.ReadResource(ctx, input.URI)
				if err != nil {
					return tool.Result{}, err
				}
				return helperResult(map[string]any{
					"server": entry.Server, "uri": input.URI, "result": result,
				})
			}
		}
		for _, entry := range e.pool.ResourceTemplateCatalog() {
			if input.Server != "" && entry.Server != input.Server {
				continue
			}
			result, err := entry.Connection.ReadResource(ctx, input.URI)
			if err == nil {
				return helperResult(map[string]any{
					"server": entry.Server, "uri": input.URI, "result": result,
				})
			}
			if errors.Is(err, ErrNotAdvertised) {
				continue
			}
			return tool.Result{}, err
		}
		return tool.Result{}, fmt.Errorf("MCP resource %q is not in the advertised catalog", input.URI)
	case "mcp_get_prompt":
		for _, entry := range e.pool.PromptCatalog() {
			if (input.Server == "" || entry.Server == input.Server) &&
				entry.Prompt.Name == input.Name {
				result, err := entry.Connection.GetPrompt(ctx, input.Name, input.Arguments)
				if err != nil {
					return tool.Result{}, err
				}
				return helperResult(map[string]any{
					"server": entry.Server, "name": input.Name, "result": result,
				})
			}
		}
		return tool.Result{}, fmt.Errorf("MCP prompt %q is not in the advertised catalog", input.Name)
	default:
		return tool.Result{}, errors.New("unknown MCP helper")
	}
}

func helperResult(value any) (tool.Result, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: string(content)}, nil
}
