package wire

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	agentengine "github.com/fwtllh-png/QCode/internal/runtime/agent/engine"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

type providerModule struct{}

func (providerModule) Name() string { return "provider" }

func (providerModule) Build(ctx context.Context, state *buildState) error {
	options := &state.options
	session := state.session
	execution := &state.config.execution
	if err := prepareFixture(options, execution, session); err != nil {
		return err
	}
	if err := prepareExecLogger(options, execution, session); err != nil {
		return err
	}
	routes, err := buildRouteSet(ctx, state)
	if err != nil {
		return err
	}
	egressGate := &egress.Gate{Enforce: true}
	grantRouteHosts(egressGate, routes)
	client := configureProviderClient(
		execution,
		egressGate,
		session.metrics,
		options.CredentialControl,
		routes.Act().Credential(),
	)
	runtimeProvider, err := newProviderRouter(client, routes)
	if err != nil {
		return err
	}
	capabilities := selectedModelCapabilities(routes.Act())
	// 热切换取决于是否注册了可选路由：默认连接上的附加模型，或默认
	// 连接之外的附加连接。单一模型连接保持 fixed。
	allowModelSelection := len(options.ModelMetadata.AdditionalDescriptors) != 0 ||
		len(options.ExtraConnections) != 0
	if allowModelSelection {
		capabilities.SelectionMode = "hot"
	} else {
		capabilities.SelectionMode = "fixed"
	}
	selectableRoutes, err := runtimeSelectableRoutes(
		routes.Act(),
		options.ModelMetadata.AdditionalDescriptors,
	)
	if err != nil {
		return fmt.Errorf("selectable model routes: %w", err)
	}
	extraRoutes, err := extraConnectionRoutes(options.ExtraConnections)
	if err != nil {
		return fmt.Errorf("extra connections: %w", err)
	}
	for key, route := range extraRoutes {
		if _, exists := selectableRoutes[key]; exists {
			continue
		}
		selectableRoutes[key] = route
		egressGate.AllowURL(route.Endpoint())
	}
	providerCatalog, modelCatalog := runtimeModelCatalog(
		routes.Act(),
		capabilities,
		selectableRoutes,
	)
	state.provider = providerBuildState{
		routes: routes, route: routes.Act(), selectableRoutes: selectableRoutes,
		egress: egressGate, provider: runtimeProvider,
		toolSampler:     agentengine.NewToolSampler(runtimeProvider),
		providerCatalog: providerCatalog, modelCatalog: modelCatalog,
		modelCapabilities: capabilities,
	}
	session.providerID, session.modelID = execution.Provider, execution.Model
	session.modelCapabilities = capabilities
	session.providerCatalog, session.modelCatalog = providerCatalog, modelCatalog
	return nil
}

func prepareFixture(
	options *ExecOptions,
	execution *config.Execution,
	session *Session,
) error {
	if options.FixturePath == "" {
		return nil
	}
	path, err := resolveFixturePath(options.FixturePath)
	if err != nil {
		return fmt.Errorf("provider fixture: %w", err)
	}
	server, err := fixture.Start(path)
	if err != nil {
		return fmt.Errorf("provider fixture: %w", err)
	}
	session.fixture = server
	execution.Provider, execution.Model = "fixture", server.Config.Model
	options.BaseURL, options.APIKeyEnv = server.URL, ""
	execution.Protocol = string(server.Config.Protocol)
	return nil
}

func prepareExecLogger(
	options *ExecOptions,
	execution *config.Execution,
	session *Session,
) error {
	if options.LogPath == "" {
		return nil
	}
	logFile, err := os.OpenFile(
		options.LogPath,
		os.O_WRONLY|os.O_CREATE|os.O_APPEND,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open exec log: %w", err)
	}
	session.logFile = logFile
	var secrets []string
	if value, exists := os.LookupEnv(options.APIKeyEnv); exists &&
		options.APIKeyEnv != "" {
		secrets = append(secrets, value)
	}
	session.logger = telemetry.NewJSONLogger(
		logFile,
		slog.LevelInfo,
		telemetry.NewRedactor(secrets...),
	)
	session.logger.Info(
		"exec started",
		"provider", execution.Provider,
		"model", execution.Model,
	)
	return nil
}

func buildRouteSet(
	ctx context.Context,
	state *buildState,
) (model.RouteSet, error) {
	options := &state.options
	execution := &state.config.execution
	wireProtocol, err := parseProtocol(execution.Protocol)
	if err != nil {
		return model.RouteSet{}, err
	}
	var routeModel *model.Model
	if state.session.fixture != nil {
		routeModel = fixtureModel(execution.Model)
	} else {
		routeModel, err = resolveModelMetadata(
			execution.Model,
			options.ModelMetadata,
		)
		if err != nil {
			return model.RouteSet{}, fmt.Errorf("model metadata: %w", err)
		}
	}
	credential := model.CredentialRef{
		Kind: state.config.snapshot.Config.Credential.Kind,
		Name: state.config.snapshot.Config.Credential.Name,
	}
	if state.session.fixture != nil {
		credential = model.CredentialRef{}
	}
	routes, err := resolveRouteSet(routeSetOptions{
		Act: execRouteOptions{
			ProviderID: execution.Provider, ModelID: execution.Model,
			BaseURL: options.BaseURL, Protocol: wireProtocol,
			APIKeyEnv: options.APIKeyEnv, Credential: credential,
			Fixture: state.session.fixture != nil, Model: routeModel,
		},
		Additional: options.ModelMetadata.AdditionalDescriptors,
		Extras:     options.ExtraConnections,
		Slots:      state.config.snapshot.Config.Route.Slots,
		Lock:       state.config.snapshot.Config.Route.Lock,
	})
	if err != nil {
		return model.RouteSet{}, fmt.Errorf("exec route: %w", err)
	}
	routes, err = overlayProbeCapabilities(
		ctx,
		routes,
		options.PersistentStore,
		options.TrustProbe,
	)
	if err != nil {
		return model.RouteSet{}, fmt.Errorf("capability probe overlay: %w", err)
	}
	return routes, nil
}

func selectedModelCapabilities(route model.ReadyRoute) protocol.ModelCapabilities {
	return catalogModelCapabilities(route.Model())
}
