package eventlog

import (
	"container/heap"
	"context"
	"errors"
	"sort"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// Declared session ownership takes precedence over thread ownership. The two
// indexes are disjoint and contain only offsets into the verified log index.
func (l *Log) indexOwner(event protocol.Event, index int) {
	if session := protocol.EventSessionID(event.Data); session != "" {
		if l.bySession == nil {
			l.bySession = make(map[string][]int)
		}
		l.bySession[session] = append(l.bySession[session], index)
	} else {
		if l.byThread == nil {
			l.byThread = make(map[protocol.ThreadID][]int)
		}
		l.byThread[event.ThreadID] = append(l.byThread[event.ThreadID], index)
	}
}

type reverseOwners [][]int

func (h reverseOwners) Len() int { return len(h) }
func (h reverseOwners) Less(i, j int) bool {
	return h[i][len(h[i])-1] > h[j][len(h[j])-1]
}
func (h reverseOwners) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *reverseOwners) Push(value any) { *h = append(*h, value.([]int)) }
func (h *reverseOwners) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

// ReplaySessionBefore returns newest-first events, bounded by an inclusive
// read fence and an optional exclusive before cursor. It decodes only matching
// records, retaining the same byte verification as ordinary replay.
func (l *Log) ReplaySessionBefore(ctx context.Context, sessionID string, threads []protocol.ThreadID, before, through protocol.Cursor, limit int) ([]protocol.Event, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("event replay limit must be positive")
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var owners reverseOwners
	add := func(indices []int) {
		end := sort.Search(len(indices), func(i int) bool {
			sequence := l.entries[indices[i]].Sequence
			return sequence > through || (before != 0 && sequence >= before)
		})
		if end > 0 {
			owners = append(owners, indices[:end])
		}
	}
	add(l.bySession[sessionID])
	seen := make(map[protocol.ThreadID]bool, len(threads))
	for _, thread := range threads {
		if !seen[thread] {
			seen[thread] = true
			add(l.byThread[thread])
		}
	}
	heap.Init(&owners)
	events := make([]protocol.Event, 0, limit)
	for len(owners) > 0 && len(events) < limit {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		indices := heap.Pop(&owners).([]int)
		record, err := l.readVerifiedRecord(l.entries[indices[len(indices)-1]])
		if err != nil {
			return nil, false, err
		}
		events = append(events, record.Event)
		if len(indices) > 1 {
			heap.Push(&owners, indices[:len(indices)-1])
		}
	}
	return events, len(owners) > 0, nil
}
