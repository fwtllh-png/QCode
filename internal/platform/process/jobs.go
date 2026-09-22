package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JobStatus distinguishes live process sessions from journal-only stale rows.
type JobStatus string

const (
	JobStatusRunning JobStatus = "running"
	JobStatusExited  JobStatus = "exited"
	JobStatusStale   JobStatus = "stale"
)

// JobInfo is the jobs-center view of a terminal/background session.
type JobInfo struct {
	ID           string    `json:"id"`
	Command      string    `json:"command"`
	Cwd          string    `json:"cwd,omitempty"`
	Status       JobStatus `json:"status"`
	Running      bool      `json:"running"`
	ExitCode     int       `json:"exit_code"`
	CreatedAt    time.Time `json:"created_at"`
	LinkedTaskID string    `json:"linked_task_id,omitempty"`
	OutputTail   string    `json:"output_tail,omitempty"`
	Cursor       uint64    `json:"cursor"`
}

type journalEntry struct {
	ID           string    `json:"id"`
	Command      string    `json:"command"`
	Cwd          string    `json:"cwd,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LinkedTaskID string    `json:"linked_task_id,omitempty"`
}

// SetJournalPath configures durable jobs-journal.jsonl for stale recovery after restart.
func (m *SessionManager) SetJournalPath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journalPath = strings.TrimSpace(path)
}

// LoadStaleJournal reads journal entries that are not currently live and exposes them as stale.
func (m *SessionManager) LoadStaleJournal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journalPath == "" {
		return nil
	}
	payload, err := os.ReadFile(m.journalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if m.stale == nil {
		m.stale = map[string]journalEntry{}
	}
	lines := strings.Split(string(payload), "\n")
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry journalEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil ||
			entry.ID == "" {
			if index == len(lines)-1 &&
				!strings.HasSuffix(string(payload), "\n") {
				break
			}
			if err == nil {
				err = errors.New("process journal entry has no id")
			}
			return fmt.Errorf(
				"decode process journal line %d: %w",
				index+1,
				err,
			)
		}
		if _, live := m.sessions[entry.ID]; live {
			continue
		}
		m.stale[entry.ID] = entry
	}
	return nil
}

func (m *SessionManager) appendJournalLocked(entry journalEntry) error {
	if m.journalPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.journalPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(m.journalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(append(payload, '\n')); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

func (m *SessionManager) prepareSession(
	id string,
	entry journalEntry,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[id]; exists {
		return errors.New("terminal session id already exists")
	}
	if _, exists := m.starting[id]; exists {
		return errors.New("terminal session id already exists")
	}
	if len(m.sessions)+len(m.starting) >= m.maxSessions {
		for existingID, session := range m.sessions {
			session.mu.RLock()
			running := session.running
			session.mu.RUnlock()
			if !running {
				// Evict through closeSession semantics so the on-disk journal
				// stays consistent with the in-memory map; a bare delete
				// leaves a stale journal row behind until the next rewrite.
				_ = m.closeSessionLocked(existingID, session)
				break
			}
		}
	}
	if len(m.sessions)+len(m.starting) >= m.maxSessions {
		return errors.New("terminal session capacity exceeded")
	}
	m.starting[id] = entry
	if err := m.appendJournalLocked(entry); err != nil {
		delete(m.starting, id)
		return fmt.Errorf(
			"record terminal session in process journal: %w",
			err,
		)
	}
	return nil
}

func (m *SessionManager) rollbackPreparedSession(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.starting[id]; !exists {
		return nil
	}
	delete(m.starting, id)
	err := m.rewriteJournalLocked()
	if err != nil {
		m.journalErr = errors.Join(m.journalErr, err)
	}
	return err
}

func (m *SessionManager) rewriteJournalLocked() error {
	if m.journalPath == "" {
		return nil
	}
	entries := make(
		[]journalEntry,
		0,
		len(m.sessions)+len(m.stale)+len(m.starting),
	)
	for _, session := range m.sessions {
		entries = append(entries, journalEntry{
			ID: session.id, Command: session.commandText, Cwd: session.cwd,
			CreatedAt: session.createdAt, LinkedTaskID: session.linkedTaskID,
		})
	}
	for _, entry := range m.stale {
		entries = append(entries, entry)
	}
	for _, entry := range m.starting {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})
	var builder strings.Builder
	for _, entry := range entries {
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		builder.Write(payload)
		builder.WriteByte('\n')
	}
	parent := filepath.Dir(m.journalPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".jobs-journal-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.WriteString(builder.String()); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, m.journalPath); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// List returns live sessions plus stale journal rows.
func (m *SessionManager) List() []JobInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]JobInfo, 0, len(m.sessions)+len(m.stale))
	for _, session := range m.sessions {
		out = append(out, session.jobInfo())
	}
	for _, entry := range m.stale {
		out = append(out, JobInfo{
			ID: entry.ID, Command: entry.Command, Cwd: entry.Cwd,
			Status: JobStatusStale, Running: false, ExitCode: -1,
			CreatedAt: entry.CreatedAt, LinkedTaskID: entry.LinkedTaskID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Info returns one job by id (live or stale).
func (m *SessionManager) Info(id string) (JobInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if session, ok := m.sessions[id]; ok {
		return session.jobInfo(), true
	}
	if entry, ok := m.stale[id]; ok {
		return JobInfo{
			ID: entry.ID, Command: entry.Command, Cwd: entry.Cwd,
			Status: JobStatusStale, Running: false, ExitCode: -1,
			CreatedAt: entry.CreatedAt, LinkedTaskID: entry.LinkedTaskID,
		}, true
	}
	return JobInfo{}, false
}

// Poll reads current output; when wait is true it blocks briefly for new data or exit.
func (m *SessionManager) Poll(ctx context.Context, id string, wait bool) (JobInfo, error) {
	info, ok := m.Info(id)
	if !ok {
		return JobInfo{}, errors.New("job not found")
	}
	if info.Status == JobStatusStale {
		return JobInfo{}, errors.New("stale job cannot be polled")
	}
	session, err := m.get(id)
	if err != nil {
		return JobInfo{}, err
	}
	threadID := session.threadID
	cursor := info.Cursor
	if wait {
		waited, waitErr := m.Wait(
			ctx,
			id,
			threadID,
			cursor,
			jobsListWait,
		)
		if waitErr != nil {
			return JobInfo{}, waitErr
		}
		_ = waited
	} else {
		if _, err := m.Read(id, threadID, cursor); err != nil {
			return JobInfo{}, err
		}
	}
	return session.jobInfo(), nil
}

// Stdin writes to a live job PTY.
func (m *SessionManager) Stdin(id, data string) error {
	info, ok := m.Info(id)
	if !ok {
		return errors.New("job not found")
	}
	if info.Status == JobStatusStale {
		return errors.New("stale job cannot accept stdin")
	}
	return m.Write(id, m.OwnerThread(id), []byte(data))
}

// Cancel closes a live job or clears a stale journal row.
func (m *SessionManager) Cancel(id string) error {
	m.mu.Lock()
	if _, stale := m.stale[id]; stale {
		entry := m.stale[id]
		delete(m.stale, id)
		err := m.rewriteJournalLocked()
		if err != nil {
			m.stale[id] = entry
			m.journalErr = errors.Join(m.journalErr, err)
		}
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	return m.Close(id, m.OwnerThread(id))
}

// CancelAll closes all live sessions and clears stale journal rows.
func (m *SessionManager) CancelAll() error {
	closeErr := m.CloseAllWithError()
	m.mu.Lock()
	previous := m.stale
	m.stale = map[string]journalEntry{}
	rewriteErr := m.rewriteJournalLocked()
	if rewriteErr != nil {
		m.stale = previous
		m.journalErr = errors.Join(m.journalErr, rewriteErr)
	}
	m.mu.Unlock()
	return errors.Join(closeErr, rewriteErr)
}

func (s *Session) jobInfo() JobInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := JobStatusExited
	if s.running {
		status = JobStatusRunning
	}
	tail := string(s.output)
	const maxTail = jobsOutputTailBytes // package const; see declaration
	if len(tail) > maxTail {
		tail = "…" + tail[len(tail)-maxTail:]
	}
	return JobInfo{
		ID: s.id, Command: s.commandText, Cwd: s.cwd, Status: status,
		Running: s.running, ExitCode: s.exitCode, CreatedAt: s.createdAt,
		LinkedTaskID: s.linkedTaskID, OutputTail: tail,
		Cursor: s.baseCursor + uint64(len(s.output)),
	}
}

// jobsListWait bounds one Session listing wait when results stream. Public
// contract constant.
const jobsListWait = 30 * time.Second

// jobsOutputTailBytes bounds the output tail each job entry carries. Public
// contract constant.
const jobsOutputTailBytes = 4 << 10
