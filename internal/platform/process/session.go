package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const defaultSessionOutputLimit = 1 << 20
const defaultSessionLimit = 128

type SessionManager struct {
	mu             sync.RWMutex
	sessions       map[string]*Session
	starting       map[string]journalEntry
	stale          map[string]journalEntry
	journalPath    string
	journalErr     error
	archive        Archive
	maxOutputBytes int
	maxSessions    int
}

// Archive is the durable copy of a session's output. The in-memory buffer is
// bounded, so a job that prints more than the bound drops its beginning and a
// poller that fell behind is told its cursor expired. With an archive the cursor
// still means the same position in the stream and the bytes are still readable.
//
// internal/persist/joblog.Store satisfies it.
type Archive interface {
	Append(id string, data []byte) error
	// Range returns up to limit bytes from offset, plus the total bytes archived.
	Range(id string, offset uint64, limit int) ([]byte, uint64, error)
}

// maxArchiveReplay bounds one read served from the archive. A poller that comes
// back after a job printed a gigabyte gets a screenful and a cursor to continue
// from, not a gigabyte.
const maxArchiveReplay = 256 << 10

type SessionOptions struct {
	Command              string
	Dir                  string
	DirFile              *os.File
	SessionID            string
	LinkedTaskID         string
	ThreadID             string // owner thread lease (N5)
	TurnID               string
	CallID               string
	Rows                 uint16
	Cols                 uint16
	Env                  []string
	PTY                  bool
	Timeout              time.Duration
	Sandbox              sandbox.Backend
	RequireSandbox       bool
	WorkspaceReadOnly    bool
	WorkspaceWritePaths  []string
	DenyNetwork          bool
	TrustedRuntimeHelper bool
	// DetachFromCaller keeps the process alive after Create's ctx ends (background/PTY).
	DetachFromCaller bool
}

type SessionRead struct {
	Data       string
	Cursor     uint64
	Running    bool
	ExitCode   int
	TTY        bool
	Terminated bool
	// Archived is true when the data came from the durable log rather than the
	// live buffer, which tells a caller its cursor had fallen behind.
	Archived bool
	// Pending is how many bytes past Cursor the job has already produced. A read
	// served from the archive is capped, so a caller that is far behind needs to
	// know to keep reading.
	Pending uint64
}

type SessionWait struct {
	SessionRead
	TimedOut bool
}

type Session struct {
	id           string
	commandText  string
	cwd          string
	linkedTaskID string
	threadID     string
	turnID       string
	callID       string
	createdAt    time.Time
	command      *exec.Cmd
	process      *sessionProcess
	input        io.WriteCloser
	outputReader io.ReadCloser
	terminal     *os.File
	tty          bool

	archive    Archive
	mu         sync.RWMutex
	deliveryMu sync.Mutex
	delivered  uint64
	output     []byte
	baseCursor uint64
	running    bool
	exitCode   int
	terminated bool
	waitDone   chan struct{}
	readDone   chan struct{}
	notify     chan struct{}
	closeOnce  sync.Once
	maxOutput  int
}

func NewSessionManager(maxOutputBytes int) *SessionManager {
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultSessionOutputLimit
	}
	return &SessionManager{
		sessions: make(map[string]*Session), starting: make(map[string]journalEntry),
		stale:          make(map[string]journalEntry),
		maxOutputBytes: maxOutputBytes, maxSessions: defaultSessionLimit,
	}
}

func (m *SessionManager) Create(
	ctx context.Context,
	options SessionOptions,
) (_ string, resultErr error) {
	ownerThreadID := strings.TrimSpace(options.ThreadID)
	if ownerThreadID == "" {
		return "", errors.New("terminal session owner thread is required")
	}
	commandText := options.Command
	if commandText == "" {
		commandText = "exec ${SHELL:-/bin/sh}"
	}
	sessionID := options.SessionID
	if sessionID == "" {
		sessionID = randomSessionID()
	}
	if !validSessionID(sessionID) {
		return "", errors.New("session id contains unsupported characters")
	}
	createdAt := time.Now().UTC()
	if err := m.prepareSession(
		sessionID,
		journalEntry{
			ID: sessionID, Command: commandText, Cwd: options.Dir,
			CreatedAt:    createdAt,
			LinkedTaskID: strings.TrimSpace(options.LinkedTaskID),
		},
	); err != nil {
		return "", err
	}
	registered := false
	defer func() {
		if registered {
			return
		}
		resultErr = errors.Join(
			resultErr,
			m.rollbackPreparedSession(sessionID),
		)
	}()
	runCtx := ctx
	if options.DetachFromCaller {
		runCtx = context.WithoutCancel(ctx)
	}
	command, err := NewCommand(runCtx, Options{
		Command: commandText, Dir: options.Dir, DirFile: options.DirFile,
		Env: options.Env, Sandbox: options.Sandbox, PTY: options.PTY,
		TrustedRuntimeHelper: options.TrustedRuntimeHelper,
		RequireSandbox:       options.RequireSandbox,
		WorkspaceReadOnly:    options.WorkspaceReadOnly,
		DenyNetwork:          options.DenyNetwork,
		WorkspaceWritePaths: append(
			[]string(nil), options.WorkspaceWritePaths...,
		),
	})
	if err != nil {
		return "", err
	}
	owner := &sessionProcess{command: command}
	command.Cancel = owner.terminate
	rows, cols := options.Rows, options.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	var input io.WriteCloser
	var output io.ReadCloser
	var terminal *os.File
	if options.PTY {
		terminal, err = pty.StartWithSize(
			command,
			&pty.Winsize{Rows: rows, Cols: cols},
		)
		if err != nil {
			return "", err
		}
		input = terminal
		output = terminal
	} else {
		stdinReader, stdinWriter, pipeErr := os.Pipe()
		if pipeErr != nil {
			return "", pipeErr
		}
		outputReader, outputWriter, pipeErr := os.Pipe()
		if pipeErr != nil {
			_ = stdinReader.Close()
			_ = stdinWriter.Close()
			return "", pipeErr
		}
		command.Stdin = stdinReader
		command.Stdout = outputWriter
		command.Stderr = outputWriter
		if err = command.Start(); err != nil {
			_ = stdinReader.Close()
			_ = stdinWriter.Close()
			_ = outputReader.Close()
			_ = outputWriter.Close()
			return "", err
		}
		_ = stdinReader.Close()
		_ = outputWriter.Close()
		input = stdinWriter
		output = outputReader
	}
	session := &Session{
		id: sessionID, commandText: commandText, cwd: options.Dir,
		linkedTaskID: strings.TrimSpace(options.LinkedTaskID),
		threadID:     ownerThreadID,
		turnID:       strings.TrimSpace(options.TurnID),
		callID:       strings.TrimSpace(options.CallID),
		createdAt:    createdAt,
		command:      command, process: owner, input: input, outputReader: output,
		terminal: terminal, tty: options.PTY,
		running: true, exitCode: -1,
		waitDone: make(chan struct{}), readDone: make(chan struct{}),
		notify: make(chan struct{}, 1), maxOutput: m.maxOutputBytes,
	}
	m.mu.RLock()
	session.archive = m.archive
	m.mu.RUnlock()
	m.mu.Lock()
	delete(m.starting, session.id)
	m.sessions[session.id] = session
	delete(m.stale, session.id)
	m.mu.Unlock()
	registered = true
	go session.readLoop()
	go session.waitLoop()
	if options.Timeout > 0 {
		go func() {
			timer := time.NewTimer(options.Timeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				session.close()
			case <-session.waitDone:
			}
		}()
	}
	if !options.DetachFromCaller {
		go func() {
			select {
			case <-ctx.Done():
				_ = m.Close(session.id, session.threadID)
			case <-session.waitDone:
			}
		}()
	}
	return session.id, nil
}

func validSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func (m *SessionManager) Write(id, threadID string, data []byte) error {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return err
	}
	session.mu.RLock()
	running := session.running
	session.mu.RUnlock()
	if !running {
		return errors.New("terminal session is not running")
	}
	_, err = session.input.Write(data)
	return err
}

// SetArchive installs the durable output log used for reads that fall behind the
// in-memory buffer. It must be set before sessions start; a session created
// earlier has nothing archived to read.
func (m *SessionManager) SetArchive(archive Archive) {
	m.mu.Lock()
	m.archive = archive
	m.mu.Unlock()
}

func (m *SessionManager) Read(id, threadID string, cursor uint64) (SessionRead, error) {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return SessionRead{}, err
	}
	session.mu.RLock()
	end := session.baseCursor + uint64(len(session.output))
	base := session.baseCursor
	running, exitCode, tty := session.running, session.exitCode, session.tty
	terminated := session.terminated
	var live string
	if cursor >= base && cursor <= end {
		live = string(session.output[cursor-base:])
	}
	session.mu.RUnlock()
	if cursor > end {
		return SessionRead{}, errors.New("terminal output cursor is ahead")
	}
	if cursor >= base {
		return SessionRead{
			Data: live, Cursor: end, Running: running, ExitCode: exitCode,
			TTY: tty, Terminated: terminated,
		}, nil
	}
	// The buffer has moved past this cursor. Without an archive the bytes are gone
	// and saying so is the only honest answer.
	m.mu.RLock()
	archive := m.archive
	m.mu.RUnlock()
	if archive == nil {
		return SessionRead{}, errors.New("terminal output cursor expired")
	}
	data, total, err := archive.Range(id, cursor, maxArchiveReplay)
	if err != nil {
		return SessionRead{}, fmt.Errorf("terminal output cursor expired: %w", err)
	}
	next := cursor + uint64(len(data))
	pending := uint64(0)
	if total > next {
		pending = total - next
	}
	return SessionRead{
		Data: string(data), Cursor: next, Running: running, ExitCode: exitCode,
		TTY: tty, Terminated: terminated, Archived: true, Pending: pending,
	}, nil
}

func (m *SessionManager) Wait(
	ctx context.Context,
	id string,
	threadID string,
	cursor uint64,
	timeout time.Duration,
) (SessionWait, error) {
	if timeout < 0 {
		return SessionWait{}, errors.New("terminal wait timeout must not be negative")
	}
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return SessionWait{}, err
	}
	var deadline <-chan time.Time
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		deadline = timer.C
		defer timer.Stop()
	}
	for {
		read, err := m.Read(id, threadID, cursor)
		if err != nil {
			return SessionWait{}, err
		}
		if read.Data != "" || !read.Running {
			return SessionWait{SessionRead: read}, nil
		}
		if timeout == 0 {
			return SessionWait{SessionRead: read, TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return SessionWait{}, ctx.Err()
		case <-deadline:
			read, err := m.Read(id, threadID, cursor)
			if err != nil {
				return SessionWait{}, err
			}
			return SessionWait{SessionRead: read, TimedOut: read.Running}, nil
		case <-session.notify:
		}
	}
}

// WaitNext advances the Runtime-owned delivery cursor for one session. Tool
// callers do not need to echo a cursor that the Runtime already knows.
func (m *SessionManager) WaitNext(
	ctx context.Context,
	id string,
	threadID string,
	timeout time.Duration,
) (SessionWait, error) {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return SessionWait{}, err
	}
	session.deliveryMu.Lock()
	defer session.deliveryMu.Unlock()
	wait, err := m.Wait(
		ctx,
		id,
		threadID,
		session.delivered,
		timeout,
	)
	if err == nil {
		session.delivered = wait.Cursor
	}
	return wait, err
}

// WaitNextEvent advances the Runtime-owned delivery cursor after output arrives
// or the process reaches a terminal state. Unlike WaitNext with a zero timeout,
// it subscribes to the session notification channel instead of polling.
func (m *SessionManager) WaitNextEvent(
	ctx context.Context,
	id string,
	threadID string,
) (SessionWait, error) {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return SessionWait{}, err
	}
	session.deliveryMu.Lock()
	defer session.deliveryMu.Unlock()
	for {
		read, readErr := m.Read(id, threadID, session.delivered)
		if readErr != nil {
			return SessionWait{}, readErr
		}
		if read.Data != "" || !read.Running {
			session.delivered = read.Cursor
			return SessionWait{SessionRead: read}, nil
		}
		select {
		case <-ctx.Done():
			return SessionWait{}, ctx.Err()
		case <-session.notify:
		}
	}
}

func (m *SessionManager) Resize(id, threadID string, rows, cols uint16) error {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return err
	}
	if !session.tty || session.terminal == nil {
		return errors.New("terminal resize requires a TTY session")
	}
	if rows == 0 || cols == 0 {
		return errors.New("terminal rows and columns must be positive")
	}
	return pty.Setsize(session.terminal, &pty.Winsize{Rows: rows, Cols: cols})
}

func (m *SessionManager) Signal(id, threadID string, signal syscall.Signal) error {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return err
	}
	session.mu.RLock()
	running := session.running
	session.mu.RUnlock()
	if !running {
		return errors.New("terminal session is not running")
	}
	session.mu.Lock()
	session.terminated = true
	session.mu.Unlock()
	return session.process.signal(signal)
}

func (m *SessionManager) Close(id, threadID string) error {
	session, err := m.getOwned(id, threadID)
	if err != nil {
		return err
	}
	return m.closeSession(id, session)
}

func (m *SessionManager) closeSession(id string, session *Session) error {
	session.close()
	m.mu.Lock()
	if m.sessions[id] == session {
		delete(m.sessions, id)
	}
	journalErr := m.rewriteJournalLocked()
	if journalErr != nil {
		m.stale[id] = journalEntry{
			ID: session.id, Command: session.commandText, Cwd: session.cwd,
			CreatedAt: session.createdAt, LinkedTaskID: session.linkedTaskID,
		}
		m.journalErr = errors.Join(m.journalErr, journalErr)
	}
	m.mu.Unlock()
	if journalErr != nil {
		return fmt.Errorf(
			"remove terminal session from process journal: %w",
			journalErr,
		)
	}
	return nil
}

func (m *SessionManager) JournalError() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.journalErr
}

func (m *SessionManager) CloseAll() {
	_ = m.CloseAllWithError()
}

func (m *SessionManager) CloseAllWithError() error {
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	var closeErrors []error
	for _, id := range ids {
		session, err := m.get(id)
		if err == nil {
			closeErrors = append(
				closeErrors,
				m.closeSession(id, session),
			)
		}
	}
	return errors.Join(closeErrors...)
}

// CloseByThread terminates sessions leased to threadID. Returns how many were closed.
func (m *SessionManager) CloseByThread(threadID string) (int, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return 0, nil
	}
	m.mu.RLock()
	ids := make([]string, 0)
	for id, session := range m.sessions {
		if session.threadID == threadID {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	var closeErrors []error
	for _, id := range ids {
		session, err := m.get(id)
		if err == nil {
			closeErrors = append(
				closeErrors,
				m.closeSession(id, session),
			)
		}
	}
	return len(ids), errors.Join(closeErrors...)
}

// CloseByTurn terminates sessions created by one Runtime turn without
// disturbing concurrent turns in the same thread.
func (m *SessionManager) CloseByTurn(turnID string) (int, error) {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return 0, nil
	}
	m.mu.RLock()
	ids := make([]string, 0)
	for id, session := range m.sessions {
		if session.turnID == turnID {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	var closeErrors []error
	for _, id := range ids {
		session, err := m.get(id)
		if err == nil {
			closeErrors = append(
				closeErrors,
				m.closeSession(id, session),
			)
		}
	}
	return len(ids), errors.Join(closeErrors...)
}

// OwnerThread returns the lease thread for a live session, or empty if unknown.
func (m *SessionManager) OwnerThread(id string) string {
	session, err := m.get(id)
	if err != nil {
		return ""
	}
	return session.threadID
}

func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *SessionManager) get(id string) (*Session, error) {
	m.mu.RLock()
	session, exists := m.sessions[id]
	m.mu.RUnlock()
	if !exists {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

func (m *SessionManager) getOwned(id, threadID string) (*Session, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if session.threadID != strings.TrimSpace(threadID) {
		return nil, fmt.Errorf(
			"terminal session belongs to another thread: %w",
			ErrSessionOwnership,
		)
	}
	return session, nil
}

// ErrSessionNotFound reports a session id that is not live in this manager.
// It may already have exited and been reaped, or the id may be wrong.
var ErrSessionNotFound = errors.New("terminal session not found")

var ErrSessionOwnership = errors.New("terminal session ownership denied")

func (s *Session) readLoop() {
	defer close(s.readDone)
	buffer := make([]byte, 32<<10)
	for {
		count, err := s.outputReader.Read(buffer)
		if count > 0 {
			if s.archive != nil {
				// A failed append costs recoverability, not the session: the live buffer
				// still has the bytes and the job keeps running.
				_ = s.archive.Append(s.id, buffer[:count])
			}
			s.mu.Lock()
			s.output = append(s.output, buffer[:count]...)
			if overflow := len(s.output) - s.maxOutput; overflow > 0 {
				s.output = append([]byte(nil), s.output[overflow:]...)
				s.baseCursor += uint64(overflow)
			}
			s.mu.Unlock()
			s.signalChange()
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
				return
			}
			return
		}
	}
}

func (s *Session) waitLoop() {
	err := s.process.wait()
	if s.terminal != nil {
		_ = s.terminal.Close()
	}
	<-s.readDone
	s.mu.Lock()
	s.running = false
	s.exitCode = ExitCode(err)
	s.mu.Unlock()
	_ = s.input.Close()
	_ = s.outputReader.Close()
	s.signalChange()
	close(s.waitDone)
}

func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.running {
			if err := s.process.terminate(); err == nil {
				s.terminated = true
			}
		}
		s.mu.Unlock()
		_ = s.input.Close()
		<-s.waitDone
	})
}

func (s *Session) signalChange() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func randomSessionID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	return "term_" + hex.EncodeToString(value[:])
}
