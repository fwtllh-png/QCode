package agentcontext

import "sort"

type WorkingSetObservation struct {
	Path   string           `json:"path"`
	Source WorkingSetSource `json:"source"`
	Turn   uint64           `json:"turn"`
}

type WorkingSetDelta struct {
	Observations []WorkingSetObservation `json:"observations,omitempty"`
}

func (l *WorkingSetLedger) Delta() WorkingSetDelta {
	if l == nil {
		return WorkingSetDelta{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var result WorkingSetDelta
	for path, record := range l.records {
		for source, turn := range record.sources {
			result.Observations = append(result.Observations, WorkingSetObservation{
				Path: path, Source: source, Turn: turn,
			})
		}
	}
	sort.Slice(result.Observations, func(i, j int) bool {
		left, right := result.Observations[i], result.Observations[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Source < right.Source
	})
	return result
}

// RetainedDelta returns the bounded live projection used for context recovery.
// Select(limit) still bounds the prompt working set. SourceRead observations
// stay even when their path falls outside that top-N so resume and snapshot
// keep the full already-read set. Older non-read observations remain available
// in the audit stream but no longer make every terminal snapshot grow.
func (l *WorkingSetLedger) RetainedDelta(turn uint64, limit, maxTotal int) WorkingSetDelta {
	if l == nil {
		return WorkingSetDelta{}
	}
	selected := l.Select(turn, limit)
	if maxTotal > 0 && len(selected) > maxTotal {
		selected = selected[:maxTotal]
	}
	kept := make(map[string]struct{}, len(selected))
	for _, entry := range selected {
		kept[entry.Path] = struct{}{}
	}
	full := l.Delta()
	result := WorkingSetDelta{}
	for _, observation := range full.Observations {
		if _, ok := kept[observation.Path]; ok {
			result.Observations = append(result.Observations, observation)
			continue
		}
		if observation.Source == SourceRead {
			result.Observations = append(result.Observations, observation)
		}
	}
	return result
}

func ApplyWorkingSetDelta(delta WorkingSetDelta) *WorkingSetLedger {
	result := NewWorkingSet()
	for _, observation := range delta.Observations {
		result.Observe(observation.Source, observation.Turn, observation.Path)
	}
	return result
}
