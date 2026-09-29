package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const controlVersion = 1

type controlConfig struct {
	Version    int       `json:"version"`
	Generation uint64    `json:"generation"`
	Reference  Reference `json:"reference"`
}

type recoveryIntent struct {
	Version            int       `json:"version"`
	OperationID        string    `json:"operation_id"`
	ExpectedGeneration uint64    `json:"expected_generation"`
	OldReference       Reference `json:"old_reference"`
	NewReference       Reference `json:"new_reference"`
	Staged             bool      `json:"staged,omitempty"`
	Phase              string    `json:"phase"`
}

type Control struct {
	mu        sync.Mutex
	root      string
	registry  string
	namespace string
	base      Reference
	keyring   store
}

func OpenControl(
	ctx context.Context,
	dataDir, workspaceID, provider string,
	base Reference,
	selected Reference,
) (*Control, Reference, error) {
	if err := ctx.Err(); err != nil {
		return nil, Reference{}, err
	}
	if strings.TrimSpace(dataDir) == "" ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(provider) == "" {
		return nil, Reference{}, errors.New(
			"credential control data directory, workspace, and provider are required",
		)
	}
	digest := sha256.Sum256([]byte(workspaceID + "\x00" + provider))
	registry := filepath.Join(dataDir, "credential-control")
	control := &Control{
		root:      filepath.Join(registry, hex.EncodeToString(digest[:16])),
		registry:  registry,
		namespace: hex.EncodeToString(digest[:8]),
		base:      base,
		keyring:   keyringStore(),
	}
	if err := control.reconcile(ctx, selected); err != nil {
		return nil, Reference{}, err
	}
	config, err := control.readConfig()
	if err != nil {
		return nil, Reference{}, err
	}
	if config.Generation == 0 {
		return control, base, nil
	}
	if config.Reference == (Reference{}) {
		return control, base, nil
	}
	return control, config.Reference, nil
}

func (c *Control) Reference(ctx context.Context) (Reference, error) {
	if c == nil {
		return Reference{}, errors.New("credential control is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	config, err := c.readConfig()
	if err != nil {
		return Reference{}, err
	}
	if config.Generation == 0 || config.Reference == (Reference{}) {
		return c.base, nil
	}
	return config.Reference, nil
}

// Restore atomically returns a failed connection change to its prior reference.
func (c *Control) Restore(
	ctx context.Context,
	current Reference,
	previous Reference,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := c.readConfig()
	if err != nil {
		return err
	}
	effective := config.Reference
	if config.Generation == 0 || effective == (Reference{}) {
		effective = c.base
	}
	if effective != current && effective != previous {
		return errors.New("credential config generation changed")
	}
	if previous != c.base &&
		previous != (Reference{}) &&
		!c.owns(previous) {
		return errors.New("previous credential reference is not restorable")
	}
	path, intent, err := c.stagedIntent(current)
	if err != nil {
		return err
	}
	if intent.OldReference != previous {
		return errors.New("staged credential rollback target changed")
	}
	intent.Phase = "rollback_requested"
	if err := writeJSONAtomic(path, intent); err != nil {
		return err
	}
	if effective == current {
		target := previous
		if target == c.base {
			target = Reference{}
		}
		if err := c.writeConfigCAS(config.Generation, target); err != nil {
			return err
		}
	}
	canDelete, err := c.canDelete(current)
	if err != nil {
		return err
	}
	if canDelete && current != previous {
		if err := c.keyring.Delete(current.Name); err != nil {
			return err
		}
	}
	intent.Phase = "completed"
	return writeJSONAtomic(path, intent)
}

// Activate publishes a staged reference after the surrounding Host state is
// durable but before live runtimes are swapped.
func (c *Control) Activate(ctx context.Context, current Reference) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := c.readConfig()
	if err != nil {
		return err
	}
	path, intent, err := c.stagedIntent(current)
	if err != nil {
		return err
	}
	effective := config.Reference
	if config.Generation == 0 || effective == (Reference{}) {
		effective = c.base
	}
	if effective != intent.OldReference {
		return errors.New("credential config generation changed")
	}
	intent.Phase = "activation_requested"
	if err := writeJSONAtomic(path, intent); err != nil {
		return err
	}
	if err := c.writeConfigCAS(config.Generation, current); err != nil {
		return err
	}
	intent.Phase = "config_committed"
	return writeJSONAtomic(path, intent)
}

// Commit completes an activated rotation after every consumer has switched.
func (c *Control) Commit(ctx context.Context, current Reference) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := c.readConfig()
	if err != nil {
		return err
	}
	if config.Reference != current {
		return errors.New("credential config generation changed")
	}
	path, intent, err := c.stagedIntent(current)
	if err != nil {
		return err
	}
	if intent.Phase != "config_committed" &&
		intent.Phase != "commit_requested" {
		return errors.New("staged credential rotation is not active")
	}
	intent.Phase = "commit_requested"
	if err := writeJSONAtomic(path, intent); err != nil {
		return err
	}
	canDelete, err := c.canDelete(intent.OldReference)
	if err != nil {
		return err
	}
	if canDelete && intent.OldReference != current {
		if err := c.keyring.Delete(intent.OldReference.Name); err != nil {
			return err
		}
	}
	intent.Phase = "completed"
	return writeJSONAtomic(path, intent)
}

func keyringStore() store {
	return NewKeyringStore()
}

func (c *Control) rotate(
	ctx context.Context,
	current Reference,
	secret string,
	staged bool,
) (Reference, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Reference{}, err
	}
	config, err := c.readConfig()
	if err != nil {
		return Reference{}, err
	}
	if config.Generation == 0 {
		config.Reference = c.base
	}
	if config.Reference != current {
		return Reference{}, errors.New("credential config generation changed")
	}
	operationID, err := randomID()
	if err != nil {
		return Reference{}, err
	}
	next := Reference{
		Kind: "keyring",
		Name: "web/" + c.namespace + "/" + operationID,
	}
	intent := recoveryIntent{
		Version:            controlVersion,
		OperationID:        operationID,
		ExpectedGeneration: config.Generation,
		OldReference:       current,
		NewReference:       next,
		Staged:             staged,
		Phase:              "prepared",
	}
	if err := c.writeIntent(intent); err != nil {
		return Reference{}, err
	}
	if err := c.keyring.Set(next.Name, secret); err != nil {
		return Reference{}, err
	}
	if staged {
		return next, nil
	}
	if err := c.writeConfigCAS(config.Generation, next); err != nil {
		_ = c.keyring.Delete(next.Name)
		return Reference{}, err
	}
	intent.Phase = "config_committed"
	if err := c.writeIntent(intent); err != nil {
		return Reference{}, err
	}
	return next, nil
}

func (c *Control) clear(
	ctx context.Context,
	current Reference,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := c.readConfig()
	if err != nil {
		return err
	}
	if config.Generation == 0 {
		config.Reference = c.base
	}
	if config.Reference != current {
		return errors.New("credential config generation changed")
	}
	operationID, err := randomID()
	if err != nil {
		return err
	}
	intent := recoveryIntent{
		Version:            controlVersion,
		OperationID:        operationID,
		ExpectedGeneration: config.Generation,
		OldReference:       current,
		Phase:              "prepared",
	}
	if err := c.writeIntent(intent); err != nil {
		return err
	}
	if err := c.writeConfigCAS(config.Generation, Reference{}); err != nil {
		return err
	}
	intent.Phase = "config_committed"
	return c.writeIntent(intent)
}

func (c *Control) reconcile(ctx context.Context, selected Reference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.intentDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read credential recovery intents: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.intentDir(), entry.Name())
		var intent recoveryIntent
		if err := readJSON(path, &intent); err != nil {
			return fmt.Errorf("read credential recovery intent: %w", err)
		}
		if intent.Version != controlVersion ||
			intent.OperationID == "" ||
			(intent.NewReference != (Reference{}) &&
				!c.owns(intent.NewReference)) ||
			(intent.Phase != "prepared" &&
				intent.Phase != "activation_requested" &&
				intent.Phase != "config_committed" &&
				intent.Phase != "commit_requested" &&
				intent.Phase != "rollback_requested" &&
				intent.Phase != "completed") {
			return errors.New("credential recovery intent is invalid")
		}
		if intent.Phase == "completed" {
			continue
		}
		config, err := c.readConfig()
		if err != nil {
			return err
		}
		oldReference := intent.OldReference
		if oldReference == c.base {
			oldReference = Reference{}
		}
		committed := config.Generation > intent.ExpectedGeneration &&
			config.Reference == intent.NewReference
		if intent.Staged {
			if intent.Phase == "prepared" &&
				selected == intent.NewReference {
				intent.Phase = "activation_requested"
				if err := writeJSONAtomic(path, intent); err != nil {
					return err
				}
			}
			switch intent.Phase {
			case "prepared":
				if committed {
					intent.Phase = "config_committed"
				} else {
					if err := c.deleteOwned(intent.NewReference); err != nil {
						return err
					}
					intent.Phase = "completed"
					if err := writeJSONAtomic(path, intent); err != nil {
						return err
					}
					continue
				}
			case "activation_requested":
				if !committed {
					if config.Generation != intent.ExpectedGeneration ||
						config.Reference != oldReference {
						return errors.New(
							"credential activation recovery conflicts with config",
						)
					}
					if err := c.writeConfigCAS(
						config.Generation,
						intent.NewReference,
					); err != nil {
						return err
					}
				}
				intent.Phase = "config_committed"
			case "rollback_requested":
				if committed {
					if err := c.writeConfigCAS(
						config.Generation,
						oldReference,
					); err != nil {
						return err
					}
				} else if config.Reference != oldReference {
					return errors.New(
						"credential rollback recovery conflicts with config",
					)
				}
				if err := c.deleteOwned(intent.NewReference); err != nil {
					return err
				}
				intent.Phase = "completed"
				if err := writeJSONAtomic(path, intent); err != nil {
					return err
				}
				continue
			case "config_committed":
				if !committed {
					return errors.New(
						"committed credential rotation is missing from config",
					)
				}
				if selected == oldReference {
					if err := c.writeConfigCAS(
						config.Generation,
						oldReference,
					); err != nil {
						return err
					}
					if err := c.deleteOwned(intent.NewReference); err != nil {
						return err
					}
					intent.Phase = "completed"
					if err := writeJSONAtomic(path, intent); err != nil {
						return err
					}
					continue
				}
				if selected != intent.NewReference {
					return errors.New(
						"credential selection conflicts with active rotation",
					)
				}
			case "commit_requested":
				if !committed {
					return errors.New(
						"committed credential rotation is missing from config",
					)
				}
			}
			if err := c.deleteOwned(intent.OldReference); err != nil {
				return err
			}
			intent.Phase = "completed"
			if err := writeJSONAtomic(path, intent); err != nil {
				return err
			}
			continue
		}
		if !committed {
			if err := c.deleteOwned(intent.NewReference); err != nil {
				return fmt.Errorf("remove uncommitted credential: %w", err)
			}
			intent.Phase = "completed"
			if err := writeJSONAtomic(path, intent); err != nil {
				return err
			}
			continue
		}
		if err := c.deleteOwned(intent.OldReference); err != nil {
			return err
		}
		intent.Phase = "completed"
		if err := writeJSONAtomic(path, intent); err != nil {
			return err
		}
	}
	return nil
}

func (c *Control) stagedIntent(
	current Reference,
) (string, recoveryIntent, error) {
	entries, err := os.ReadDir(c.intentDir())
	if err != nil {
		return "", recoveryIntent{}, err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.intentDir(), entry.Name())
		var intent recoveryIntent
		if err := readJSON(path, &intent); err != nil {
			return "", recoveryIntent{}, err
		}
		if intent.Staged && intent.Phase != "completed" &&
			intent.NewReference == current {
			return path, intent, nil
		}
	}
	return "", recoveryIntent{}, errors.New("staged credential rotation is missing")
}

func (c *Control) deleteOwned(reference Reference) error {
	canDelete, err := c.canDelete(reference)
	if err != nil {
		return err
	}
	if !canDelete {
		return nil
	}
	return c.keyring.Delete(reference.Name)
}

func (c *Control) canDelete(reference Reference) (bool, error) {
	if !c.owns(reference) {
		return false, nil
	}
	registry := c.registry
	if registry == "" {
		registry = filepath.Dir(c.root)
	}
	entries, err := os.ReadDir(registry)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var config controlConfig
		err := readJSON(
			filepath.Join(registry, entry.Name(), "config.json"),
			&config,
		)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if config.Reference == reference {
			return false, nil
		}
	}
	return true, nil
}

func (c *Control) owns(reference Reference) bool {
	return reference.Kind == "keyring" &&
		strings.HasPrefix(reference.Name, "web/"+c.namespace+"/") &&
		len(strings.TrimPrefix(reference.Name, "web/"+c.namespace+"/")) == 32
}

func (c *Control) readConfig() (controlConfig, error) {
	var config controlConfig
	err := readJSON(c.configPath(), &config)
	if errors.Is(err, os.ErrNotExist) {
		return controlConfig{Version: controlVersion}, nil
	}
	if err != nil {
		return controlConfig{}, fmt.Errorf("read credential config: %w", err)
	}
	if config.Version != controlVersion || config.Generation == 0 ||
		(config.Reference != (Reference{}) && !c.owns(config.Reference)) {
		return controlConfig{}, errors.New("credential config is invalid")
	}
	return config, nil
}

func (c *Control) writeConfigCAS(expected uint64, reference Reference) error {
	current, err := c.readConfig()
	if err != nil {
		return err
	}
	if current.Generation != expected {
		return errors.New("credential config generation changed")
	}
	return writeJSONAtomic(c.configPath(), controlConfig{
		Version:    controlVersion,
		Generation: expected + 1,
		Reference:  reference,
	})
}

func (c *Control) writeIntent(intent recoveryIntent) error {
	return writeJSONAtomic(
		filepath.Join(c.intentDir(), intent.OperationID+".json"),
		intent,
	)
}

func (c *Control) configPath() string {
	return filepath.Join(c.root, "config.json")
}

func (c *Control) intentDir() string {
	return filepath.Join(c.root, "intents")
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func readJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("credential control file must be regular")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.OpenFile(
		path+".tmp",
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		0o600,
	)
	if errors.Is(err, os.ErrExist) {
		if removeErr := os.Remove(path + ".tmp"); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		temp, err = os.OpenFile(
			path+".tmp",
			os.O_CREATE|os.O_EXCL|os.O_WRONLY,
			0o600,
		)
	}
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
