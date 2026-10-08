package agentcontext

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

const (
	worldMarkerPrefix = "qcode:world:"
)

type WorldMode string

const (
	WorldFull  WorldMode = "full"
	WorldPatch WorldMode = "patch"
)

// WorldSection is the current rendered value of one diffable context section.
// A nil Message represents an intentionally empty or removed section.
type WorldSection struct {
	ID      string
	Digest  string
	Present bool
	Message *provider.Message
}

type WorldEntry struct {
	ID       string `json:"id"`
	Digest   string `json:"digest"`
	Revision uint64 `json:"revision"`
	Present  bool   `json:"present"`
}

// WorldBaseline binds section digests to fragments retained in durable history.
type WorldBaseline struct {
	Revision uint64       `json:"revision,omitempty"`
	Digest   string       `json:"digest,omitempty"`
	Entries  []WorldEntry `json:"entries,omitempty"`
}

type WorldProjection struct {
	Mode     WorldMode
	Messages []provider.Message
	Baseline WorldBaseline
	Changed  []string
}

type worldMarker struct {
	ID       string    `json:"id"`
	Digest   string    `json:"digest"`
	Revision uint64    `json:"revision"`
	Mode     WorldMode `json:"mode"`
	Present  bool      `json:"present"`
}

func ProjectWorld(
	current []WorldSection,
	baseline WorldBaseline,
	history []provider.Message,
) (WorldProjection, error) {
	valid := WorldBaselineValid(history, baseline)
	mode := WorldPatch
	if !valid {
		mode = WorldFull
		baseline = WorldBaseline{}
	}
	sections := make(map[string]WorldSection, len(current))
	for _, section := range current {
		if section.ID == "" || section.Digest == "" {
			return WorldProjection{}, errors.New("world section id and digest are required")
		}
		if _, exists := sections[section.ID]; exists {
			return WorldProjection{}, fmt.Errorf("duplicate world section %q", section.ID)
		}
		if section.Message != nil {
			message := CloneMessage(*section.Message)
			section.Message = &message
		}
		sections[section.ID] = section
	}
	if !valid {
		for id, section := range sections {
			if !section.Present && section.Message == nil {
				delete(sections, id)
			}
		}
	}
	previous := make(map[string]WorldEntry, len(baseline.Entries))
	for _, entry := range baseline.Entries {
		previous[entry.ID] = entry
	}
	for id, section := range sections {
		if section.Present || section.Message != nil {
			continue
		}
		if _, existed := previous[id]; !existed {
			delete(sections, id)
		}
	}
	if valid {
		for id := range previous {
			if _, exists := sections[id]; exists {
				continue
			}
			sections[id] = WorldSection{ID: id, Digest: absentWorldDigest(id)}
		}
	}
	ids := make([]string, 0, len(sections))
	for id := range sections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	revision := baseline.Revision
	var changed []string
	for _, id := range ids {
		section := sections[id]
		entry, existed := previous[id]
		present := section.Present || section.Message != nil
		if mode == WorldPatch && existed &&
			entry.Digest == section.Digest && entry.Present == present {
			continue
		}
		changed = append(changed, id)
	}
	if len(changed) != 0 || baseline.Revision == 0 {
		revision++
	}
	nextEntries := make([]WorldEntry, 0, len(ids))
	var messages []provider.Message
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	for _, id := range ids {
		section := sections[id]
		present := section.Present || section.Message != nil
		entry, existed := previous[id]
		if _, changed := changedSet[id]; changed {
			if present && section.Message == nil {
				return WorldProjection{}, fmt.Errorf(
					"changed world section %q is missing rendered content",
					id,
				)
			}
			entry = WorldEntry{
				ID: id, Digest: section.Digest,
				Revision: revision, Present: present,
			}
			messages = append(messages, worldMessage(section, entry, mode))
		} else if !existed {
			return WorldProjection{}, fmt.Errorf("world section %q has no baseline entry", id)
		}
		nextEntries = append(nextEntries, entry)
	}
	next := WorldBaseline{Revision: revision, Entries: nextEntries}
	next.Digest = worldBaselineDigest(next)
	return WorldProjection{
		Mode: mode, Messages: messages, Baseline: next, Changed: changed,
	}, nil
}

func WorldBaselineValid(
	history []provider.Message,
	baseline WorldBaseline,
) bool {
	if baseline.Revision == 0 || baseline.Digest == "" ||
		worldBaselineDigest(baseline) != baseline.Digest {
		return false
	}
	latest := make(map[string]worldMarker)
	for _, message := range history {
		marker, _, ok := parseWorldMessage(message)
		if ok {
			latest[marker.ID] = marker
		}
	}
	for _, entry := range baseline.Entries {
		marker, ok := latest[entry.ID]
		if !ok || marker.Digest != entry.Digest ||
			marker.Revision != entry.Revision ||
			marker.Present != entry.Present {
			return false
		}
	}
	return true
}

func CloneWorldBaseline(value WorldBaseline) WorldBaseline {
	value.Entries = append([]WorldEntry(nil), value.Entries...)
	return value
}

func StripWorldState(messages []provider.Message) []provider.Message {
	result := make([]provider.Message, 0, len(messages))
	for _, message := range messages {
		if _, _, ok := parseWorldMessage(message); ok {
			continue
		}
		result = append(result, message)
	}
	return result
}

// InspectWorldMessage returns content-safe metadata carried outside model text.
func InspectWorldMessage(
	message provider.Message,
) (WorldEntry, WorldMode, bool) {
	marker, _, ok := parseWorldMessage(message)
	if !ok {
		return WorldEntry{}, "", false
	}
	return WorldEntry{
		ID: marker.ID, Digest: marker.Digest, Revision: marker.Revision,
		Present: marker.Present,
	}, marker.Mode, true
}

func worldMessage(
	section WorldSection,
	entry WorldEntry,
	mode WorldMode,
) provider.Message {
	role, turn, body := provider.RoleSystem, uint64(0), "(section removed)"
	if entry.Present && section.Message != nil {
		role, turn, body = section.Message.Role, section.Message.Turn, section.Message.Text()
	} else if entry.Present {
		body = "(section retained from the previous world-state baseline)"
	}
	metadata, _ := json.Marshal(worldMarker{
		ID: entry.ID, Digest: entry.Digest, Revision: entry.Revision,
		Mode: mode, Present: entry.Present,
	})
	message := provider.TextMessage(role, body)
	message.Blocks[0].ID = worldMarkerPrefix +
		base64.RawURLEncoding.EncodeToString(metadata)
	message.Turn = turn
	return message
}

func parseWorldMessage(
	message provider.Message,
) (worldMarker, string, bool) {
	if len(message.Blocks) != 1 ||
		message.Blocks[0].Type != provider.ContentText {
		return worldMarker{}, "", false
	}
	id := message.Blocks[0].ID
	if len(id) <= len(worldMarkerPrefix) ||
		id[:len(worldMarkerPrefix)] != worldMarkerPrefix {
		return worldMarker{}, "", false
	}
	encoded, err := base64.RawURLEncoding.DecodeString(id[len(worldMarkerPrefix):])
	if err != nil {
		return worldMarker{}, "", false
	}
	var marker worldMarker
	if json.Unmarshal(encoded, &marker) != nil ||
		marker.ID == "" || marker.Digest == "" || marker.Revision == 0 ||
		marker.Mode != WorldFull && marker.Mode != WorldPatch {
		return worldMarker{}, "", false
	}
	return marker, message.Text(), true
}

func worldBaselineDigest(value WorldBaseline) string {
	entries := append([]WorldEntry(nil), value.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	encoded, _ := json.Marshal(struct {
		Revision uint64       `json:"revision"`
		Entries  []WorldEntry `json:"entries"`
	}{Revision: value.Revision, Entries: entries})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func absentWorldDigest(id string) string {
	sum := sha256.Sum256([]byte("absent:" + id))
	return hex.EncodeToString(sum[:])
}
