// Package web owns the single user-facing QCode host process.
package web

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
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/buildinfo"
	"github.com/fwtllh-png/QCode/internal/config"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
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
	host            string
	port            int
	open            bool
	noOpen          bool
	replaceOwner    bool
	enableTools     bool
	posture         string
	mcpConfig       string
	provider        string
	model           string
	apiKeyEnv       string
	providerFixture string
}

const defaultWebPort = 6732

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
		_, _ = fmt.Fprintln(flags.Output(), "Run the local QCode Web workspace.")
		flags.PrintDefaults()
	}
	flags.StringVar(&options.workspace, "workspace", "", "explicit workspace root to add and open")
	flags.StringVar(&options.configPath, "config", "", "TOML configuration file")
	flags.StringVar(&options.dataDir, "data-dir", "", "persistent state directory")
	flags.StringVar(&options.host, "host", "127.0.0.1", "loopback listen host")
	flags.IntVar(
		&options.port,
		"port",
		defaultWebPort,
		"listen port (0 selects an available port)",
	)
	flags.BoolVar(&options.open, "open", false, "open the Web workspace in a browser")
	flags.BoolVar(&options.noOpen, "no-open", false, "do not open a browser")
	flags.BoolVar(
		&options.replaceOwner,
		"replace-owner",
		false,
		"restart an older Web owner instead of reusing it",
	)
	flags.BoolVar(&options.enableTools, "enable-tools", false, "enable built-in workspace tools")
	flags.StringVar(&options.posture, "posture", "auto", "tool permission posture")
	flags.StringVar(&options.mcpConfig, "mcp-config", "", "versioned MCP stdio server config JSON")
	flags.StringVar(&options.provider, "provider", "", "explicit provider id")
	flags.StringVar(&options.model, "model", "", "model id within the selected provider")
	flags.StringVar(&options.apiKeyEnv, "api-key-env", "", "provider credential environment variable")
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
	if options.configPath == "" && !webFlagProvided(args, "enable-tools") {
		options.enableTools = true
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
	if !options.open && !options.noOpen {
		options.open = terminalWriter(stdout)
	}
	return runWeb(ctx, options, stdout, stderr)
}

func runWeb(
	ctx context.Context,
	options webCommandOptions,
	stdout, stderr io.Writer,
) int {
	if options.host != "127.0.0.1" {
		_, _ = fmt.Fprintln(stderr, "qcode: --host must be 127.0.0.1")
		return 2
	}
	if options.port < 0 || options.port > 65535 {
		_, _ = fmt.Fprintln(stderr, "qcode: --port must be between 0 and 65535")
		return 2
	}
	if !oneOf(options.posture, "suggest", "auto", "never") {
		_, _ = fmt.Fprintln(stderr, "qcode: --posture must be suggest, auto, or never")
		return 2
	}
	if options.open && options.noOpen {
		_, _ = fmt.Fprintln(stderr, "qcode: --open and --no-open are mutually exclusive")
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
				_, reference, resolveErr := resolveWebSetup(webhost.SetupRequest{
					Provider: selection.Provider, Model: selection.Model,
					BaseURL: selection.BaseURL, Protocol: selection.Protocol,
					APIKey: "persisted", ModelMetadata: selection.Metadata,
				})
				if resolveErr != nil {
					configErr = resolveErr
				} else {
					routeReference = reference
					if selection.Credential != nil {
						routeReference = *selection.Credential
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
		default:
			selection = webSetupSelection{
				Version:            webSetupVersion,
				Provider:           loaded.Config.Execution.Provider,
				Model:              loaded.Config.Execution.Model,
				Protocol:           loaded.Config.Execution.Protocol,
				MetadataProvenance: model.ProvenanceBundled,
			}
			if options.providerFixture != "" {
				selection.MetadataProvenance = model.ProvenanceFixture
				break
			}
			if loaded.Config.Credential.Empty() {
				if provider, exists := model.DefaultCatalog().Provider(selection.Provider); exists {
					selection.Protocol = string(provider.Protocol)
					routeReference = credential.Reference{
						Kind: provider.Credential.Kind,
						Name: provider.Credential.Name,
					}
					loaded, configErr = loadWebSetupConfig(
						options,
						selection,
						routeReference,
					)
				}
			} else {
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
			OwnerKind: webOwnerKind(options.replaceOwner),
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
					if options.replaceOwner &&
						held.Metadata.Build != ownerMetadata.Build {
						_, _ = fmt.Fprintf(
							stdout,
							"QCode Dev Restart: stopping build %s (pid %d)\n",
							held.Metadata.Build,
							held.Metadata.PID,
						)
						lease, err = replaceWebOwner(
							ctx,
							leasePath,
							ownerMetadata,
							held.Metadata,
							signalWebOwner,
						)
						if err != nil {
							_, _ = fmt.Fprintf(
								stderr,
								"qcode: replace Web owner: %v\n",
								err,
							)
							return 1
						}
					} else {
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
						readyLabel := "Runtime Ready"
						if status == "setup_required" {
							readyLabel = "Setup Ready"
						}
						_, _ = fmt.Fprintf(
							stdout,
							"QCode %s: %s\n",
							readyLabel,
							targetURL,
						)
						if options.open && !options.noOpen {
							if openErr := openWebBrowser(targetURL); openErr != nil {
								_, _ = fmt.Fprintf(stderr, "qcode: open browser: %v\n", openErr)
							}
						}
						return 0
					}
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
			if lease == nil {
				return 1
			}
		}
		defer lease.Close()
	}

	listener, err := net.Listen(
		"tcp",
		net.JoinHostPort(options.host, strconv.Itoa(options.port)),
	)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: listen: %v\n", err)
		return 1
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	hostPort := net.JoinHostPort(options.host, strconv.Itoa(address.Port))
	publicURL := "http://" + hostPort + "/"
	workspaceURL := publicURL
	if workspaceRoot != "" {
		workspaceURL += "?workspace=" + url.QueryEscape(workspaceIdentity.RootID)
	}
	info := buildinfo.Current()
	setupRequests := make(chan webSetupAttempt)
	setupOptions := &webhost.SetupOptions{
		WorkspaceRoot: workspaceRoot, WorkspaceIdentity: workspaceIdentity,
		Catalog: webSetupCatalog(),
		Probe: func(
			requestContext context.Context,
			request webhost.SetupProbeRequest,
		) (webhost.SetupProbeResult, error) {
			providerID, baseURL, protocol, probeErr := resolveSetupProbeConnection(request)
			if probeErr != nil {
				return webhost.SetupProbeResult{}, probeErr
			}
			modelID := strings.TrimSpace(request.Model)
			if !setupModelIDPattern.MatchString(modelID) {
				return webhost.SetupProbeResult{}, invalidSetup(
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
					request.Provider,
					credential.Reference{},
					credential.Reference{},
				)
				if credentialErr != nil {
					return webhost.SetupProbeResult{}, credentialErr
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
				return webhost.SetupProbeResult{}, probeErr
			}
			result := webhost.SetupProbeResult{
				Capabilities: setupCapabilitiesDTO(probed.Capabilities),
			}
			for _, value := range probed.Models {
				result.Models = append(result.Models, webhost.SetupDiscoveredModel{
					ID: value.ID, Name: value.Name,
					ContextTokens:   value.ContextTokens,
					MaxOutputTokens: value.MaxOutputTokens,
				})
			}
			if !slices.ContainsFunc(result.Models, func(
				value webhost.SetupDiscoveredModel,
			) bool {
				return value.ID == modelID
			}) {
				result.Models = append(result.Models, webhost.SetupDiscoveredModel{
					ID: modelID,
				})
			}
			result.Warning = probed.Warning
			return result, nil
		},
		Apply: func(
			requestContext context.Context,
			request webhost.SetupRequest,
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
	server, err := webhost.New(webhost.Options{
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
			OwnerKind:       webOwnerKind(options.replaceOwner),
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

	if options.open && !options.noOpen {
		if err := openWebBrowser(workspaceURL); err != nil {
			_, _ = fmt.Fprintf(stderr, "qcode: open browser: %v\n", err)
		}
	}
	if configErr != nil {
		server.FailBoot(configErr)
		_, _ = fmt.Fprintf(stderr, "qcode: config: %v\n", configErr)
		return waitForWebShutdown(ctx, httpServer, serveErr, server, nil, nil, stderr, 1)
	}

	store, err := state.Open(ctx, state.Options{
		DataDir: dataDir, BusyTimeout: loaded.Config.State.BusyTimeout,
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
			workspaceIdentity, store, repositories, stderr, secret,
			nil, credential.Reference{},
		)
		if prepareErr == nil && prepared.credentialActivate != nil {
			value := prepared.credentialReference
			candidateSelection.Credential = &value
			reference = value
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
			prepareErr = prepared.activateCredential()
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
				prepareErr = errors.Join(
					prepareErr,
					prepared.rollbackCredential(),
				)
			}
			workspaceManager.SetRoute(selection, routeReference)
			return prepareErr
		}
		if commitErr := prepared.commitCredential(); commitErr != nil {
			_, _ = fmt.Fprintf(
				stderr,
				"qcode: finalize credential rotation: %v\n",
				commitErr,
			)
		}
		workspaceManager.RegisterInitial(workspaceIdentity, prepared)
		loaded = candidate
		selection = candidateSelection
		routeReference = reference
		return nil
	}

	if setupRequired {
		_, _ = fmt.Fprintf(stdout, "QCode Setup Ready: %s\n", publicURL)
		for !workspaceManager.Configured() {
			select {
			case attempt := <-setupRequests:
				candidateSelection, reference, setupErr := resolveWebSetup(attempt.request)
				var candidate config.Snapshot
				if setupErr == nil {
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
	_, _ = fmt.Fprintf(stdout, "QCode Runtime Ready: %s\n", publicURL)
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
	if strings.TrimSpace(token) == "" {
		return "", errors.New("owner capability token is required")
	}
	requestContext, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if target.Scheme != "http" || target.Hostname() != "127.0.0.1" ||
		target.User != nil {
		return "", errors.New("owner URL is not a trusted loopback HTTP endpoint")
	}
	target.Path = "/api/v1/workspace/add"
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	body, err := json.Marshal(webhost.WorkspaceAddRequest{Path: workspaceRoot})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(
		requestContext,
		http.MethodPost,
		target.String(),
		bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-QCode-Request-ID", "workspace-register")
	digest := sha256.Sum256([]byte(workspaceRoot))
	request.Header.Set(
		"Idempotency-Key",
		"workspace-register-"+hex.EncodeToString(digest[:]),
	)
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("owner Workspace registration redirects are forbidden")
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var envelope struct {
		Result  webhost.WorkspaceAddResult `json:"result"`
		Problem *protocol.Problem          `json:"problem,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&envelope); err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		if envelope.Problem != nil {
			return "", errors.New(envelope.Problem.Message)
		}
		return "", fmt.Errorf("Workspace registration failed (HTTP %d)", response.StatusCode)
	}
	return envelope.Result.Workspace.ID, nil
}

func loadWebConfig(options webCommandOptions) (config.Snapshot, error) {
	return config.Load(config.LoadOptions{
		Path:      options.configPath,
		Overrides: webConfigOverrides(options),
	})
}

type preparedWebRuntime struct {
	*preparedWebCredentials
	application  *wire.Session
	extensions   *wire.SkillControlHandle
	dependencies webhost.Dependencies
}

type preparedWebCredentials struct {
	credentialActivate  func() error
	credentialCommit    func() error
	credentialRollback  func() error
	credentialControl   *credential.Control
	credentialReference credential.Reference
}

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
	stagedControl *credential.Control,
	stagedReference credential.Reference,
) (_ *preparedWebRuntime, resultErr error) {
	preparedCredentials, err := prepareWebCredentials(
		ctx, loaded, selection, secret, stagedControl, stagedReference,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, preparedCredentials.rollbackCredential())
		}
	}()
	credentialControl := preparedCredentials.credentialControl
	effectiveCredential := preparedCredentials.credentialReference

	runtimeOverrides := webConfigOverrides(options)
	runtimeOverrides.Provider = &loaded.Config.Execution.Provider
	runtimeOverrides.Model = &loaded.Config.Execution.Model
	runtimeOverrides.Protocol = &loaded.Config.Execution.Protocol
	runtimeOverrides.CredentialKind = &effectiveCredential.Kind
	runtimeOverrides.CredentialName = &effectiveCredential.Name
	skillOptions := wire.SkillOptions{DataDir: loaded.Config.State.DataDir}
	application, err := wire.NewExec(ctx, wire.ExecOptions{
		ConfigPath:        options.configPath,
		ConfigOverrides:   runtimeOverrides,
		BaseURL:           selection.BaseURL,
		APIKeyEnv:         options.apiKeyEnv,
		FixturePath:       options.providerFixture,
		Permission:        options.posture,
		MCPConfigPath:     options.mcpConfig,
		PersistentStore:   store,
		CredentialControl: credentialControl,
		WorkspaceIdentity: workspaceIdentity,
		Skills:            skillOptions,
		ModelMetadata:     setupModelMetadata(selection),
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
			ctx, application.ProviderID(), selection.BaseURL,
			model.CredentialRef{Kind: reference.Kind, Name: reference.Name},
			setupWireModelID(selection, selection.Model),
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
			selection.BaseURL,
			model.CredentialRef{
				Kind: status.Reference.Kind,
				Name: status.Reference.Name,
			},
			setupWireModelID(selection, modelID),
		)
	}
	connection := webhost.WorkspaceConnection{
		Provider:      selection.Provider,
		Endpoint:      selection.BaseURL,
		Protocol:      selection.Protocol,
		ModelMetadata: selection.Metadata,
	}
	if provider, ok := model.DefaultCatalog().Provider(application.ProviderID()); ok {
		if connection.Endpoint == "" {
			connection.Endpoint = provider.Endpoint
		}
		if connection.Protocol == "" {
			connection.Protocol = string(provider.Protocol)
		}
	}
	return &preparedWebRuntime{
		preparedWebCredentials: preparedCredentials,
		application:            application, extensions: extensions,
		dependencies: webhost.Dependencies{
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

func prepareWebCredentials(
	ctx context.Context,
	loaded config.Snapshot,
	selection webSetupSelection,
	secret string,
	stagedControl *credential.Control,
	stagedReference credential.Reference,
) (*preparedWebCredentials, error) {
	credentialControl := stagedControl
	effectiveCredential := stagedReference
	if credentialControl == nil {
		var err error
		credentialControl, effectiveCredential, err = credential.OpenControl(
			ctx,
			loaded.Config.State.DataDir,
			webSupervisorScope,
			selection.Provider,
			credential.Reference{
				Kind: loaded.Config.Credential.Kind,
				Name: loaded.Config.Credential.Name,
			},
			func() credential.Reference {
				if selection.Credential == nil {
					return credential.Reference{}
				}
				return *selection.Credential
			}(),
		)
		if err != nil {
			return nil, fmt.Errorf("credential recovery: %w", err)
		}
	}
	var credentialActivate, credentialCommit, credentialRollback func() error
	if secret != "" {
		previousCredential := effectiveCredential
		setupCredentials := credential.New(
			effectiveCredential,
			credential.WithControl(credentialControl),
			credential.WithLiveReload(),
		)
		status, setErr := setupCredentials.StageKeyring(ctx, secret)
		if setErr != nil {
			return nil, fmt.Errorf("store setup credential: %w", setErr)
		}
		effectiveCredential = status.Reference
		credentialActivate = func() error {
			return credentialControl.Activate(
				context.Background(),
				status.Reference,
			)
		}
		credentialCommit = func() error {
			return credentialControl.Commit(
				context.Background(),
				status.Reference,
			)
		}
		credentialRollback = func() error {
			return credentialControl.Restore(
				context.Background(),
				status.Reference,
				previousCredential,
			)
		}
	}

	return &preparedWebCredentials{
		credentialActivate:  credentialActivate,
		credentialCommit:    credentialCommit,
		credentialRollback:  credentialRollback,
		credentialControl:   credentialControl,
		credentialReference: effectiveCredential,
	}, nil
}

func (p *preparedWebCredentials) activateCredential() error {
	if p == nil || p.credentialActivate == nil {
		return nil
	}
	activate := p.credentialActivate
	p.credentialActivate = nil
	return activate()
}

func (p *preparedWebRuntime) dependenciesWithDiagnostics(
	diagnostics io.Writer,
) webhost.Dependencies {
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

func (p *preparedWebCredentials) commitCredential() error {
	if p == nil || p.credentialCommit == nil {
		return nil
	}
	commit := p.credentialCommit
	p.credentialActivate = nil
	p.credentialCommit = nil
	p.credentialRollback = nil
	return commit()
}

func (p *preparedWebCredentials) rollbackCredential() error {
	if p == nil || p.credentialRollback == nil {
		return nil
	}
	rollback := p.credentialRollback
	p.credentialActivate = nil
	p.credentialCommit = nil
	p.credentialRollback = nil
	return rollback()
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
	if options.enableTools {
		overrides.Tools = &options.enableTools
	}
	if options.dataDir != "" {
		overrides.StateDataDir = &options.dataDir
	}
	if options.provider != "" {
		overrides.Provider = &options.provider
	}
	if options.model != "" {
		overrides.Model = &options.model
	}
	return overrides
}

func waitForWebShutdown(
	ctx context.Context,
	httpServer *http.Server,
	serveErr <-chan error,
	server *webhost.Server,
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

func openWebBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

func terminalWriter(writer io.Writer) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func webFlagProvided(args []string, name string) bool {
	prefix := "--" + name
	for _, arg := range args {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}
