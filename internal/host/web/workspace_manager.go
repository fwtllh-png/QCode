package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	webhost "github.com/fwtllh-png/QCode/internal/host/runtimeapi/web"
	"github.com/fwtllh-png/QCode/internal/persist/atomicfile"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/credential"
)

const workspaceRegistryVersion = 1

type workspaceRegistryState struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
}

type workspaceRuntimeManager struct {
	mu sync.Mutex

	dataDir      string
	options      webCommandOptions
	server       *webhost.Server
	store        *state.Store
	repositories apppersistence.PersistentRepositories
	stderr       io.Writer

	selection     webSetupSelection
	reference     credential.Reference
	roots         []string
	active        map[string]*preparedWebRuntime
	problems      map[string]string
	loading       map[string]chan struct{}
	loads         sync.WaitGroup
	reconfiguring bool
	closing       bool
}

func newWorkspaceRuntimeManager(
	dataDir string,
	initialRoot string,
) (*workspaceRuntimeManager, error) {
	roots, err := loadWorkspaceRoots(dataDir)
	if err != nil {
		return nil, err
	}
	if initialRoot != "" {
		initialRoot, _, err = normalizeWorkspaceRoot(initialRoot)
		if err != nil {
			return nil, err
		}
		roots = prependUniqueRoot(roots, initialRoot)
	}
	return &workspaceRuntimeManager{
		dataDir: dataDir, roots: roots,
		active:   make(map[string]*preparedWebRuntime),
		problems: make(map[string]string),
		loading:  make(map[string]chan struct{}),
	}, nil
}

func (m *workspaceRuntimeManager) Bind(
	server *webhost.Server,
	options webCommandOptions,
	store *state.Store,
	repositories apppersistence.PersistentRepositories,
	stderr io.Writer,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.server = server
	m.options = options
	m.store = store
	m.repositories = repositories
	m.stderr = stderr
}

func (m *workspaceRuntimeManager) SetRoute(
	selection webSetupSelection,
	reference credential.Reference,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.selection = selection
	m.reference = reference
}

func (m *workspaceRuntimeManager) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.selection.Provider != ""
}

// Connection setup is Supervisor-owned and does not require a synthetic
// Workspace or a Runtime with filesystem access.
func (m *workspaceRuntimeManager) configureWithoutRuntime(
	ctx context.Context,
	selection webSetupSelection,
	reference credential.Reference,
	secret string,
	persist bool,
) (resultErr error) {
	m.mu.Lock()
	options := m.options
	previous := m.selection
	m.mu.Unlock()
	loaded, err := loadWebSetupConfig(options, selection, reference)
	if err != nil {
		return err
	}
	prepared, err := prepareWebCredentials(
		ctx, loaded, selection, secret, nil, credential.Reference{},
	)
	if err != nil {
		return err
	}
	selectionPersisted := false
	defer func() {
		if resultErr == nil {
			return
		}
		if selectionPersisted {
			if previous.Provider != "" {
				resultErr = errors.Join(resultErr, saveWebSetupSelection(m.dataDir, "", previous))
			} else if err := os.Remove(setupSelectionPath(m.dataDir, "")); !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, err)
			}
		}
		resultErr = errors.Join(resultErr, prepared.rollbackCredential())
	}()
	reference = prepared.credentialReference
	if prepared.credentialActivate != nil {
		value := reference
		selection.Credential = &value
	}
	if persist {
		if err := saveWebSetupSelection(m.dataDir, "", selection); err != nil {
			return err
		}
		selectionPersisted = true
	}
	if err := m.Persist(); err != nil {
		return err
	}
	if err := prepared.activateCredential(); err != nil {
		return err
	}
	m.SetRoute(selection, reference)
	m.server.ActivateSupervisor()
	if err := prepared.commitCredential(); err != nil {
		_, _ = fmt.Fprintf(m.stderr, "qcode: finalize credential rotation: %v\n", err)
	}
	return nil
}

func (m *workspaceRuntimeManager) Reconfigure(
	ctx context.Context,
	request webhost.SetupRequest,
) error {
	resolveRequest := request
	if strings.TrimSpace(resolveRequest.APIKey) == "" {
		resolveRequest.APIKey = "persisted"
	}
	selection, reference, err := resolveWebSetup(resolveRequest)
	if err != nil {
		return err
	}
	_, err = m.replaceSelection(ctx, selection, reference, request.APIKey, true)
	return err
}

func (m *workspaceRuntimeManager) AddModel(
	ctx context.Context,
	request webhost.ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	return m.mutateModel(ctx, request, false)
}

func (m *workspaceRuntimeManager) ProbeModel(
	ctx context.Context,
	modelID string,
) (webhost.SetupProbeResult, error) {
	modelID = strings.TrimSpace(modelID)
	if !setupModelIDPattern.MatchString(modelID) {
		return webhost.SetupProbeResult{}, invalidSetup("model id is invalid")
	}
	m.mu.Lock()
	selection := cloneWebSetupSelection(m.selection)
	reference := m.reference
	m.mu.Unlock()
	baseURL := selection.BaseURL
	providerID := selection.Provider
	if baseURL == "" {
		if provider, exists := model.DefaultCatalog().Provider(providerID); exists {
			baseURL = provider.Endpoint
		}
	}
	probed, err := wire.ProbeModelConnection(
		ctx,
		providerID,
		baseURL,
		modelID,
		"",
		model.CredentialRef{Kind: reference.Kind, Name: reference.Name},
		model.WireProtocol(selection.Protocol),
	)
	if err != nil {
		return webhost.SetupProbeResult{}, err
	}
	result := webhost.SetupProbeResult{
		Capabilities: setupCapabilitiesDTO(probed.Capabilities),
		Warning:      probed.Warning,
	}
	for _, value := range probed.Models {
		result.Models = append(result.Models, webhost.SetupDiscoveredModel{
			ID: value.ID, Name: value.Name,
			ContextTokens:   value.ContextTokens,
			MaxOutputTokens: value.MaxOutputTokens,
		})
	}
	return result, nil
}

func (m *workspaceRuntimeManager) UpdateModel(
	ctx context.Context,
	request webhost.ModelMutationRequest,
) (protocol.ModelCatalog, error) {
	return m.mutateModel(ctx, request, true)
}

func (m *workspaceRuntimeManager) RemoveModel(
	ctx context.Context,
	modelID string,
) (protocol.ModelCatalog, error) {
	modelID = strings.TrimSpace(modelID)
	m.mu.Lock()
	selection := cloneWebSetupSelection(m.selection)
	reference := m.reference
	active := maps.Clone(m.active)
	m.mu.Unlock()
	if modelID == "" {
		return protocol.ModelCatalog{}, invalidSetup("model id is required")
	}
	if modelID == selection.Model {
		return protocol.ModelCatalog{}, invalidSetup(
			"the connection default model cannot be removed",
		)
	}
	found := false
	next := selection
	next.Models = make([]webSetupModel, 0, len(selection.Models))
	for _, entry := range selection.Models {
		if entry.ID == modelID {
			found = true
			continue
		}
		next.Models = append(next.Models, entry)
	}
	if !found {
		return protocol.ModelCatalog{}, invalidSetup("model is not registered")
	}
	for _, runtime := range active {
		sessions, err := runtime.application.Runtime.ListSessions(
			ctx,
			protocol.SessionListQuery{
				WorkspaceRoot:   runtime.dependencies.WorkspaceRoot,
				IncludeArchived: true,
				Limit:           1000,
			},
		)
		if err != nil {
			return protocol.ModelCatalog{}, err
		}
		for _, session := range sessions.Sessions {
			profile, err := runtime.application.Runtime.SessionProfile(
				ctx,
				session.SessionID,
			)
			if err == nil && profile.Profile.Model == modelID {
				return protocol.ModelCatalog{}, protocol.NewProblem(
					protocol.CodeConflict,
					"model is still used by a Session",
					false,
					nil,
				)
			}
		}
	}
	return m.replaceSelection(ctx, next, reference, "", false)
}

func (m *workspaceRuntimeManager) mutateModel(
	ctx context.Context,
	request webhost.ModelMutationRequest,
	update bool,
) (protocol.ModelCatalog, error) {
	modelID := strings.TrimSpace(request.Model)
	if !setupModelIDPattern.MatchString(modelID) {
		return protocol.ModelCatalog{}, invalidSetup("model id is invalid")
	}
	m.mu.Lock()
	selection := m.selection
	reference := m.reference
	m.mu.Unlock()
	metadata, err := resolveSetupModelMetadata(
		selection.Protocol,
		&request.ModelMetadata,
	)
	if err != nil {
		return protocol.ModelCatalog{}, err
	}
	if modelID == selection.Model {
		return protocol.ModelCatalog{}, invalidSetup(
			"the connection default model is managed in Connection settings",
		)
	}
	index := -1
	for candidate := range selection.Models {
		if selection.Models[candidate].ID == modelID {
			index = candidate
			break
		}
	}
	if update && index < 0 {
		return protocol.ModelCatalog{}, invalidSetup("model is not registered")
	}
	if !update && index >= 0 {
		return protocol.ModelCatalog{}, invalidSetup("model is already registered")
	}
	entry := webSetupModel{ID: modelID, Metadata: *metadata}
	if index >= 0 {
		selection.Models[index] = entry
	} else {
		selection.Models = append(selection.Models, entry)
	}
	sort.Slice(selection.Models, func(i, j int) bool {
		return selection.Models[i].ID < selection.Models[j].ID
	})
	return m.replaceSelection(ctx, selection, reference, "", false)
}

func (m *workspaceRuntimeManager) replaceSelection(
	ctx context.Context,
	selection webSetupSelection,
	reference credential.Reference,
	secret string,
	rebindProfiles bool,
) (protocol.ModelCatalog, error) {

	m.mu.Lock()
	switch {
	case m.closing:
		m.mu.Unlock()
		return protocol.ModelCatalog{}, errors.New("Web Host is shutting down")
	case m.reconfiguring:
		m.mu.Unlock()
		return protocol.ModelCatalog{}, protocol.NewProblem(
			protocol.CodeConflict,
			"Runtime configuration is already restarting",
			true,
			nil,
		)
	case len(m.loading) != 0:
		m.mu.Unlock()
		return protocol.ModelCatalog{}, protocol.NewProblem(
			protocol.CodeConflict,
			"Workspace Runtime is still starting",
			true,
			nil,
		)
	case m.server == nil || m.store == nil:
		m.mu.Unlock()
		return protocol.ModelCatalog{}, errors.New("Workspace Runtime is unavailable")
	}
	if !m.server.BeginRuntimeReconfiguration() {
		m.mu.Unlock()
		return protocol.ModelCatalog{}, protocol.NewProblem(
			protocol.CodeConflict,
			"Runtime configuration is already restarting",
			true,
			nil,
		)
	}
	m.reconfiguring = true
	active := maps.Clone(m.active)
	previousSelection := cloneWebSetupSelection(m.selection)
	options := m.options
	store := m.store
	repositories := m.repositories
	server := m.server
	stderr := m.stderr
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.reconfiguring = false
		m.mu.Unlock()
		server.EndRuntimeReconfiguration()
	}()

	if err := idleWorkspaceRuntimes(ctx, active); err != nil {
		return protocol.ModelCatalog{}, err
	}
	if len(active) == 0 {
		err := m.configureWithoutRuntime(ctx, selection, reference, secret, true)
		return protocol.ModelCatalog{}, err
	}
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	prepared := make(map[string]*preparedWebRuntime, len(active))
	var stagedControl *credential.Control
	var stagedReference credential.Reference
	closePrepared := func() error {
		var closeErr error
		for _, runtime := range prepared {
			runtime.close()
			closeErr = errors.Join(closeErr, runtime.rollbackCredential())
		}
		return closeErr
	}
	for index, id := range ids {
		current := active[id]
		root := current.dependencies.WorkspaceRoot
		identity := current.dependencies.WorkspaceIdentity
		options.workspace = root
		candidate, loadErr := loadWebSetupConfig(options, selection, reference)
		if loadErr != nil {
			return protocol.ModelCatalog{}, errors.Join(loadErr, closePrepared())
		}
		runtimeSecret := ""
		if index == 0 {
			runtimeSecret = secret
		}
		replacement, prepareErr := prepareWebRuntime(
			ctx,
			options,
			candidate,
			selection,
			root,
			identity,
			store,
			repositories,
			stderr,
			runtimeSecret,
			stagedControl,
			stagedReference,
		)
		if prepareErr != nil {
			return protocol.ModelCatalog{}, errors.Join(prepareErr, closePrepared())
		}
		prepared[id] = replacement
		if replacement.credentialActivate != nil {
			stagedControl = replacement.credentialControl
			stagedReference = replacement.credentialReference
		}
	}
	if stagedControl != nil {
		value := stagedReference
		selection.Credential = &value
		reference = value
	}
	if err := idleWorkspaceRuntimes(ctx, active); err != nil {
		return protocol.ModelCatalog{}, errors.Join(err, closePrepared())
	}
	roots := make([]string, 0, len(ids))
	for _, id := range ids {
		roots = append(roots, active[id].dependencies.WorkspaceRoot)
	}
	if err := saveWebSetupSelection(m.dataDir, "", selection); err != nil {
		return protocol.ModelCatalog{}, errors.Join(err, closePrepared())
	}
	previousProfile := active[ids[0]].application.DefaultProfile()
	nextProfile := prepared[ids[0]].application.DefaultProfile()
	if rebindProfiles {
		if err := repositories.Sessions.RebindWorkspaceProfiles(
			ctx,
			roots,
			nextProfile,
		); err != nil {
			_ = saveWebSetupSelection(m.dataDir, "", previousSelection)
			return protocol.ModelCatalog{}, errors.Join(err, closePrepared())
		}
	}
	for _, runtime := range prepared {
		if err := runtime.activateCredential(); err != nil {
			var rollbackErr error
			if rebindProfiles {
				rollbackErr = repositories.Sessions.RebindWorkspaceProfiles(
					ctx,
					roots,
					previousProfile,
				)
			}
			rollbackErr = errors.Join(
				rollbackErr,
				saveWebSetupSelection(m.dataDir, "", previousSelection),
			)
			return protocol.ModelCatalog{}, errors.Join(err, rollbackErr, closePrepared())
		}
	}
	replaced := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := server.AddWorkspace(
			prepared[id].dependenciesWithDiagnostics(stderr),
		); err != nil {
			for _, replacedID := range replaced {
				_ = server.AddWorkspace(
					active[replacedID].dependenciesWithDiagnostics(stderr),
				)
			}
			var rollbackErr error
			if rebindProfiles {
				rollbackErr = repositories.Sessions.RebindWorkspaceProfiles(
					ctx,
					roots,
					previousProfile,
				)
			}
			rollbackErr = errors.Join(
				rollbackErr,
				saveWebSetupSelection(m.dataDir, "", previousSelection),
			)
			return protocol.ModelCatalog{}, errors.Join(err, rollbackErr, closePrepared())
		}
		replaced = append(replaced, id)
	}

	m.mu.Lock()
	m.selection = selection
	m.reference = reference
	m.active = prepared
	m.mu.Unlock()
	for _, runtime := range prepared {
		if commitErr := runtime.commitCredential(); commitErr != nil {
			_, _ = fmt.Fprintf(
				stderr,
				"qcode: finalize credential rotation: %v\n",
				commitErr,
			)
		}
	}
	for _, runtime := range active {
		runtime.close()
	}
	return prepared[ids[0]].application.ModelCatalog(), nil
}

func idleWorkspaceRuntimes(
	ctx context.Context,
	runtimes map[string]*preparedWebRuntime,
) error {
	for _, runtime := range runtimes {
		activity := runtime.application.Runtime.Snapshot(ctx)
		if activity.ActiveTurns != 0 ||
			activity.ActiveProviderCalls != 0 ||
			activity.ActiveToolExecutions != 0 ||
			activity.PendingApprovals != 0 ||
			activity.PendingInputs != 0 ||
			activity.PendingOperations != 0 {
			return protocol.NewProblem(
				protocol.CodeConflict,
				"Finish active and pending work before changing Provider",
				true,
				nil,
			)
		}
	}
	return nil
}

func (m *workspaceRuntimeManager) RegisterInitial(
	identity protocol.WorkspaceIdentity,
	runtime *preparedWebRuntime,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[identity.RootID] = runtime
	delete(m.problems, identity.RootID)
	m.roots = prependUniqueRoot(m.roots, identity.RuntimePath)
}

func (m *workspaceRuntimeManager) Persist() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return saveWorkspaceRoots(m.dataDir, m.roots)
}

func (m *workspaceRuntimeManager) ActivateRegistered(ctx context.Context) {
	m.mu.Lock()
	roots := append([]string(nil), m.roots...)
	m.mu.Unlock()
	for _, root := range roots {
		_, _ = m.Add(ctx, root)
	}
}

func (m *workspaceRuntimeManager) List(
	ctx context.Context,
) (webhost.WorkspaceCatalog, error) {
	m.mu.Lock()
	roots := append([]string(nil), m.roots...)
	active := maps.Clone(m.active)
	problems := maps.Clone(m.problems)
	m.mu.Unlock()

	result := webhost.WorkspaceCatalog{
		Version:    workspaceRegistryVersion,
		Workspaces: make([]webhost.WorkspaceDescriptor, 0, len(roots)),
	}
	for _, root := range roots {
		_, identity, err := normalizeWorkspaceRoot(root)
		if err != nil {
			storedIdentity, identityErr := workspaceIdentityForStoredRoot(root)
			descriptor := webhost.WorkspaceDescriptor{
				Root: root, Label: filepath.Base(root), Problem: err.Error(),
			}
			if identityErr == nil {
				descriptor.ID = storedIdentity.RootID
				descriptor.Removable = true
			}
			result.Workspaces = append(result.Workspaces, descriptor)
			continue
		}
		descriptor := webhost.WorkspaceDescriptor{
			ID: identity.RootID, Root: identity.RuntimePath,
			Label:     filepath.Base(identity.RuntimePath),
			Removable: true,
			Problem:   problems[identity.RootID],
		}
		if runtime := active[identity.RootID]; runtime != nil {
			descriptor.Ready = true
			if workspace := runtime.application.WorkspaceQuery(); workspace != nil {
				git, gitErr := workspace.GitState(ctx)
				if gitErr == nil {
					descriptor.Git = &git
				}
			}
			page, listErr := runtime.application.Runtime.ListSessions(
				ctx,
				protocol.SessionListQuery{
					WorkspaceRoot: identity.RuntimePath,
					Limit:         1000,
				},
			)
			if listErr == nil {
				descriptor.SessionCount = len(page.Sessions)
			}
		}
		result.Workspaces = append(result.Workspaces, descriptor)
	}
	return result, nil
}

func (m *workspaceRuntimeManager) Add(
	ctx context.Context,
	path string,
) (webhost.WorkspaceDescriptor, error) {
	root, identity, err := normalizeWorkspaceRoot(path)
	if err != nil {
		return webhost.WorkspaceDescriptor{}, invalidSetup(err.Error())
	}
	if _, err := wire.ValidateExternalStateDirectory(root, m.dataDir); err != nil {
		return webhost.WorkspaceDescriptor{}, invalidSetup(err.Error())
	}
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return webhost.WorkspaceDescriptor{}, errors.New("Web Host is shutting down")
		}
		if m.reconfiguring {
			m.mu.Unlock()
			return webhost.WorkspaceDescriptor{}, protocol.NewProblem(
				protocol.CodeConflict,
				"Provider connection is restarting",
				true,
				nil,
			)
		}
		if runtime := m.active[identity.RootID]; runtime != nil {
			m.mu.Unlock()
			return m.descriptor(ctx, identity, runtime), nil
		}
		if loading := m.loading[identity.RootID]; loading != nil {
			m.mu.Unlock()
			select {
			case <-loading:
				continue
			case <-ctx.Done():
				return webhost.WorkspaceDescriptor{}, ctx.Err()
			}
		}
		m.roots = appendUniqueRoot(m.roots, root)
		if m.store == nil || m.server == nil || m.selection.Provider == "" {
			saveErr := saveWorkspaceRoots(m.dataDir, m.roots)
			m.mu.Unlock()
			if saveErr != nil {
				return webhost.WorkspaceDescriptor{}, saveErr
			}
			return webhost.WorkspaceDescriptor{
				ID: identity.RootID, Root: root, Label: filepath.Base(root),
				Removable: true,
			}, nil
		}
		loading := make(chan struct{})
		m.loading[identity.RootID] = loading
		m.loads.Add(1)
		options := m.options
		selection := m.selection
		reference := m.reference
		store := m.store
		repositories := m.repositories
		server := m.server
		stderr := m.stderr
		m.mu.Unlock()

		options.workspace = root
		loaded, loadErr := loadWebSetupConfig(options, selection, reference)
		var prepared *preparedWebRuntime
		if loadErr == nil {
			prepared, loadErr = prepareWebRuntime(
				ctx, options, loaded, selection, root, identity,
				store, repositories, stderr, "",
				nil, credential.Reference{},
			)
		}

		m.mu.Lock()
		if loadErr == nil && m.closing {
			loadErr = errors.New("Web Host is shutting down")
		}
		if loadErr == nil {
			loadErr = saveWorkspaceRoots(m.dataDir, m.roots)
		}
		if loadErr == nil {
			loadErr = server.AddWorkspace(
				prepared.dependenciesWithDiagnostics(stderr),
			)
		}
		if loadErr != nil {
			m.problems[identity.RootID] = loadErr.Error()
		} else {
			m.active[identity.RootID] = prepared
			delete(m.problems, identity.RootID)
		}
		delete(m.loading, identity.RootID)
		close(loading)
		m.loads.Done()
		m.mu.Unlock()

		if loadErr != nil {
			if prepared != nil {
				prepared.close()
			}
			return webhost.WorkspaceDescriptor{}, loadErr
		}
		return m.descriptor(ctx, identity, prepared), nil
	}
}

func (m *workspaceRuntimeManager) Remove(
	ctx context.Context,
	workspaceID string,
) (webhost.WorkspaceCatalog, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return webhost.WorkspaceCatalog{}, invalidSetup("Workspace ID is required")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return webhost.WorkspaceCatalog{}, errors.New("Web Host is shutting down")
	}
	if m.reconfiguring {
		m.mu.Unlock()
		return webhost.WorkspaceCatalog{}, protocol.NewProblem(
			protocol.CodeConflict,
			"Provider connection is restarting",
			true,
			nil,
		)
	}
	if m.loading[workspaceID] != nil {
		m.mu.Unlock()
		return webhost.WorkspaceCatalog{}, protocol.NewProblem(
			protocol.CodeConflict,
			"Workspace Runtime is still starting",
			true,
			nil,
		)
	}
	rootIndex := -1
	for index, root := range m.roots {
		identity, identityErr := workspaceIdentityForStoredRoot(root)
		if identityErr == nil && identity.RootID == workspaceID {
			rootIndex = index
			break
		}
	}
	if rootIndex < 0 {
		m.mu.Unlock()
		return m.List(ctx)
	}
	previousRoots := append([]string(nil), m.roots...)
	nextRoots := append([]string(nil), m.roots[:rootIndex]...)
	nextRoots = append(nextRoots, m.roots[rootIndex+1:]...)
	runtime := m.active[workspaceID]
	if runtime != nil && runtime.application != nil &&
		runtime.application.Runtime != nil {
		activity := runtime.application.Runtime.Snapshot(ctx)
		if activity.ActiveTurns != 0 ||
			activity.ActiveProviderCalls != 0 ||
			activity.ActiveToolExecutions != 0 ||
			activity.PendingApprovals != 0 ||
			activity.PendingInputs != 0 ||
			activity.PendingOperations != 0 {
			m.mu.Unlock()
			return webhost.WorkspaceCatalog{}, protocol.NewProblem(
				protocol.CodeConflict,
				"Workspace has active or pending work",
				true,
				nil,
			)
		}
	}
	if err := saveWorkspaceRoots(m.dataDir, nextRoots); err != nil {
		m.mu.Unlock()
		return webhost.WorkspaceCatalog{}, err
	}
	if runtime != nil && m.server != nil {
		if err := m.server.RemoveWorkspace(workspaceID); err != nil {
			rollbackErr := saveWorkspaceRoots(m.dataDir, previousRoots)
			m.mu.Unlock()
			return webhost.WorkspaceCatalog{}, errors.Join(err, rollbackErr)
		}
	}
	m.roots = nextRoots
	delete(m.active, workspaceID)
	delete(m.problems, workspaceID)
	m.mu.Unlock()

	if runtime != nil {
		runtime.close()
	}
	return m.List(ctx)
}

func (m *workspaceRuntimeManager) descriptor(
	ctx context.Context,
	identity protocol.WorkspaceIdentity,
	runtime *preparedWebRuntime,
) webhost.WorkspaceDescriptor {
	descriptor := webhost.WorkspaceDescriptor{
		ID: identity.RootID, Root: identity.RuntimePath,
		Label: filepath.Base(identity.RuntimePath), Ready: true,
		Removable: true,
	}
	if workspace := runtime.application.WorkspaceQuery(); workspace != nil {
		if git, err := workspace.GitState(ctx); err == nil {
			descriptor.Git = &git
		}
	}
	page, err := runtime.application.Runtime.ListSessions(
		ctx,
		protocol.SessionListQuery{WorkspaceRoot: identity.RuntimePath, Limit: 1000},
	)
	if err == nil {
		descriptor.SessionCount = len(page.Sessions)
	}
	return descriptor
}

func (m *workspaceRuntimeManager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
	m.loads.Wait()
	m.mu.Lock()
	runtimes := make([]*preparedWebRuntime, 0, len(m.active))
	for _, runtime := range m.active {
		runtimes = append(runtimes, runtime)
	}
	m.active = make(map[string]*preparedWebRuntime)
	m.mu.Unlock()
	var closeErr error
	for _, runtime := range runtimes {
		if runtime.extensions != nil {
			closeErr = errors.Join(closeErr, runtime.extensions.Close())
		}
		if runtime.application != nil {
			closeErr = errors.Join(closeErr, runtime.application.Close(ctx))
		}
	}
	return closeErr
}

func workspaceRegistryPath(dataDir string) string {
	return filepath.Join(dataDir, "web-workspaces.json")
}

func loadWorkspaceRoots(dataDir string) ([]string, error) {
	data, err := os.ReadFile(workspaceRegistryPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Workspace registry: %w", err)
	}
	var state workspaceRegistryState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, fmt.Errorf("decode Workspace registry: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("Workspace registry has trailing data")
	}
	if state.Version != workspaceRegistryVersion {
		return nil, fmt.Errorf("unsupported Workspace registry version %d", state.Version)
	}
	return state.Roots, nil
}

func saveWorkspaceRoots(dataDir string, roots []string) error {
	data, err := json.Marshal(workspaceRegistryState{
		Version: workspaceRegistryVersion,
		Roots:   roots,
	})
	if err != nil {
		return err
	}
	return atomicfile.Replace(
		workspaceRegistryPath(dataDir),
		append(data, '\n'),
		0o600,
	)
}

func normalizeWorkspaceRoot(
	value string,
) (string, protocol.WorkspaceIdentity, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", protocol.WorkspaceIdentity{}, errors.New("Workspace path is required")
	}
	root, err := filepath.Abs(value)
	if err != nil {
		return "", protocol.WorkspaceIdentity{}, fmt.Errorf("resolve Workspace path: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", protocol.WorkspaceIdentity{}, fmt.Errorf("resolve Workspace links: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", protocol.WorkspaceIdentity{}, fmt.Errorf("inspect Workspace: %w", err)
	}
	if !info.IsDir() {
		return "", protocol.WorkspaceIdentity{}, errors.New("Workspace path is not a directory")
	}
	identity, err := workspaceIdentityForStoredRoot(root)
	if err != nil {
		return "", protocol.WorkspaceIdentity{}, err
	}
	return root, identity, nil
}

func workspaceIdentityForStoredRoot(root string) (protocol.WorkspaceIdentity, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." || !filepath.IsAbs(root) {
		return protocol.WorkspaceIdentity{}, errors.New("Workspace root is invalid")
	}
	return protocol.NewWorkspaceIdentity(
		(&url.URL{Scheme: "file", Path: root}).String(),
		root,
		"",
	)
}

func prependUniqueRoot(roots []string, root string) []string {
	result := []string{root}
	for _, candidate := range roots {
		if candidate != root {
			result = append(result, candidate)
		}
	}
	return result
}

func appendUniqueRoot(roots []string, root string) []string {
	for _, candidate := range roots {
		if candidate == root {
			return roots
		}
	}
	return append(roots, root)
}
