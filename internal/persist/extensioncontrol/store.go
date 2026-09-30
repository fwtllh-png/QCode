// Package extensioncontrol persists idempotent Runtime Extension operations.
package extensioncontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/common/atomicfile"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	FileName = "extension-control.json"
	version  = 1
	maxItems = 4096
)

type Entry struct {
	Digest string                          `json:"digest"`
	Status string                          `json:"status"`
	Result protocol.ExtensionControlResult `json:"result"`
}

type document struct {
	Version    int                              `json:"version"`
	Revision   uint64                           `json:"revision"`
	Operations map[string]Entry                 `json:"operations"`
	Events     []protocol.ExtensionControlEvent `json:"events,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("extension control path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(absolute) != FileName {
		return nil, fmt.Errorf("extension control store must be named %s", FileName)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, err
	}
	if err := validateFile(absolute); err != nil {
		return nil, err
	}
	return &Store{path: absolute}, nil
}

func (s *Store) Lookup(
	ctx context.Context,
	id string,
) (Entry, bool, error) {
	if err := contextError(ctx); err != nil {
		return Entry{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return Entry{}, false, err
	}
	entry, ok := value.Operations[id]
	entry.Result.Extensions = append(
		[]protocol.ExtensionProjection(nil), entry.Result.Extensions...,
	)
	return entry, ok, nil
}

func (s *Store) Commit(
	ctx context.Context,
	id, digest string,
	result protocol.ExtensionControlResult,
	event protocol.ExtensionControlEvent,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if id == "" || digest == "" || result.OperationID != id ||
		event.OperationID != id {
		return errors.New("extension control commit is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return err
	}
	if existing, ok := value.Operations[id]; ok {
		if existing.Digest != digest {
			return errors.New("extension operation id conflicts with prior payload")
		}
		if existing.Status == "committed" {
			return nil
		}
		if existing.Status != "prepared" {
			return errors.New("extension operation journal status is invalid")
		}
	}
	value.Revision++
	result.Revision = value.Revision
	if result.Receipt != nil {
		result.Receipt.Revision = value.Revision
	}
	event.Sequence = value.Revision
	event.Receipt.Revision = value.Revision
	value.Operations[id] = Entry{
		Digest: digest, Status: "committed", Result: result,
	}
	value.Events = append(value.Events, event)
	if len(value.Events) > maxItems {
		value.Events = append(
			[]protocol.ExtensionControlEvent(nil),
			value.Events[len(value.Events)-maxItems:]...,
		)
	}
	if len(value.Operations) > maxItems {
		return errors.New("extension control operation journal is full")
	}
	return s.writeLocked(value)
}

func (s *Store) Prepare(ctx context.Context, id, digest string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if id == "" || digest == "" {
		return errors.New("extension control prepare is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return err
	}
	if existing, ok := value.Operations[id]; ok {
		if existing.Digest != digest {
			return errors.New("extension operation id conflicts with prior payload")
		}
		return nil
	}
	if len(value.Operations) >= maxItems {
		return errors.New("extension control operation journal is full")
	}
	value.Operations[id] = Entry{Digest: digest, Status: "prepared"}
	return s.writeLocked(value)
}

func (s *Store) Abort(ctx context.Context, id, digest string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return err
	}
	entry, ok := value.Operations[id]
	if !ok {
		return nil
	}
	if entry.Digest != digest || entry.Status != "prepared" {
		return errors.New("extension control abort conflicts with journal")
	}
	delete(value.Operations, id)
	return s.writeLocked(value)
}

func (s *Store) Snapshot(
	ctx context.Context,
) (uint64, []protocol.ExtensionControlEvent, error) {
	if err := contextError(ctx); err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := s.loadLocked()
	if err != nil {
		return 0, nil, err
	}
	return value.Revision, append(
		[]protocol.ExtensionControlEvent(nil), value.Events...,
	), nil
}

func (s *Store) loadLocked() (document, error) {
	if err := validateFile(s.path); err != nil {
		return document{}, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return document{
			Version: version, Operations: make(map[string]Entry),
		}, nil
	}
	if err != nil {
		return document{}, err
	}
	var value document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return document{}, err
	}
	if value.Version != version || value.Revision < uint64(len(value.Events)) ||
		len(value.Events) > maxItems || len(value.Operations) > maxItems {
		return document{}, errors.New("extension control journal is invalid")
	}
	if value.Operations == nil {
		value.Operations = make(map[string]Entry)
	}
	for id, entry := range value.Operations {
		if entry.Status == "" {
			entry.Status = "committed"
			value.Operations[id] = entry
		}
		if entry.Status != "prepared" && entry.Status != "committed" {
			return document{}, errors.New("extension control journal status is invalid")
		}
	}
	return value, nil
}

func (s *Store) writeLocked(value document) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicfile.Replace(s.path, data, 0o600)
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
		return errors.New("extension control journal must be a regular non-symlink file")
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("extension control context is required")
	}
	return ctx.Err()
}
