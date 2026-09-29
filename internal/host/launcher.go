// Package host owns the single user-facing QCode host process.
package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	buildinfo "github.com/fwtllh-png/QCode/internal"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/platform/ownerlease"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
	webassets "github.com/fwtllh-png/QCode/web"
)

type webCommandOptions struct {
	workspace       string
	configPath      string
	dataDir         string
	port            int
	mcpConfig       string
	providerFixture string
}

const (
	webHost        = "127.0.0.1"
	defaultWebPort = 6732
)

var loadWebAssets = webassets.Assets

// RunContext parses process startup flags and runs the local Web workspace.
func RunContext(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
) int {
	options := webCommandOptions{}
	flags := flag.NewFlagSet("qcode", flag.ContinueOnError)
	flags.SetOutput(stderr)
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			flags.SetOutput(stdout)
			break
		}
	}
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: qcode [flags]")
		_, _ = fmt.Fprintln(flags.Output())
		_, _ = fmt.Fprintln(flags.Output(), "Run the local QCode Runtime. Normally launched by QCode.app.")
		flags.PrintDefaults()
	}
	flags.StringVar(&options.configPath, "config", "", "TOML configuration file")
	flags.StringVar(&options.dataDir, "data-dir", "", "persistent state directory")
	flags.IntVar(
		&options.port,
		"port",
		defaultWebPort,
		"listen port (0 selects an available port)",
	)
	flags.StringVar(&options.mcpConfig, "mcp-config", "", "versioned MCP stdio server config JSON")
	flags.StringVar(
		&options.providerFixture,
		"provider-fixture",
		"",
		"provider fixture directory",
	)
	showVersion := flags.Bool("version", false, "print version information")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "qcode: unexpected arguments: %v\n", flags.Args())
		return 2
	}
	if *showVersion {
		info := buildinfo.Current()
		_, _ = fmt.Fprintf(
			stdout,
			"%s %s (commit %s, built %s, %s, %s/%s)\n",
			info.Name,
			info.Version,
			info.Commit,
			info.BuildDate,
			info.GoVersion,
			info.OS,
			info.Arch,
		)
		return 0
	}
	return runWeb(ctx, options, stdout, stderr)
}

func runWeb(
	ctx context.Context,
	options webCommandOptions,
	stdout, stderr io.Writer,
) int {
	if options.port < 0 || options.port > 65535 {
		_, _ = fmt.Fprintln(stderr, "qcode: --port must be between 0 and 65535")
		return 2
	}
	bundle, err := loadWebAssets()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: assets: %v\n", err)
		return 1
	}
	loaded, configErr := loadWebConfig(options)
	var workspaceRoot, dataDir string
	var workspaceIdentity protocol.WorkspaceIdentity
	if configErr == nil {
		if loaded.Provenance["execution.workspace"] != config.SourceDefault {
			workspaceRoot, workspaceIdentity, configErr = normalizeWorkspaceRoot(
				loaded.Config.Execution.Workspace,
			)
		}
		dataDir = loaded.Config.State.DataDir
		options.workspace = workspaceRoot
	}
	if configErr == nil {
		if workspaceRoot == "" {
			dataDir, configErr = wire.ResolveSupervisorStateDirectory(dataDir)
		} else {
			dataDir, configErr = wire.ValidateExternalStateDirectory(workspaceRoot, dataDir)
		}
		if configErr == nil {
			// Keep subsequent setup reloads and Runtime wiring on the same
			// canonical state root used by the persistent store.
			options.dataDir = dataDir
			loaded.Config.State.DataDir = dataDir
		}
	}
	selection := webSetupSelection{}
	routeReference := credential.Reference{}
	setupRequired := false
	if configErr == nil {
		providerConfigured := loaded.Config.Execution.Provider != ""
		modelConfigured := loaded.Config.Execution.Model != ""
		switch {
		case providerConfigured != modelConfigured:
			configErr = errors.New("provider and model must be configured together")
		case !providerConfigured:
			var found bool
			selection, found, configErr = loadWebSetupSelection(
				dataDir, workspaceIdentity.RootID,
			)
			if configErr == nil && found {
				active := selection.Active()
				_, reference, resolveErr := resolveWebSetup(SetupRequest{
					Model:   active.Model,
					BaseURL: active.BaseURL, Protocol: active.Protocol,
					APIKey: "persisted", ModelMetadata: active.Metadata,
				})
				if resolveErr != nil {
					configErr = resolveErr
				} else {
					routeReference = reference
					if active.Credential != nil {
						routeReference = *active.Credential
					}
					loaded, configErr = loadWebSetupConfig(
						options,
						selection,
						routeReference,
					)
				}
			} else if configErr == nil {
				setupRequired = true
			}
		case loaded.Config.Execution.BaseURL == "" && options.providerFixture == "":
			configErr = errors.New(
				"execution.base_url is required with a configured provider; " +
					"connections are OpenAI-compatible and explicit")
		default:
			// 显式配置的单连接会话：provider/model/protocol/base_url
			// 全部显式声明；模型元数据来自 execution.model_metadata 文件。
			// fixture 会话使用内置 fixture 模型描述与 fixture 端点。
			execution := loaded.Config.Execution
			protocol := execution.Protocol
			if protocol == "" {
				protocol = string(model.ProtocolOpenAIChat)
			}
			var metadata *SetupModelMetadata
			if options.providerFixture == "" {
				var metadataErr error
				metadata, metadataErr = loadExecutionModelMetadata(execution)
				if metadataErr != nil {
					configErr = metadataErr
					break
				}
			}
			id := connectionID(execution.BaseURL)
			provenance := model.ProvenanceConfig
			if options.providerFixture != "" {
				id = execution.Provider
				provenance = model.ProvenanceFixture
			}
			connection := webSetupConnection{
				ID:                 id,
				Provider:           id,
				Model:              execution.Model,
				BaseURL:            execution.BaseURL,
				Protocol:           protocol,
				Metadata:           metadata,
				MetadataProvenance: provenance,
			}
			selection = webSetupSelection{
				Version:           webSetupVersion,
				Connections:       []webSetupConnection{connection},
				DefaultConnection: connection.ID,
			}
			if !loaded.Config.Credential.Empty() {
				routeReference = credential.Reference{
					Kind: loaded.Config.Credential.Kind,
					Name: loaded.Config.Credential.Name,
				}
			}
		}
	}
	var workspaceManager *workspaceRuntimeManager
	if configErr == nil {
		workspaceManager, configErr = newWorkspaceRuntimeManager(dataDir, workspaceRoot)
	}

	var lease *ownerlease.Lease
	if configErr == nil {
		info := buildinfo.Current()
		leasePath := ownerlease.Path(dataDir, webSupervisorScope)
		ownerMetadata := ownerlease.Metadata{
			OwnerKind: "web",
			Build:     webOwnerBuild(info),
		}
		lease, err = ownerlease.Acquire(leasePath, ownerMetadata)
		if err != nil {
			var held *ownerlease.HeldError
			if errors.As(err, &held) && held.Metadata.PublicURL != "" {
				if status, probeErr := probeWebStatus(
					ctx,
					held.Metadata.PublicURL,
				); probeErr == nil {
					targetURL := held.Metadata.PublicURL
					if workspaceRoot != "" && held.Metadata.CapabilityToken != "" {
						workspaceID, registerErr := registerWorkspaceWithOwner(
							ctx,
							held.Metadata.PublicURL,
							held.Metadata.CapabilityToken,
							workspaceRoot,
						)
						if registerErr != nil {
							_, _ = fmt.Fprintf(
								stderr,
								"qcode: register Workspace: %v\n",
								registerErr,
							)
							return 1
						}
						if workspaceID != "" {
							targetURL += "?workspace=" + url.QueryEscape(workspaceID)
						}
					}
					launchURL := func() string {
						if held.Metadata.CapabilityToken == "" {
							return targetURL
						}
						value, launchErr := ownerLaunchURL(
							ctx,
							held.Metadata.PublicURL,
							held.Metadata.CapabilityToken,
							targetURL,
						)
						if launchErr != nil {
							_, _ = fmt.Fprintf(stderr, "qcode: launch code: %v\n", launchErr)
							return targetURL
						}
						return value
					}
					readyLabel := "Runtime Ready"
					if status == "setup_required" {
						readyLabel = "Setup Ready"
					}
					_, _ = fmt.Fprintf(
						stdout,
						"QCode %s: %s\n",
						readyLabel,
						launchURL(),
					)
					return 0
				} else {
					_, _ = fmt.Fprintf(
						stderr,
						"qcode: owner lease URL failed readiness probe: %v\n",
						probeErr,
					)
				}
			} else {
				_, _ = fmt.Fprintf(stderr, "qcode: owner lease: %v\n", err)
			}
			return 1
		}
		defer lease.Close()
	}

	listener, err := net.Listen(
		"tcp",
		net.JoinHostPort(webHost, strconv.Itoa(options.port)),
	)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: listen: %v\n", err)
		return 1
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	hostPort := net.JoinHostPort(webHost, strconv.Itoa(address.Port))
	publicURL := "http://" + hostPort + "/"
	workspaceURL := publicURL
	if workspaceRoot != "" {
		workspaceURL += "?workspace=" + url.QueryEscape(workspaceIdentity.RootID)
	}
	info := buildinfo.Current()
	setupRequests := make(chan webSetupAttempt)
	setupOptions := &SetupOptions{
		WorkspaceRoot: workspaceRoot, WorkspaceIdentity: workspaceIdentity,
		Probe: func(
			requestContext context.Context,
			request SetupProbeRequest,
		) (SetupProbeResult, error) {
			providerID, baseURL, protocol, probeErr := resolveSetupProbeConnection(request)
			if probeErr != nil {
				return SetupProbeResult{}, probeErr
			}
			modelID := strings.TrimSpace(request.Model)
			if !setupModelIDPattern.MatchString(modelID) {
				return SetupProbeResult{}, invalidSetup(
					"custom provider model id is invalid",
				)
			}
			var reference credential.Reference
			if strings.TrimSpace(request.APIKey) != "" {
				reference = credential.Reference{}
			} else {
				_, recovered, credentialErr := credential.OpenControl(
					requestContext,
					dataDir,
					webSupervisorScope,
					providerID,
					credential.Reference{},
					credential.Reference{},
				)
				if credentialErr != nil {
					return SetupProbeResult{}, credentialErr
				}
				reference = recovered
			}
			probed, probeErr := wire.ProbeModelConnection(
				requestContext,
				providerID,
				baseURL,
				modelID,
				strings.TrimSpace(request.APIKey),
				model.CredentialRef{
					Kind: reference.Kind,
					Name: reference.Name,
				},
				protocol,
			)
			if probeErr != nil {
				return SetupProbeResult{}, probeErr
			}
			result := setupProbeResult(probed)
			if !slices.ContainsFunc(result.Models, func(
				value SetupDiscoveredModel,
			) bool {
				return value.ID == modelID
			}) {
				result.Models = append(result.Models, SetupDiscoveredModel{
					ID: modelID,
				})
			}
			return result, nil
		},
		Apply: func(
			requestContext context.Context,
			request SetupRequest,
		) error {
			if !setupRequired || workspaceManager.Configured() {
				return workspaceManager.Reconfigure(requestContext, request)
			}
			attempt := webSetupAttempt{request: request, result: make(chan error, 1)}
			select {
			case setupRequests <- attempt:
			case <-requestContext.Done():
				return requestContext.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-attempt.result:
				return err
			case <-requestContext.Done():
				return requestContext.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	setupOptions.Connections = workspaceManager
	server, err := New(Options{
		Assets: bundle, ExpectedHost: hostPort, Origin: "http://" + hostPort,
		Build: info.Version + "+" + info.Commit, Setup: setupOptions,
		Workspaces: workspaceManager, Models: workspaceManager,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: server: %v\n", err)
		return 1
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	_, _ = fmt.Fprintf(stdout, "QCode Web Listening: %s\n", publicURL)
	if lease != nil {
		metadata := ownerlease.Metadata{
			OwnerKind:       "web",
			Build:           webOwnerBuild(buildinfo.Current()),
			PublicURL:       publicURL,
			CapabilityToken: server.CapabilityToken(),
		}
		if err := lease.Update(metadata); err != nil {
			server.FailBoot(err)
			_, _ = fmt.Fprintf(stderr, "qcode: owner lease metadata: %v\n", err)
			return shutdownBootServer(httpServer, serveErr, nil, nil, stderr, 1)
		}
	}

	launchURL := func() string {
		value, launchErr := server.LaunchURL(workspaceURL)
		if launchErr != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: launch code: %v\n", launchErr)
			return workspaceURL
		}
		return value
	}
	if configErr != nil {
		server.FailBoot(configErr)
		_, _ = fmt.Fprintf(stderr, "qcode: config: %v\n", configErr)
		return waitForWebShutdown(ctx, httpServer, serveErr, server, nil, nil, stderr, 1)
	}

	store, err := state.Open(ctx, state.Options{
		DataDir: dataDir, BusyTimeout: loaded.Config.State.BusyTimeout,
		DeletedEventRetention: loaded.Config.State.DeletedEventRetention,
		ArchiveDeletedEvents:  loaded.Config.State.ArchiveDeletedEvents,
	})
	if err != nil {
		server.FailBoot(err)
		_, _ = fmt.Fprintf(stderr, "qcode: state: %v\n", err)
		return waitForWebShutdown(ctx, httpServer, serveErr, server, nil, nil, stderr, 1)
	}
	repositories, err := apppersistence.NewPersistentRepositories(store)
	if err != nil {
		server.FailBoot(err)
		_, _ = fmt.Fprintf(stderr, "qcode: repositories: %v\n", err)
		return waitForWebShutdown(ctx, httpServer, serveErr, server, nil, store, stderr, 1)
	}
	workspaceManager.Bind(server, options, store, repositories, stderr)
	activate := func(
		candidate config.Snapshot,
		candidateSelection webSetupSelection,
		reference credential.Reference,
		secret string,
		persist bool,
	) error {
		if workspaceRoot == "" {
			return workspaceManager.configureWithoutRuntime(
				ctx, candidateSelection, reference, secret, persist,
			)
		}
		selectionPersisted := false
		prepared, prepareErr := prepareWebRuntime(
			ctx, options, candidate, candidateSelection, workspaceRoot,
			workspaceIdentity, store, repositories, stderr, secret, nil,
		)
		if prepareErr == nil && prepared.credentials.bindTo(candidateSelection, "") {
			reference = prepared.credentials.Reference()
		}
		if prepareErr == nil && persist {
			prepareErr = saveWebSetupSelection(
				dataDir, workspaceIdentity.RootID, candidateSelection,
			)
			selectionPersisted = prepareErr == nil
		}
		if prepareErr == nil {
			workspaceManager.SetRoute(candidateSelection, reference)
			prepareErr = workspaceManager.Persist()
		}
		if prepareErr == nil && !persist {
			prepareErr = repositories.Sessions.RebindWorkspaceProfiles(
				ctx,
				[]string{workspaceRoot},
				prepared.application.DefaultProfile(),
			)
		}
		if prepareErr == nil {
			prepareErr = prepared.credentials.Activate()
		}
		if prepareErr == nil {
			prepareErr = server.Activate(prepared.dependenciesWithDiagnostics(stderr))
		}
		if prepareErr != nil {
			if selectionPersisted {
				removeErr := os.Remove(
					setupSelectionPath(dataDir, workspaceIdentity.RootID),
				)
				if !errors.Is(removeErr, os.ErrNotExist) {
					prepareErr = errors.Join(prepareErr, removeErr)
				}
			}
			if prepared != nil {
				prepared.close()
				prepared.credentials.settle(&prepareErr, stderr)
			}
			workspaceManager.SetRoute(selection, routeReference)
			return prepareErr
		}
		prepared.credentials.commitOrReport(stderr)
		workspaceManager.RegisterInitial(workspaceIdentity, prepared)
		loaded = candidate
		selection = candidateSelection
		routeReference = reference
		return nil
	}

	if setupRequired {
		_, _ = fmt.Fprintf(stdout, "QCode Setup Ready: %s\n", launchURL())
		for !workspaceManager.Configured() {
			select {
			case attempt := <-setupRequests:
				connection, reference, setupErr := resolveWebSetup(attempt.request)
				var candidateSelection webSetupSelection
				var candidate config.Snapshot
				if setupErr == nil {
					candidateSelection = mergeConnection(selection, connection)
					candidate, setupErr = loadWebSetupConfig(
						options, candidateSelection, reference,
					)
				}
				if setupErr == nil {
					setupErr = activate(
						candidate, candidateSelection, reference,
						attempt.request.APIKey, true,
					)
				}
				attempt.result <- setupErr
			case serveFailure := <-serveErr:
				if serveFailure != nil {
					_, _ = fmt.Fprintf(stderr, "qcode: serve: %v\n", serveFailure)
				}
				return shutdownBootServer(
					httpServer, serveErr, nil, store, stderr, 1,
				)
			case <-ctx.Done():
				return shutdownBootServer(
					httpServer, serveErr, nil, store, stderr, 0,
				)
			}
		}
	} else if err := activate(
		loaded, selection, routeReference, "", false,
	); err != nil {
		server.FailBoot(err)
		_, _ = fmt.Fprintf(stderr, "qcode: Runtime: %v\n", err)
		return waitForWebShutdown(
			ctx, httpServer, serveErr, server, nil, store, stderr, 1,
		)
	}
	workspaceManager.ActivateRegistered(ctx)
	_, _ = fmt.Fprintf(stdout, "QCode Runtime Ready: %s\n", launchURL())
	return waitForWebShutdown(
		ctx,
		httpServer,
		serveErr,
		server,
		workspaceManager,
		store,
		stderr,
		0,
	)
}

func probeWebReadiness(ctx context.Context, rawURL string) error {
	_, err := probeWebStatus(ctx, rawURL)
	return err
}

func probeWebStatus(ctx context.Context, rawURL string) (string, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if target.Scheme != "http" || target.Hostname() != "127.0.0.1" ||
		target.User != nil {
		return "", errors.New("owner URL is not a trusted loopback HTTP endpoint")
	}
	target.Path = "/healthz"
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	probeContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		probeContext,
		http.MethodGet,
		target.String(),
		nil,
	)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("owner readiness redirects are forbidden")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var readiness struct {
		Version int    `json:"version"`
		Status  string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&readiness); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK ||
		readiness.Version != 1 ||
		(readiness.Status != "ready" && readiness.Status != "setup_required") {
		return "", fmt.Errorf(
			"owner endpoint is not ready (HTTP %d, protocol %d, status %q)",
			response.StatusCode,
			readiness.Version,
			readiness.Status,
		)
	}
	return readiness.Status, nil
}

func registerWorkspaceWithOwner(
	ctx context.Context,
	rawURL string,
	token string,
	workspaceRoot string,
) (string, error) {
	digest := sha256.Sum256([]byte(workspaceRoot))
	var result WorkspaceAddResult
	if err := callOwner(
		ctx, rawURL, token, "workspace/add",
		WorkspaceAddRequest{Path: workspaceRoot},
		"workspace-register-"+hex.EncodeToString(digest[:]),
		&result,
	); err != nil {
		return "", err
	}
	return result.Workspace.ID, nil
}

// ownerLaunchURL asks a running owner for a one-time launch code so the
// browser opened by this invocation gets its own session cookie.
func ownerLaunchURL(ctx context.Context, rawURL, token, targetURL string) (string, error) {
	var result LaunchCodeResult
	if err := callOwner(
		ctx, rawURL, token, "auth/launch-code", struct{}{}, "", &result,
	); err != nil {
		return "", err
	}
	return WithLaunchCode(targetURL, result.Code)
}

func callOwner(
	ctx context.Context,
	rawURL, token, route string,
	body any,
	idempotencyKey string,
	result any,
) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("owner capability token is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	target, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if target.Scheme != "http" || target.Hostname() != "127.0.0.1" ||
		target.User != nil {
		return errors.New("owner URL is not a trusted loopback HTTP endpoint")
	}
	target.Path = "/api/v1/" + route
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		requestContext,
		http.MethodPost,
		target.String(),
		bytes.NewReader(encoded),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-QCode-Request-ID", "owner-"+strings.ReplaceAll(route, "/", "-"))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("owner Web API redirects are forbidden")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	envelope := struct {
		Result  any               `json:"result"`
		Problem *protocol.Problem `json:"problem,omitempty"`
	}{Result: result}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		if envelope.Problem != nil {
			return errors.New(envelope.Problem.Message)
		}
		return fmt.Errorf("owner Web API %s failed (HTTP %d)", route, response.StatusCode)
	}
	return nil
}

func loadWebConfig(options webCommandOptions) (config.Snapshot, error) {
	return loadWebConfigWithOverrides(options.configPath, webConfigOverrides(options))
}

func loadWebConfigWithOverrides(path string, overrides config.Overrides) (config.Snapshot, error) {
	loaded, err := config.Load(config.LoadOptions{
		Path: path, Overrides: overrides,
	})
	if err == nil && loaded.Provenance["execution.tools"] == config.SourceDefault {
		// Desktop workspaces enable built-in tools by default. Explicit TOML
		// or environment choices retain precedence, including tools = false.
		loaded.Config.Execution.Tools = true
	}
	return loaded, err
}

func webOwnerBuild(info buildinfo.Info) string {
	return info.Version + "+" + info.Commit + "@" + info.BuildDate
}

type preparedWebRuntime struct {
	credentials  *credentialRotation
	application  *wire.Session
	extensions   *wire.SkillControlHandle
	dependencies Dependencies
}

// prepareWebRuntime builds a runtime for selection's active connection. With
// inherited set, the runtime shares that rotation's Control and reference and
// its own rotation stays idle: the inherited rotation's owner settles it.
func prepareWebRuntime(
	ctx context.Context,
	options webCommandOptions,
	loaded config.Snapshot,
	selection webSetupSelection,
	workspaceRoot string,
	workspaceIdentity protocol.WorkspaceIdentity,
	store *state.Store,
	repositories apppersistence.PersistentRepositories,
	stderr io.Writer,
	secret string,
	inherited *credentialRotation,
) (_ *preparedWebRuntime, resultErr error) {
	active := selection.Active()
	if active == nil {
		return nil, errors.New("no model connection is configured")
	}
	rotation, err := openCredentialRotation(ctx, loaded, selection, "", secret, inherited)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, rotation.Rollback())
		}
	}()
	credentialControl := rotation.Control()
	effectiveCredential := rotation.Reference()

	runtimeOverrides := webConfigOverrides(options)
	runtimeOverrides.Tools = &loaded.Config.Execution.Tools
	runtimeOverrides.Provider = &loaded.Config.Execution.Provider
	runtimeOverrides.Model = &loaded.Config.Execution.Model
	runtimeOverrides.Protocol = &loaded.Config.Execution.Protocol
	runtimeOverrides.CredentialKind = &effectiveCredential.Kind
	runtimeOverrides.CredentialName = &effectiveCredential.Name
	extraConnections := make([]wire.ExtraConnectionSpec, 0,
		len(selection.Connections)-1)
	for index := range selection.Connections {
		connection := selection.Connections[index]
		if connection.ID == active.ID {
			continue
		}
		// fixture 连接（BaseURL 由 fixture 服务器提供时为空）只作为活动
		// 连接存在，不进入附加连接。
		if connection.BaseURL == "" {
			continue
		}
		metadata := setupModelMetadata(connection)
		spec := wire.ExtraConnectionSpec{
			ProviderID: connection.ID,
			Protocol:   model.WireProtocol(connection.Protocol),
			BaseURL:    connection.BaseURL,
			Model:      metadata.Descriptor,
			Models:     metadata.AdditionalDescriptors,
		}
		if connection.Credential != nil {
			spec.Credential = model.CredentialRef{
				Kind: connection.Credential.Kind,
				Name: connection.Credential.Name,
			}
		}
		extraConnections = append(extraConnections, spec)
	}
	skillOptions := wire.SkillOptions{DataDir: loaded.Config.State.DataDir}
	application, err := wire.NewExec(ctx, wire.ExecOptions{
		ConfigPath:        options.configPath,
		ConfigOverrides:   runtimeOverrides,
		ExtraConnections:  extraConnections,
		BaseURL:           active.BaseURL,
		FixturePath:       options.providerFixture,
		Permission:        "auto",
		MCPConfigPath:     options.mcpConfig,
		PersistentStore:   store,
		CredentialControl: credentialControl,
		WorkspaceIdentity: workspaceIdentity,
		Skills:            skillOptions,
		ModelMetadata:     setupModelMetadata(*active),
	})
	if err != nil {
		return nil, err
	}
	skillPaths, err := wire.ResolveSkillPaths(skillOptions, workspaceRoot)
	if err != nil {
		closeWebRuntime(application)
		return nil, fmt.Errorf("extension paths: %w", err)
	}
	extensions, err := application.OpenSkillControl(skillPaths, workspaceRoot)
	if err != nil {
		closeWebRuntime(application)
		return nil, fmt.Errorf("extension control: %w", err)
	}
	credentialOptions := []credential.Option{
		credential.WithControl(credentialControl),
		credential.WithLiveReload(),
	}
	credentialOptions = append(credentialOptions, credential.WithProbe(func(
		ctx context.Context,
		reference credential.Reference,
	) error {
		if strings.TrimSpace(options.providerFixture) != "" {
			return nil
		}
		available, probeErr := wire.ProbeLiveModel(
			ctx, application.ProviderID(), active.BaseURL,
			model.CredentialRef{Kind: reference.Kind, Name: reference.Name},
			setupWireModelID(*active, active.Model),
		)
		if probeErr != nil {
			return probeErr
		}
		if !available {
			return errors.New("configured model is not listed by provider")
		}
		return nil
	}))
	credentials := credential.New(effectiveCredential, credentialOptions...)
	modelProbe := func(ctx context.Context, modelID string) (bool, error) {
		if strings.TrimSpace(options.providerFixture) != "" {
			return true, nil
		}
		status, statusErr := credentials.Status(ctx)
		if statusErr != nil {
			return false, statusErr
		}
		return wire.ProbeLiveModel(
			ctx,
			application.ProviderID(),
			active.BaseURL,
			model.CredentialRef{
				Kind: status.Reference.Kind,
				Name: status.Reference.Name,
			},
			setupWireModelID(*active, modelID),
		)
	}
	connection := WorkspaceConnection{
		Provider:      active.Provider,
		Endpoint:      active.BaseURL,
		Protocol:      active.Protocol,
		ModelMetadata: active.Metadata,
	}
	return &preparedWebRuntime{
		credentials: rotation,
		application: application, extensions: extensions,
		dependencies: Dependencies{
			Runtime: application.Runtime, WorkspaceRoot: workspaceRoot,
			WorkspaceIdentity: workspaceIdentity,
			DefaultProfile:    application.DefaultProfile(),
			ProviderCatalog:   application.ProviderCatalog(),
			ModelCatalog:      application.ModelCatalog(), Connection: connection,
			MCPHealth:   application.MCPHealth,
			Diagnostics: stderr, Usage: repositories.Usage,
			Agents: application.Subagents(), Extensions: extensions.Service,
			SessionWorkspaces: application.SessionWorkspaces(),
			Workspace:         application.WorkspaceQuery(),
			RepositoryIndex:   application.RepositoryIndex(), Credentials: credentials,
			ModelProbe: modelProbe,
		},
	}, nil
}

// openCredentialRotation 在指定连接（缺省为活动连接）的 provider 命名空间
// 打开 Control 并暂存密钥；connection/add 用它定向到非默认连接。inherited
// 非空时复用其 Control 与引用，不再打开新的命名空间。
func openCredentialRotation(
	ctx context.Context,
	loaded config.Snapshot,
	selection webSetupSelection,
	targetConnectionID string,
	secret string,
	inherited *credentialRotation,
) (*credentialRotation, error) {
	credentialControl := inherited.Control()
	active := selection.Active()
	if active == nil {
		return nil, errors.New("no model connection is configured")
	}
	target := active
	if targetConnectionID != "" && targetConnectionID != active.ID {
		if connection := selection.Connection(targetConnectionID); connection != nil {
			target = connection
		}
	}
	effectiveCredential := inherited.Reference()
	if credentialControl == nil {
		var err error
		credentialControl, effectiveCredential, err = credential.OpenControl(
			ctx,
			loaded.Config.State.DataDir,
			webSupervisorScope,
			target.Provider,
			func() credential.Reference {
				if target.ID != active.ID {
					if target.Credential != nil {
						return *target.Credential
					}
					return credential.Reference{}
				}
				return credential.Reference{
					Kind: loaded.Config.Credential.Kind,
					Name: loaded.Config.Credential.Name,
				}
			}(),
			func() credential.Reference {
				if target.Credential == nil {
					return credential.Reference{}
				}
				return *target.Credential
			}(),
		)
		if err != nil {
			return nil, fmt.Errorf("credential recovery: %w", err)
		}
	}
	return stageCredentialRotation(ctx, credentialControl, effectiveCredential, secret)
}

func (p *preparedWebRuntime) dependenciesWithDiagnostics(
	diagnostics io.Writer,
) Dependencies {
	result := p.dependencies
	result.Diagnostics = diagnostics
	return result
}

func (p *preparedWebRuntime) close() {
	if p == nil {
		return
	}
	if p.extensions != nil {
		_ = p.extensions.Close()
	}
	closeWebRuntime(p.application)
}

func closeWebRuntime(application *wire.Session) {
	if application == nil {
		return
	}
	closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = application.Close(closeContext)
}

func webConfigOverrides(options webCommandOptions) config.Overrides {
	overrides := config.Overrides{}
	if options.workspace != "" {
		overrides.Workspace = &options.workspace
	}
	if options.dataDir != "" {
		overrides.StateDataDir = &options.dataDir
	}
	return overrides
}

func waitForWebShutdown(
	ctx context.Context,
	httpServer *http.Server,
	serveErr <-chan error,
	server *Server,
	runtimeResource interface {
		Close(context.Context) error
	},
	store *state.Store,
	stderr io.Writer,
	code int,
) int {
	select {
	case err := <-serveErr:
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: serve: %v\n", err)
			code = 1
		}
	case <-ctx.Done():
	}
	server.Drain()
	return shutdownBootServer(httpServer, serveErr, runtimeResource, store, stderr, code)
}

func shutdownBootServer(
	httpServer *http.Server,
	serveErr <-chan error,
	runtimeResource interface {
		Close(context.Context) error
	},
	store *state.Store,
	stderr io.Writer,
	code int,
) int {
	httpContext, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
	if err := httpServer.Shutdown(httpContext); err != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: shutdown: %v\n", err)
		code = 1
	}
	cancelHTTP()
	select {
	case err := <-serveErr:
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: serve: %v\n", err)
			code = 1
		}
	default:
	}
	if runtimeResource != nil {
		runtimeContext, cancelRuntime := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		if err := runtimeResource.Close(runtimeContext); err != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: Runtime close: %v\n", err)
			code = 1
		}
		cancelRuntime()
	}
	if store != nil {
		storeContext, cancelStore := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		if err := store.CloseAll(storeContext); err != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: state close: %v\n", err)
			code = 1
		}
		cancelStore()
	}
	return code
}
