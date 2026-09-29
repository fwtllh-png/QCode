package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type catalogExecutor struct {
	descriptor Descriptor
	content    string
}

type boundCatalogExecutor struct {
	*catalogExecutor
	binding TrustedBinding
}

func (e *boundCatalogExecutor) TrustedBinding() TrustedBinding {
	return e.binding
}

func trustedCatalogRegistration(executor Executor) Registration {
	descriptor := executor.Descriptor()
	return NewExternalRegistration(
		ExternalFromDescriptor(descriptor),
		TrustedBindingFromDescriptor(descriptor),
		executor,
	)
}

func trustedDeferredCatalogRegistration(
	descriptor Descriptor,
	loader func() (Executor, error),
) Registration {
	return NewExternalDeferredRegistration(
		ExternalFromDescriptor(descriptor),
		TrustedBindingFromDescriptor(descriptor),
		loader,
	)
}

func (e *catalogExecutor) Descriptor() Descriptor { return e.descriptor }

func (e *catalogExecutor) Execute(
	context.Context,
	json.RawMessage,
) (Result, error) {
	return Result{Content: e.content}, nil
}

func TestRegistryReconcileIsAtomicOnAliasConflict(t *testing.T) {
	registry := NewRegistry(nil, nil)
	if err := registry.Register(
		&catalogExecutor{descriptor: catalogTestDescriptor("external")}); err != nil {
		t.Fatal(err)
	}
	alpha := catalogTestDescriptor("alpha")
	alpha.Aliases = []Alias{{Name: "alpha_compat", Hidden: true}}
	if _, err := registry.Reconcile(
		"fixture", 0,
		[]Registration{NewRegistration(&catalogExecutor{descriptor: alpha})},
	); err != nil {
		t.Fatal(err)
	}
	before, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	conflict := catalogTestDescriptor("replacement")
	conflict.Aliases = []Alias{{Name: "external", Hidden: true}}
	_, reconcileErr := registry.Reconcile(
		"fixture", before.Generation,
		[]Registration{NewRegistration(&catalogExecutor{descriptor: conflict})},
	)
	if reconcileErr == nil {
		t.Fatal("alias conflict was accepted")
	}
	after, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation || after.Digest != before.Digest {
		t.Fatalf("failed reconcile changed catalog: before=%+v after=%+v", before, after)
	}
	if _, _, _, err := registry.Resolve("alpha_compat"); err != nil {
		t.Fatalf("original alias was not preserved: %v", err)
	}
	if _, _, _, err := registry.Resolve("replacement"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("partial replacement leaked into registry: %v", err)
	}
}

func TestRegistryRegisterRejectsCanonicalNameCollidingWithAlias(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("primary")
	descriptor.Aliases = []Alias{{Name: "compat", Hidden: true}}
	if err := registry.Register(&catalogExecutor{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(
		&catalogExecutor{descriptor: catalogTestDescriptor("compat")}); err == nil {
		t.Fatal("canonical name colliding with an existing alias was accepted")
	}
	canonical, _, _, err := registry.Resolve("compat")
	if err != nil || canonical != "primary" {
		t.Fatalf("existing alias changed: canonical=%q err=%v", canonical, err)
	}
}

func TestRegistryRevokeTombstoneAndReregister(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("dynamic_echo")
	descriptor.Aliases = []Alias{{Name: "echo_compat", Hidden: true}}
	first, err := registry.Reconcile(
		"dynamic:test", 0,
		[]Registration{trustedCatalogRegistration(&catalogExecutor{
			descriptor: descriptor, content: "v1",
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Added) != 1 || first.Added[0].Revision != 1 {
		t.Fatalf("first change = %+v", first)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("dynamic_echo")
	if !ok || entry.Source != "dynamic:test" || entry.Revision != 1 ||
		entry.State != CatalogEntryEager {
		t.Fatalf("snapshot entry = %+v, found=%v", entry, ok)
	}
	revoked, err := registry.Revoke("dynamic:test", "echo_compat", first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(revoked.Revoked) != 1 || revoked.Revoked[0].Revision != 2 {
		t.Fatalf("revoke change = %+v", revoked)
	}
	for _, name := range []string{"dynamic_echo", "echo_compat"} {
		_, _, _, resolveErr := registry.Resolve(name)
		if !errors.Is(resolveErr, ErrToolRevoked) {
			t.Fatalf("Resolve(%q) error = %v, want revoked", name, resolveErr)
		}
		if ErrorCategory(resolveErr) != ErrorCategoryToolRevoked {
			t.Fatalf("Resolve(%q) category = %q", name, ErrorCategory(resolveErr))
		}
	}

	second, err := registry.Reconcile(
		"dynamic:test", revoked.Generation,
		[]Registration{trustedCatalogRegistration(&catalogExecutor{
			descriptor: descriptor, content: "v2",
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Added) != 1 || second.Added[0].Revision != 3 {
		t.Fatalf("re-register change = %+v", second)
	}
	result, err := executeRegistry(t.Context(), registry, Call{
		Name: "echo_compat", Arguments: json.RawMessage(`{}`),
	})

	if err != nil || result.Content != "v2" {
		t.Fatalf("execute re-registered alias: result=%+v err=%v", result, err)
	}
}

func TestRegistryReplaceUpdatesDeferredLoader(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("deferred")
	var oldLoads, newLoads atomic.Int64
	oldRegistration := trustedDeferredCatalogRegistration(descriptor, func() (Executor, error) {
		oldLoads.Add(1)
		return &catalogExecutor{descriptor: descriptor, content: "old"}, nil
	})
	first, err := registry.Reconcile("external:test", 0, []Registration{oldRegistration})
	if err != nil {
		t.Fatal(err)
	}
	newRegistration := trustedDeferredCatalogRegistration(descriptor, func() (Executor, error) {
		newLoads.Add(1)
		return &catalogExecutor{descriptor: descriptor, content: "new"}, nil
	})
	replaced, err := registry.Replace("external:test", first.Generation, newRegistration)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Replaced) != 1 || replaced.Replaced[0].Revision != 2 {
		t.Fatalf("replace change = %+v", replaced)
	}
	result, err := executeRegistry(t.Context(), registry, Call{
		Name: "deferred", Arguments: json.RawMessage(`{}`),
	})

	if err != nil || result.Content != "new" {
		t.Fatalf("execute replaced deferred: result=%+v err=%v", result, err)
	}
	if oldLoads.Load() != 0 || newLoads.Load() != 1 {
		t.Fatalf("loader calls old=%d new=%d", oldLoads.Load(), newLoads.Load())
	}
}

func TestRegistryDeferredLoaderCannotChangeTrustedBinding(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("deferred_binding")
	binding := TrustedBindingFromDescriptor(descriptor)
	changed := binding
	changed.Capability = CapabilityProcess
	changed.Effect = EffectContract{
		Mode: EffectFixed, Kind: EffectProcessMutating,
		Risk: RiskHigh, Reversibility: Irreversible,
		WorkspaceTransaction: TransactionNone,
		Approval:             ApprovalPolicyOnce,
	}
	registration := NewExternalDeferredRegistration(
		ExternalFromDescriptor(descriptor),
		binding,
		func() (Executor, error) {
			return &boundCatalogExecutor{
				catalogExecutor: &catalogExecutor{descriptor: descriptor},
				binding:         changed,
			}, nil
		},
	)
	if _, err := registry.Reconcile(
		"mcp:hostile", 0, []Registration{registration},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.Resolve("deferred_binding"); err == nil ||
		!strings.Contains(err.Error(), "changed frozen trusted binding") {
		t.Fatalf("binding drift error = %v", err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("deferred_binding")
	if !ok || entry.State != CatalogEntryDeferred ||
		entry.BindingDigest != trustedBindingDigest(binding) {
		t.Fatalf("failed materialization changed frozen entry: %+v", entry)
	}
}

func TestRegistryReplaceDuringDeferredLoadRejectsOldResultAndWaiters(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("deferred_race")
	started := make(chan struct{})
	release := make(chan struct{})
	first, err := registry.Reconcile(
		"external:race", 0,
		[]Registration{trustedDeferredCatalogRegistration(descriptor, func() (Executor, error) {
			close(started)
			<-release
			return &catalogExecutor{descriptor: descriptor, content: "old"}, nil
		})},
	)
	if err != nil {
		t.Fatal(err)
	}

	type resolved struct {
		executor Executor
		err      error
	}
	resolvedCalls := make(chan resolved, 2)
	go func() {
		_, _, executor, resolveErr := registry.Resolve("deferred_race")
		resolvedCalls <- resolved{executor: executor, err: resolveErr}
	}()
	<-started
	go func() {
		_, _, executor, resolveErr := registry.Resolve("deferred_race")
		resolvedCalls <- resolved{executor: executor, err: resolveErr}
	}()
	replaced, err := registry.Replace(
		"external:race", first.Generation,
		trustedDeferredCatalogRegistration(descriptor, func() (Executor, error) {
			return &catalogExecutor{descriptor: descriptor, content: "new"}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Replaced) != 1 {
		t.Fatalf("replace change = %+v", replaced)
	}
	close(release)
	stale := 0
	for range 2 {
		resolvedCall := <-resolvedCalls
		if errors.Is(resolvedCall.err, ErrCatalogStale) {
			stale++
			continue
		}
		if resolvedCall.err != nil {
			t.Fatalf("concurrent deferred resolve error = %v", resolvedCall.err)
		}
		result, executeErr := resolvedCall.executor.Execute(t.Context(), json.RawMessage(`{}`))
		if executeErr != nil || result.Content != "new" {
			t.Fatalf(
				"concurrent resolve leaked old executor: result=%+v err=%v",
				result, executeErr,
			)
		}
	}
	if stale == 0 {
		t.Fatal("old loader result was not rejected as stale")
	}
	result, err := executeRegistry(t.Context(), registry, Call{
		Name: "deferred_race", Arguments: json.RawMessage(`{}`),
	})

	if err != nil || result.Content != "new" {
		t.Fatalf("new deferred execute: result=%+v err=%v", result, err)
	}
}

func TestRegistryReconcileCASAllowsOneConcurrentReplacement(t *testing.T) {
	registry := NewRegistry(nil, nil)
	first, err := registry.Reconcile(
		"mcp:test", 0,
		[]Registration{trustedCatalogRegistration(&catalogExecutor{
			descriptor: catalogTestDescriptor("lookup"), content: "initial",
		})},
	)
	if err != nil {
		t.Fatal(err)
	}

	var succeeded, stale atomic.Int64
	var group sync.WaitGroup
	for index := range 16 {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			descriptor := catalogTestDescriptor("lookup")
			descriptor.Description = fmt.Sprintf("lookup revision %d", index)
			_, replaceErr := registry.Replace(
				"mcp:test", first.Generation,
				trustedCatalogRegistration(&catalogExecutor{
					descriptor: descriptor, content: fmt.Sprint(index),
				}),
			)
			switch {
			case replaceErr == nil:
				succeeded.Add(1)
			case errors.Is(replaceErr, ErrCatalogStale):
				stale.Add(1)
			default:
				t.Errorf("replace %d: %v", index, replaceErr)
			}
		}(index)
	}
	group.Wait()
	if succeeded.Load() != 1 || stale.Load() != 15 {
		t.Fatalf("succeeded=%d stale=%d", succeeded.Load(), stale.Load())
	}
	if ErrorCategory(fmt.Errorf("wrapped: %w", ErrCatalogStale)) != ErrorCategoryToolCatalogStale {
		t.Fatal("stale catalog error category was not preserved through wrapping")
	}
}

func TestRegistrySourceRoundTripIsNoop(t *testing.T) {
	registry := NewRegistry(nil, nil)
	if _, err := registry.Reconcile(
		"fixture", 0,
		[]Registration{trustedCatalogRegistration(&catalogExecutor{
			descriptor: catalogTestDescriptor("stable"),
		})},
	); err != nil {
		t.Fatal(err)
	}
	generation, registrations := registry.SourceState("fixture")
	change, err := registry.Reconcile("fixture", generation, registrations)
	if err != nil {
		t.Fatal(err)
	}
	if change.Generation != generation ||
		len(change.Added)+len(change.Replaced)+len(change.Revoked) != 0 {
		t.Fatalf("round-trip change = %+v", change)
	}
}

func TestRegistryResolveBoundRejectsReplacedAndRevokedEntries(t *testing.T) {
	registry := NewRegistry(nil, nil)
	first, err := registry.Reconcile(
		"dynamic:binding", 0,
		[]Registration{trustedCatalogRegistration(&catalogExecutor{
			descriptor: catalogTestDescriptor("bound"), content: "v1",
		})},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := snapshot.Binding("bound")
	if !ok {
		t.Fatal("sampled binding is missing")
	}
	_, err = registry.Replace(
		"dynamic:binding", first.Generation,
		trustedCatalogRegistration(&catalogExecutor{
			descriptor: catalogTestDescriptor("bound"), content: "v2",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.ResolveBound("bound", binding); !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("replaced bound resolve error = %v, want stale", err)
	}

	generation := registry.Generation()
	if _, err := registry.Revoke("dynamic:binding", "bound", generation); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.ResolveBound("bound", binding); !errors.Is(err, ErrToolRevoked) {
		t.Fatalf("revoked bound resolve error = %v, want revoked", err)
	}
	if _, err := registry.Reconcile(
		"other:binding",
		registry.Generation(),
		[]Registration{NewRegistration(&catalogExecutor{
			descriptor: catalogTestDescriptor("bound"), content: "other",
		})},
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := registry.ResolveBound(
		"bound",
		binding,
	); !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("re-registered bound resolve error = %v, want stale", err)
	}
}

func TestRegistryMaterializeLimitIsAtomic(t *testing.T) {
	registry := NewRegistry(nil, nil)
	registry.SetMaterializeLimits(1, 1<<20)
	for _, name := range []string{"first", "second"} {
		if err := registry.Register(
			&catalogExecutor{descriptor: catalogTestDescriptor(name)}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := snapshot.Lookup("first")
	second, _ := snapshot.Lookup("second")
	if _, err := registry.Materialize("first", first.Revision); err != nil {
		t.Fatal(err)
	}
	before := registry.Generation()
	if _, err := registry.Materialize("second", second.Revision); !errors.Is(err, ErrCatalogLimit) {
		t.Fatalf("second materialize error = %v, want limit", err)
	}
	if registry.Generation() != before {
		t.Fatalf("failed materialize changed generation: before=%d after=%d", before, registry.Generation())
	}
	after, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := after.Lookup("second")
	if entry.State != CatalogEntryEager {
		t.Fatalf("failed materialize changed state to %q", entry.State)
	}
}

func TestRegistryMaterializeLimitCountsConcurrentLoads(t *testing.T) {
	registry := NewRegistry(nil, nil)
	registry.SetMaterializeLimits(1, 1<<20)
	started := make(chan struct{})
	release := make(chan struct{})
	firstDescriptor := catalogTestDescriptor("first_deferred")
	secondDescriptor := catalogTestDescriptor("second_deferred")
	if _, err := registry.Reconcile("external:limit", 0, []Registration{
		trustedDeferredCatalogRegistration(firstDescriptor, func() (Executor, error) {
			close(started)
			<-release
			return &catalogExecutor{descriptor: firstDescriptor}, nil
		}),
		trustedDeferredCatalogRegistration(secondDescriptor, func() (Executor, error) {
			return &catalogExecutor{descriptor: secondDescriptor}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := snapshot.Lookup("first_deferred")
	second, _ := snapshot.Lookup("second_deferred")
	firstDone := make(chan error, 1)
	go func() {
		_, materializeErr := registry.Materialize("first_deferred", first.Revision)
		firstDone <- materializeErr
	}()
	<-started
	if _, err := registry.Materialize("second_deferred", second.Revision); !errors.Is(err, ErrCatalogLimit) {
		t.Fatalf("concurrent materialize error = %v, want limit", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestRegistryMaterializeRejectsLoadedSchemaDrift(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("schema_drift")
	if err := registry.RegisterTrusted(
		"test:schema-drift",
		NewExternalDeferredRegistration(
			ExternalFromDescriptor(descriptor),
			TrustedBindingFromDescriptor(descriptor),
			func() (Executor, error) {
				loaded := cloneDescriptor(descriptor)
				loaded.InputSchema = map[string]any{
					"type": "object",
					"properties": map[string]any{
						"unexpected": map[string]any{"type": "string"},
					},
				}
				return &catalogExecutor{descriptor: loaded}, nil
			},
		),
	); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := snapshot.Lookup("schema_drift")
	if _, err := registry.Materialize("schema_drift", entry.Revision); !errors.Is(err, ErrToolLoadFailed) {
		t.Fatalf("materialize schema drift error = %v, want load failed", err)
	}
}

func TestCatalogToolIDIsStableAndBindingChecked(t *testing.T) {
	registry := NewRegistry(nil, nil)
	descriptor := catalogTestDescriptor("stable_id")
	if err := registry.Register(&catalogExecutor{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Lookup("stable_id")
	if !ok {
		t.Fatal("stable_id is missing")
	}
	if !strings.HasPrefix(entry.Source, "builtin:stable_id:") {
		t.Fatalf("source = %q", entry.Source)
	}
	binding, ok := snapshot.Binding("stable_id")
	if !ok {
		t.Fatal("stable_id binding is missing")
	}
	id, err := registry.ResolveCatalogToolID("stable_id", binding)
	if err != nil {
		t.Fatal(err)
	}
	if id != "builtin:stable_id" {
		t.Fatalf("tool id = %q", id)
	}
	binding.Revision++
	if _, err := registry.ResolveCatalogToolID(
		"stable_id",
		binding,
	); !errors.Is(err, ErrCatalogStale) {
		t.Fatalf("stale binding error = %v", err)
	}
}
