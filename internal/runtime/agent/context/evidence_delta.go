package agentcontext

import "sort"

type EvidenceReadState struct {
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Turn       uint64 `json:"turn"`
	RepeatTurn uint64 `json:"repeat_turn,omitempty"`
	Repeats    int    `json:"repeats,omitempty"`
	Stale      bool   `json:"stale,omitempty"`
}

type EvidenceHandleState struct {
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	Turn     uint64 `json:"turn"`
	Consumed bool   `json:"consumed,omitempty"`
}

type EvidenceDelta struct {
	Turn    uint64                `json:"turn"`
	Facts   []EvidenceFact        `json:"facts,omitempty"`
	Changes []EvidenceChange      `json:"changes,omitempty"`
	Reads   []EvidenceReadState   `json:"reads,omitempty"`
	Handles []EvidenceHandleState `json:"handles,omitempty"`
}

func (s *EvidenceSet) Delta() EvidenceDelta {
	if s == nil {
		return EvidenceDelta{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := EvidenceDelta{Turn: s.turn}
	for _, fact := range s.facts {
		result.Facts = append(result.Facts, fact)
	}
	for path, entry := range s.changes {
		result.Changes = append(result.Changes, EvidenceChange{
			Path: path, Turn: entry.turn, Read: entry.read,
			Verified: entry.verified, Diagnostics: entry.diagnostics,
			Stale: entry.stale,
		})
	}
	for path, entry := range s.reads {
		result.Reads = append(result.Reads, EvidenceReadState{
			Path: path, Digest: entry.digest, Turn: entry.turn,
			RepeatTurn: entry.repeatTurn, Repeats: entry.repeats,
			Stale: entry.stale,
		})
	}
	for id, entry := range s.handles {
		result.Handles = append(result.Handles, EvidenceHandleState{
			ID: id, Tool: entry.tool, Turn: entry.turn,
			Consumed: entry.consumed,
		})
	}
	sort.Slice(result.Facts, func(i, j int) bool {
		left, right := result.Facts[i], result.Facts[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Line < right.Line
	})
	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].Path < result.Changes[j].Path
	})
	sort.Slice(result.Reads, func(i, j int) bool {
		return result.Reads[i].Path < result.Reads[j].Path
	})
	sort.Slice(result.Handles, func(i, j int) bool {
		return result.Handles[i].ID < result.Handles[j].ID
	})
	return result
}

// RetainedDelta returns the bounded live evidence projection. Mandatory change
// risks are never removed here; admission is responsible for refusing growth
// that would make that set unrepresentable. Reads stay complete so resume can
// avoid re-reading files that dropped out of the prompt working-set top-N.
func (s *EvidenceSet) RetainedDelta(
	factLimit int,
	verifiedChangeRetentionTurns uint64,
	handleLimit int,
) EvidenceDelta {
	if s == nil {
		return EvidenceDelta{}
	}
	full := s.Delta()
	snapshot := s.Snapshot(factLimit)
	result := EvidenceDelta{Turn: full.Turn, Facts: snapshot.Facts}
	verified := 0
	for _, change := range full.Changes {
		mandatory := !change.Verified || change.Diagnostics || change.Stale
		recent := full.Turn < change.Turn ||
			full.Turn-change.Turn <= verifiedChangeRetentionTurns
		if !mandatory && (!recent || factLimit > 0 && verified == factLimit) {
			continue
		}
		if !mandatory {
			verified++
		}
		result.Changes = append(result.Changes, change)
	}
	for _, read := range full.Reads {
		result.Reads = append(result.Reads, read)
	}
	handles := append([]EvidenceHandleState(nil), full.Handles...)
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].Consumed != handles[j].Consumed {
			return !handles[i].Consumed
		}
		if handles[i].Turn != handles[j].Turn {
			return handles[i].Turn > handles[j].Turn
		}
		return handles[i].ID < handles[j].ID
	})
	for _, handle := range handles {
		if handle.Consumed || handleLimit > 0 &&
			len(result.Handles) == handleLimit {
			continue
		}
		result.Handles = append(result.Handles, handle)
	}
	return result
}

func ApplyEvidenceDelta(delta EvidenceDelta) *EvidenceSet {
	result := NewEvidenceSet()
	result.turn = delta.Turn
	for _, fact := range delta.Facts {
		result.facts[factKey{
			kind: fact.Kind, path: fact.Path, line: fact.Line,
		}] = fact
	}
	for _, value := range delta.Changes {
		result.changes[value.Path] = &change{
			turn: value.Turn, read: value.Read,
			verified: value.Verified, diagnostics: value.Diagnostics,
			stale: value.Stale,
		}
	}
	for _, value := range delta.Reads {
		result.reads[value.Path] = &read{
			digest: value.Digest, turn: value.Turn,
			repeatTurn: value.RepeatTurn, repeats: value.Repeats,
			stale: value.Stale,
		}
	}
	for _, value := range delta.Handles {
		result.handles[value.ID] = &handle{
			tool: value.Tool, turn: value.Turn, consumed: value.Consumed,
		}
	}
	return result
}
