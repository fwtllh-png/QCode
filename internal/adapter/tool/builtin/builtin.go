package builtin

import (
	"errors"
	"time"

	language "github.com/fwtllh-png/QCode/internal/adapter/lsp"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	completiontool "github.com/fwtllh-png/QCode/internal/adapter/tool/completion"
	contenttool "github.com/fwtllh-png/QCode/internal/adapter/tool/content"
	devtool "github.com/fwtllh-png/QCode/internal/adapter/tool/dev"
	filetool "github.com/fwtllh-png/QCode/internal/adapter/tool/file"
	gittool "github.com/fwtllh-png/QCode/internal/adapter/tool/git"
	handletool "github.com/fwtllh-png/QCode/internal/adapter/tool/handle"
	lsptool "github.com/fwtllh-png/QCode/internal/adapter/tool/lsp"
	repohosttool "github.com/fwtllh-png/QCode/internal/adapter/tool/repohost"
	searchtool "github.com/fwtllh-png/QCode/internal/adapter/tool/search"
	shelltool "github.com/fwtllh-png/QCode/internal/adapter/tool/shell"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/toolsearch"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/persist/contentstore"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/platform/symbols"
	"github.com/fwtllh-png/QCode/internal/security/authority"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/security/workspacebroker"
)

func NewWithDependencies(
	root string,
	backend sandbox.Backend,
	store contentstore.Store,
	manager *process.SessionManager,
	webOpts ...webtool.Options,
) (*tool.Registry, *handletool.Store, error) {
	return NewWithIndex(root, backend, store, manager, nil, webOpts...)
}

// NewWithIndex is NewWithDependencies with a repository index for the symbol
// tools. A nil index registers them as unavailable rather than hiding them, so
// the model learns why symbol lookup is not an option in this session.
func NewWithIndex(
	root string,
	backend sandbox.Backend,
	store contentstore.Store,
	manager *process.SessionManager,
	index *repoindex.Index,
	webOpts ...webtool.Options,
) (*tool.Registry, *handletool.Store, error) {
	return NewWithIndexAndRuntime(
		root, backend, store, manager, index, nil, nil, webOpts...,
	)
}

func NewWithIndexAndRuntime(
	root string,
	backend sandbox.Backend,
	store contentstore.Store,
	manager *process.SessionManager,
	index *repoindex.Index,
	semantic symbols.Provider,
	workspaceRuntime *workspacebroker.Runtime,
	webOpts ...webtool.Options,
) (*tool.Registry, *handletool.Store, error) {
	if backend == nil {
		return nil, nil, errors.New("builtin tools require an injected sandbox backend")
	}
	if store == nil {
		store = contentstore.NewMemory(contentstore.Options{})
	}
	if manager == nil {
		return nil, nil, errors.New("builtin tools require an injected process manager")
	}
	var err error
	backend, err = sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return nil, nil, err
	}
	registry := tool.NewRegistry(nil, tool.NewResultStoreWithStore(32<<10, store))
	registry.SetSandboxBackend(backend)
	handles := handletool.NewStore()
	if err := handletool.Register(registry, handles); err != nil {
		return nil, nil, err
	}
	if err := completiontool.Register(registry); err != nil {
		return nil, nil, err
	}
	if err := tool.RegisterImageReopen(registry); err != nil {
		return nil, nil, err
	}
	files, err := filetool.NewWithBackend(root, backend)
	if err != nil {
		return nil, nil, err
	}
	if err := files.Register(registry); err != nil {
		return nil, nil, err
	}
	options := webtool.OptionsFromEnv()
	if len(webOpts) > 0 {
		merged := webOpts[0]
		if merged.SearchBackend == "" {
			merged.SearchBackend = options.SearchBackend
		}
		if merged.SearchURL == "" {
			merged.SearchURL = options.SearchURL
		}
		if merged.PrimaryURL == "" {
			merged.PrimaryURL = options.PrimaryURL
		}
		if merged.FallbackURL == "" {
			merged.FallbackURL = options.FallbackURL
		}
		if merged.TavilyURL == "" {
			merged.TavilyURL = options.TavilyURL
		}
		if merged.TavilyAPIKey == "" {
			merged.TavilyAPIKey = options.TavilyAPIKey
		}
		if merged.SearXNGURL == "" {
			merged.SearXNGURL = options.SearXNGURL
		}
		if merged.BochaURL == "" {
			merged.BochaURL = options.BochaURL
		}
		if merged.BochaAPIKey == "" {
			merged.BochaAPIKey = options.BochaAPIKey
		}
		if merged.Browser == nil {
			merged.Browser = options.Browser
		}
		if merged.HTTP == nil {
			merged.HTTP = options.HTTP
		}
		options = merged
	}
	if err := webtool.RegisterWithOptions(registry, options); err != nil {
		return nil, nil, err
	}
	semanticProvider := semantic
	if semanticProvider == nil {
		// Without a resident pool the session keeps the one-shot checker it
		// always had: one server per query, no state between them.
		semanticProvider = language.Checker{Root: root, Sandbox: backend}
	}
	if err := searchtool.RegisterWithProviders(
		registry, root, backend, index, semanticProvider,
	); err != nil {
		return nil, nil, err
	}
	if err := gittool.RegisterWithBackendAndRuntime(
		registry, root, backend, workspaceRuntime,
	); err != nil {
		return nil, nil, err
	}
	if err := lsptool.RegisterWithBackend(registry, root, backend); err != nil {
		return nil, nil, err
	}
	if err := contenttool.RegisterWithBackendAndRuntime(
		registry, root, backend, workspaceRuntime,
	); err != nil {
		return nil, nil, err
	}
	if err := devtool.Register(registry, root, backend); err != nil {
		return nil, nil, err
	}
	if err := repohosttool.Register(registry, root, backend); err != nil {
		return nil, nil, err
	}
	if err := shelltool.RegisterWithManagerAndBackend(
		registry, root, manager, backend,
	); err != nil {
		return nil, nil, err
	}
	if err := toolsearch.Register(registry); err != nil {
		return nil, nil, err
	}
	return registry, handles, nil
}

func NewWithAuthority(
	root string,
	backend sandbox.Backend,
	store contentstore.Store,
	manager *process.SessionManager,
	index *repoindex.Index,
	semantic symbols.Provider,
	leaseAuthority *authority.LeaseAuthority,
	leaseTTL time.Duration,
	webOpts ...webtool.Options,
) (*tool.Registry, *handletool.Store, error) {
	runtime, err := NewWorkspaceBroker(
		root, leaseAuthority, leaseTTL,
	)
	if err != nil {
		return nil, nil, err
	}
	return NewWithIndexAndRuntime(
		root, backend, store, manager, index, semantic, runtime, webOpts...,
	)
}
