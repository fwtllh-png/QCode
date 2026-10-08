package agentcontext

import (
	"context"
	"errors"
	"sort"
)

// Bodies are separate CAS objects. Updating focus or adding a report rewrites
// only the index owner; existing source bodies retain their content references.
type ConversationManifest struct {
	Index  OwnerManifest         `json:"index"`
	Bodies map[string]ContentRef `json:"bodies"`
}

func (m ConversationManifest) ContentIDs() []string {
	ids := []string{m.Index.BaseRef.Handle}
	for _, ref := range m.Index.DeltaRefs {
		ids = append(ids, ref.Handle)
	}
	var keys []string
	for key := range m.Bodies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ids = append(ids, m.Bodies[key].Handle)
	}
	return ids
}

func (m ConversationManifest) Validate() error {
	if m.Index.Digest == "" {
		return errors.New("conversation index digest is missing")
	}
	if err := m.Index.BaseRef.Validate(); err != nil {
		return err
	}
	for _, ref := range m.Index.DeltaRefs {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	for id, ref := range m.Bodies {
		if id == "" {
			return errors.New("conversation body source is missing")
		}
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func buildConversationManifest(ctx context.Context, store BlobStore, state *ConversationState, previous *ConversationManifest, limits ManifestLimits) (*ConversationManifest, error) {
	if state == nil {
		return nil, nil
	}
	index := CloneConversation(state)
	manifest := &ConversationManifest{Bodies: make(map[string]ContentRef, len(state.Sources))}
	for id, source := range index.Sources {
		var ref ContentRef
		if previous != nil {
			ref = previous.Bodies[id]
		}
		if ref.Handle == "" {
			var err error
			ref, err = stageValue(ctx, store, "conversation-body", source.Text)
			if err != nil {
				return nil, err
			}
		}
		manifest.Bodies[id] = ref
		source.Text = ""
		index.Sources[id] = source
	}
	var prior OwnerManifest
	if previous != nil {
		prior = previous.Index
	}
	var err error
	manifest.Index, err = appendOwner(ctx, store, "conversation", index, prior, limits)
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func loadConversationManifest(ctx context.Context, store BlobStore, manifest *ConversationManifest) (*ConversationState, error) {
	if manifest == nil {
		return nil, nil
	}
	var state ConversationState
	if err := loadOwner(ctx, store, "conversation", manifest.Index, &state); err != nil {
		return nil, err
	}
	if len(state.Sources) != len(manifest.Bodies) {
		return nil, errors.New("conversation body directory differs from index")
	}
	for id, source := range state.Sources {
		ref, ok := manifest.Bodies[id]
		if !ok || source.Text != "" {
			return nil, errors.New("conversation body reference is invalid")
		}
		if err := readValue(ctx, store, ref, &source.Text); err != nil {
			return nil, err
		}
		state.Sources[id] = source
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return &state, nil
}
