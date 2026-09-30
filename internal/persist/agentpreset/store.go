// Package agentpreset persists workspace-scoped Agent presets.
package agentpreset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/atomicfile"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	FileName   = "agent-presets-v1.json"
	maxPresets = 128
)

type Store struct {
	path  string
	mu    sync.Mutex
	state protocol.AgentPresetList
}

func NewMemory() *Store {
	return &Store{state: protocol.AgentPresetList{
		Version: protocol.AgentPresetVersion,
	}}
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("agent preset path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(absolute) != FileName {
		return nil, errors.New("agent preset store has an unexpected filename")
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(absolute), 0o700); mkdirErr != nil {
		return nil, mkdirErr
	}
	if validationErr := validateFile(absolute); validationErr != nil {
		return nil, validationErr
	}
	loaded, loadErr := load(absolute)
	if loadErr != nil {
		return nil, loadErr
	}
	return &Store{path: absolute, state: loaded}, nil
}

func (s *Store) List(ctx context.Context) (protocol.AgentPresetList, error) {
	if err := contextError(ctx); err != nil {
		return protocol.AgentPresetList{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneList(s.state), nil
}

func (s *Store) Get(
	ctx context.Context,
	id string,
) (protocol.AgentPreset, error) {
	if err := contextError(ctx); err != nil {
		return protocol.AgentPreset{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, preset := range s.state.Presets {
		if preset.ID == id {
			return clonePreset(preset), nil
		}
	}
	return protocol.AgentPreset{}, protocol.ErrAgentPresetNotFound
}

func (s *Store) Save(
	ctx context.Context,
	candidate protocol.AgentPreset,
	expectedRevision uint64,
) (protocol.AgentPresetMutationResult, error) {
	if err := contextError(ctx); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	if strings.TrimSpace(candidate.ID) == "" {
		return protocol.AgentPresetMutationResult{}, errors.New("agent preset id is required")
	}
	candidate.Name = strings.TrimSpace(candidate.Name)
	candidate.Description = strings.TrimSpace(candidate.Description)
	candidate.Profile.EnabledToolIDs = append(
		[]string(nil),
		candidate.Profile.EnabledToolIDs...,
	)
	slices.Sort(candidate.Profile.EnabledToolIDs)
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneList(s.state)
	index := -1
	for i, preset := range next.Presets {
		if preset.ID == candidate.ID {
			index = i
			break
		}
	}
	if index >= 0 {
		current := next.Presets[index]
		if expectedRevision != current.Revision {
			if expectedRevision+1 == current.Revision &&
				equalPresetContent(current, candidate) {
				return protocol.AgentPresetMutationResult{
					Version:   protocol.AgentPresetVersion,
					Revision:  next.Revision,
					Preset:    presetPointer(current),
					Duplicate: true,
				}, nil
			}
			return protocol.AgentPresetMutationResult{},
				protocol.ErrAgentPresetRevisionConflict
		}
		candidate.Version = protocol.AgentPresetVersion
		candidate.Revision = current.Revision + 1
		candidate.CreatedAt = current.CreatedAt
		candidate.UpdatedAt = time.Now().UTC()
		next.Presets[index] = clonePreset(candidate)
	} else {
		if expectedRevision != 0 {
			return protocol.AgentPresetMutationResult{},
				protocol.ErrAgentPresetRevisionConflict
		}
		if len(next.Presets) >= maxPresets {
			return protocol.AgentPresetMutationResult{},
				errors.New("agent preset store is full")
		}
		now := time.Now().UTC()
		candidate.Version = protocol.AgentPresetVersion
		candidate.Revision = 1
		candidate.CreatedAt = now
		candidate.UpdatedAt = now
		next.Presets = append(next.Presets, clonePreset(candidate))
	}
	for _, preset := range next.Presets {
		if preset.ID != candidate.ID &&
			strings.EqualFold(strings.TrimSpace(preset.Name), strings.TrimSpace(candidate.Name)) {
			return protocol.AgentPresetMutationResult{},
				protocol.ErrAgentPresetNameConflict
		}
	}
	if err := candidate.Validate(); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	next.Revision++
	sortPresets(next.Presets)
	if err := s.commit(next); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	s.state = next
	return protocol.AgentPresetMutationResult{
		Version:  protocol.AgentPresetVersion,
		Revision: next.Revision,
		Preset:   presetPointer(candidate),
	}, nil
}

func (s *Store) Delete(
	ctx context.Context,
	id string,
	expectedRevision uint64,
) (protocol.AgentPresetMutationResult, error) {
	if err := contextError(ctx); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return protocol.AgentPresetMutationResult{}, errors.New("agent preset id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneList(s.state)
	index := -1
	for i, preset := range next.Presets {
		if preset.ID == id {
			index = i
			if preset.Revision != expectedRevision {
				return protocol.AgentPresetMutationResult{},
					protocol.ErrAgentPresetRevisionConflict
			}
			break
		}
	}
	if index < 0 {
		return protocol.AgentPresetMutationResult{
			Version:   protocol.AgentPresetVersion,
			Revision:  next.Revision,
			DeletedID: id,
			Duplicate: true,
		}, nil
	}
	next.Presets = slices.Delete(next.Presets, index, index+1)
	next.Revision++
	if err := s.commit(next); err != nil {
		return protocol.AgentPresetMutationResult{}, err
	}
	s.state = next
	return protocol.AgentPresetMutationResult{
		Version:   protocol.AgentPresetVersion,
		Revision:  next.Revision,
		DeletedID: id,
	}, nil
}

func (s *Store) commit(next protocol.AgentPresetList) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Replace(s.path, append(data, '\n'), 0o600)
}

func load(path string) (protocol.AgentPresetList, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.AgentPresetList{Version: protocol.AgentPresetVersion}, nil
	}
	if err != nil {
		return protocol.AgentPresetList{}, err
	}
	var value protocol.AgentPresetList
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return protocol.AgentPresetList{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return protocol.AgentPresetList{}, errors.New(
			"agent preset store has trailing JSON",
		)
	}
	if err := value.Validate(); err != nil {
		return protocol.AgentPresetList{}, err
	}
	sortPresets(value.Presets)
	return value, nil
}

func cloneList(value protocol.AgentPresetList) protocol.AgentPresetList {
	result := value
	result.Presets = make([]protocol.AgentPreset, len(value.Presets))
	for index, preset := range value.Presets {
		result.Presets[index] = clonePreset(preset)
	}
	return result
}

func clonePreset(value protocol.AgentPreset) protocol.AgentPreset {
	value.Profile.EnabledToolIDs = append(
		[]string(nil),
		value.Profile.EnabledToolIDs...,
	)
	return value
}

func presetPointer(value protocol.AgentPreset) *protocol.AgentPreset {
	copy := clonePreset(value)
	return &copy
}

func equalPresetContent(
	left, right protocol.AgentPreset,
) bool {
	return left.Name == right.Name &&
		left.Description == right.Description &&
		left.Scope == right.Scope &&
		left.Profile.Mode == right.Profile.Mode &&
		left.Profile.Provider == right.Profile.Provider &&
		left.Profile.Model == right.Profile.Model &&
		left.Profile.ReasoningEffort == right.Profile.ReasoningEffort &&
		slices.Equal(left.Profile.EnabledToolIDs, right.Profile.EnabledToolIDs) &&
		left.Profile.ApprovalPosture == right.Profile.ApprovalPosture &&
		left.Profile.ExecutionTarget == right.Profile.ExecutionTarget &&
		left.Profile.MaxSteps == right.Profile.MaxSteps
}

func sortPresets(values []protocol.AgentPreset) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name == values[j].Name {
			return values[i].ID < values[j].ID
		}
		return values[i].Name < values[j].Name
	})
}

func validateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("agent preset store must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("agent preset store permissions are too broad")
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("agent preset context is required")
	}
	return ctx.Err()
}
