package app

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type SessionHistoryQuery struct {
	SessionID string
	Since     protocol.Cursor
	Before    protocol.Cursor
	Limit     int
}

type SessionHistoryPage struct {
	SessionID  string           `json:"session_id"`
	Events     []protocol.Event `json:"events"`
	Next       protocol.Cursor  `json:"next_sequence"`
	More       bool             `json:"more"`
	Previous   protocol.Cursor  `json:"previous_sequence,omitempty"`
	MoreBefore bool             `json:"more_before,omitempty"`
}

type SessionPresentationSnapshot struct {
	Version                int               `json:"version"`
	SessionID              string            `json:"session_id"`
	ThreadID               protocol.ThreadID `json:"thread_id"`
	SessionRevision        uint64            `json:"session_revision"`
	ThroughSequence        protocol.Cursor   `json:"through_sequence"`
	Events                 []protocol.Event  `json:"events"`
	HistoryTruncatedBefore protocol.Cursor   `json:"history_truncated_before,omitempty"`
}

type SessionExport struct {
	Version    int                         `json:"version"`
	ExportedAt time.Time                   `json:"exported_at"`
	Session    protocol.SessionSummary     `json:"session"`
	Snapshot   SessionPresentationSnapshot `json:"snapshot"`
	Integrity  SessionExportIntegrity      `json:"integrity"`
}

type SessionExportIntegrity struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}
