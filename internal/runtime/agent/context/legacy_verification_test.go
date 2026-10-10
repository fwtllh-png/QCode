package agentcontext

import (
	"sort"
	"strings"
)

// Historical fixture helpers, deliberately unavailable to production.
func (s *EvidenceSet) MarkVerified(paths []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range paths {
		if entry, found := s.changes[strings.TrimSpace(path)]; found {
			entry.verified = true
			entry.stale = false
		}
	}
}
func (s *EvidenceSet) UnverifiedPaths() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var paths []string
	for path, entry := range s.changes {
		if !entry.verified {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func (f *Failures) NoteVerify(turn uint64, scope, status, message string) {
	reason := strings.TrimSpace(status)
	if detail := strings.TrimSpace(message); detail != "" {
		if reason == "" {
			reason = detail
		} else {
			reason += ": " + detail
		}
	}
	f.note(KindVerify, turn, scope, "", reason)
}
