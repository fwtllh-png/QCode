package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestWebSocketReplaysTenThousandEvents(t *testing.T) {
	const count = 10_000
	store := app.NewMemoryEventStore(count)
	for sequence := 1; sequence <= count; sequence++ {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: protocol.Cursor(sequence), OperationID: "operation",
			ThreadID: "thread", TurnID: "turn", ItemID: "item",
		}, &protocol.OutputDeltaData{Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := &eventAuthorizationLifecycle{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session", ThreadID: "thread", Status: protocol.SessionStatusIdle,
		WorkspaceRoot: "/workspace", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	runtime := app.NewRuntime(app.Options{
		EventStore: store, SessionLifecycle: lifecycle, WorkspaceRoot: "/workspace",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	server, origin, token := runningWebServer(t, runtime, Capacity{})

	connection := openWebSocket(t, origin, token, 0)
	defer connection.CloseNow()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var last protocol.Cursor
	for received := 0; received < count; received++ {
		var frame eventFrame
		if err := wsjson.Read(ctx, connection, &frame); err != nil {
			t.Fatalf("read replay event %d: %v", received+1, err)
		}
		if frame.Type != "event" || frame.Event == nil {
			t.Fatalf("frame %d = %+v", received+1, frame)
		}
		if frame.Sequence <= last {
			t.Fatalf("sequence %d followed %d", frame.Sequence, last)
		}
		last = frame.Sequence
	}
	if last != count {
		t.Fatalf("last replay sequence = %d, want %d", last, count)
	}
	_ = server
}

func TestWebSocketCapsBrowserConnectionsAtSixteen(t *testing.T) {
	runtime := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	_, origin, token := runningWebServer(t, runtime, Capacity{})
	connections := make([]*websocket.Conn, 0, defaultMaxConnections)
	defer func() {
		for _, connection := range connections {
			_ = connection.CloseNow()
		}
	}()
	for range defaultMaxConnections {
		connections = append(connections, openWebSocket(t, origin, token, 0))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(
		ctx,
		"ws"+origin[len("http"):]+"/api/v1/events",
		nil,
	)
	if connection != nil {
		_ = connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("seventeenth connection response=%v err=%v", response, err)
	}
}

func TestWebSocketDisconnectStormReleasesSlotsGoroutinesAndDescriptors(t *testing.T) {
	application := app.NewRuntime(app.Options{})
	t.Cleanup(func() { _ = application.Close(context.Background()) })
	_, origin, token := runningWebServer(t, application, Capacity{})
	baselineGoroutines := runtime.NumGoroutine()
	baselineDescriptors, descriptorsAvailable := openDescriptorCount()

	for range 128 {
		connection := openWebSocket(t, origin, token, 0)
		_ = connection.CloseNow()
	}

	connections := make([]*websocket.Conn, 0, defaultMaxConnections)
	for range defaultMaxConnections {
		connections = append(connections, openWebSocket(t, origin, token, 0))
	}
	for _, connection := range connections {
		_ = connection.CloseNow()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		goroutines := runtime.NumGoroutine()
		descriptors, available := openDescriptorCount()
		goroutinesSettled := goroutines <= baselineGoroutines+8
		descriptorsSettled := !descriptorsAvailable ||
			!available ||
			descriptors <= baselineDescriptors+8
		if goroutinesSettled && descriptorsSettled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"resources did not settle: goroutines=%d baseline=%d descriptors=%d baseline=%d",
				goroutines,
				baselineGoroutines,
				descriptors,
				baselineDescriptors,
			)
		}
		time.Sleep(25 * time.Millisecond)
	}

	reopened := make([]*websocket.Conn, 0, defaultMaxConnections)
	defer func() {
		for _, connection := range reopened {
			_ = connection.CloseNow()
		}
	}()
	for range defaultMaxConnections {
		reopened = append(reopened, openWebSocket(t, origin, token, 0))
	}
}

func TestWebSocketSlowReaderReleasesConnectionSlotAfterWriteTimeout(t *testing.T) {
	const count = 2048
	store := app.NewMemoryEventStore(count)
	payload := strings.Repeat("x", 64<<10)
	for sequence := 1; sequence <= count; sequence++ {
		event, err := protocol.NewEvent(protocol.EventMeta{
			Sequence: protocol.Cursor(sequence), OperationID: "operation",
			ThreadID: "thread", TurnID: "turn", ItemID: "item",
		}, &protocol.OutputDeltaData{Text: payload})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := &eventAuthorizationLifecycle{summary: protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: "session", ThreadID: "thread", Status: protocol.SessionStatusIdle,
		WorkspaceRoot: "/workspace", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}}
	application := app.NewRuntime(app.Options{
		EventStore: store, SessionLifecycle: lifecycle, WorkspaceRoot: "/workspace",
	})
	t.Cleanup(func() { _ = application.Close(context.Background()) })
	server, origin, token := runningWebServer(t, application, Capacity{
		MaxConnections: 1, MaxReplayEvents: count,
	})
	connection := openWebSocket(t, origin, token, 0)
	defer connection.CloseNow()

	deadline := time.Now().Add(websocketTimeout + 3*time.Second)
	for server.connections.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow WebSocket retained its connection slot after write timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}

	reopened := openWebSocket(t, origin, token, protocol.Cursor(count))
	_ = reopened.CloseNow()
}

func TestWebSessionCapacityAllowsThirtyTwoAndPreservesIdempotentRetry(t *testing.T) {
	store := newCapacityLifecycleStore()
	runtime := newCapacityRuntime(t, store)
	_, origin, token := runningWebServer(t, runtime, Capacity{})

	var last app.SessionBinding
	for index := range defaultMaxActiveSessions {
		sessionID := "session_" + strconv.Itoa(index)
		binding, problem, status, err := createWebSession(
			t.Context(),
			origin,
			token,
			sessionID,
			"request_"+strconv.Itoa(index),
		)
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || problem != nil {
			t.Fatalf(
				"create session %d status=%d problem=%+v",
				index+1,
				status,
				problem,
			)
		}
		last = binding
	}
	if got := store.activeCount(); got != defaultMaxActiveSessions {
		t.Fatalf("active sessions = %d, want %d", got, defaultMaxActiveSessions)
	}

	retry, problem, status, err := createWebSession(
		t.Context(),
		origin,
		token,
		last.SessionID,
		"request_"+strconv.Itoa(defaultMaxActiveSessions-1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || problem != nil {
		t.Fatalf("idempotent retry status=%d problem=%+v", status, problem)
	}
	if retry != last {
		t.Fatalf("idempotent retry binding = %+v, want %+v", retry, last)
	}

	_, problem, status, err = createWebSession(
		t.Context(),
		origin,
		token,
		"session_over_capacity",
		"request_over_capacity",
	)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTooManyRequests ||
		problem == nil ||
		problem.Code != protocol.CodeResourceExhausted {
		t.Fatalf("over-capacity status=%d problem=%+v", status, problem)
	}
	if got := store.activeCount(); got != defaultMaxActiveSessions {
		t.Fatalf(
			"active sessions after refusal = %d, want %d",
			got,
			defaultMaxActiveSessions,
		)
	}
}

func TestWebSessionCapacityIsAtomicUnderConcurrentCreate(t *testing.T) {
	store := newCapacityLifecycleStore()
	runtime := newCapacityRuntime(t, store)
	_, origin, token := runningWebServer(t, runtime, Capacity{})

	const attempts = defaultMaxActiveSessions * 2
	type result struct {
		status  int
		problem *protocol.Problem
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, attempts)
	var requests sync.WaitGroup
	for index := range attempts {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-start
			_, problem, status, err := createWebSession(
				t.Context(),
				origin,
				token,
				"concurrent_session_"+strconv.Itoa(index),
				"concurrent_request_"+strconv.Itoa(index),
			)
			results <- result{status: status, problem: problem, err: err}
		}()
	}
	close(start)
	requests.Wait()
	close(results)

	var created, exhausted int
	for value := range results {
		if value.err != nil {
			t.Fatal(value.err)
		}
		switch {
		case value.status == http.StatusOK && value.problem == nil:
			created++
		case value.status == http.StatusTooManyRequests &&
			value.problem != nil &&
			value.problem.Code == protocol.CodeResourceExhausted:
			exhausted++
		default:
			t.Fatalf(
				"unexpected concurrent create status=%d problem=%+v",
				value.status,
				value.problem,
			)
		}
	}
	if created != defaultMaxActiveSessions ||
		exhausted != attempts-defaultMaxActiveSessions {
		t.Fatalf("created=%d exhausted=%d", created, exhausted)
	}
	if got := store.activeCount(); got != defaultMaxActiveSessions {
		t.Fatalf("active sessions = %d, want %d", got, defaultMaxActiveSessions)
	}
}

func newCapacityRuntime(
	t *testing.T,
	store *capacityLifecycleStore,
) *app.Runtime {
	t.Helper()
	runtime := app.NewRuntime(app.Options{
		Engine:           app.NewThreadManager(nil),
		WorkspaceRoot:    "/workspace",
		SessionLifecycle: store,
		DefaultProfile: protocol.SessionProfile{
			Provider: "fixture",
			Model:    "fixture",
		},
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}

func createWebSession(
	ctx context.Context,
	origin, token, sessionID, idempotencyKey string,
) (app.SessionBinding, *protocol.Problem, int, error) {
	body, err := json.Marshal(map[string]any{
		"session_id": sessionID,
		"title":      "New Chat",
		"isolation":  "shared",
	})
	if err != nil {
		return app.SessionBinding{}, nil, 0, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		origin+"/api/v1/session/create",
		bytes.NewReader(body),
	)
	if err != nil {
		return app.SessionBinding{}, nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		return app.SessionBinding{}, nil, 0, err
	}
	request.Header.Set("X-QCode-Workspace-ID", identity.RootID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return app.SessionBinding{}, nil, 0, err
	}
	defer response.Body.Close()
	var envelope struct {
		Result  app.SessionBinding `json:"result"`
		Problem *protocol.Problem  `json:"problem"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return app.SessionBinding{}, nil, response.StatusCode, err
	}
	return envelope.Result, envelope.Problem, response.StatusCode, nil
}

func runningWebServer(
	t *testing.T,
	runtime *app.Runtime,
	capacity Capacity,
) (*Server, string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	server, err := New(Options{
		Assets: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte("<main>QCode</main>"), Mode: fs.FileMode(0o444),
			},
		},
		ExpectedHost: host, Origin: "http://" + host, Capacity: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := protocol.NewWorkspaceIdentity(
		"file:///workspace",
		"/workspace",
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Activate(Dependencies{
		Runtime: runtime, WorkspaceRoot: "/workspace",
		WorkspaceIdentity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: server.Handler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Shutdown(context.Background()) })
	origin := "http://" + host
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		origin+"/api/v1/bootstrap",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var bootstrap bootstrapResponse
	if err := decodeJSON(response.Body, &bootstrap); err != nil {
		t.Fatal(err)
	}
	return server, origin, bootstrap.Token
}

func openWebSocket(
	t *testing.T,
	origin, token string,
	cursor protocol.Cursor,
) *websocket.Conn {
	t.Helper()
	response, err := http.Get(origin + "/api/v1/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap bootstrapResponse
	if err := decodeJSON(response.Body, &bootstrap); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if bootstrap.Workspace == nil {
		t.Fatal("bootstrap omitted Workspace identity")
	}
	connection, _, err := websocket.Dial(
		t.Context(),
		"ws"+origin[len("http"):]+"/api/v1/events",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(t.Context(), connection, authFrame{
		Type: "authenticate", Token: token,
		WorkspaceID: bootstrap.Workspace.RootID, Cursor: cursor,
	}); err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	var hello eventFrame
	if err := wsjson.Read(t.Context(), connection, &hello); err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	if hello.Type != "hello" {
		_ = connection.CloseNow()
		t.Fatalf("hello = %+v", hello)
	}
	return connection
}

func decodeJSON(reader interface{ Read([]byte) (int, error) }, value any) error {
	decoder := json.NewDecoder(reader)
	return decoder.Decode(value)
}

func openDescriptorCount() (int, bool) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

type capacityLifecycleStore struct {
	mu       sync.Mutex
	sessions map[string]protocol.SessionSummary
	threads  map[protocol.ThreadID]string
}

func newCapacityLifecycleStore() *capacityLifecycleStore {
	return &capacityLifecycleStore{
		sessions: make(map[string]protocol.SessionSummary),
		threads:  make(map[protocol.ThreadID]string),
	}
}

func (s *capacityLifecycleStore) CreateLifecycle(
	_ context.Context,
	seed protocol.SessionCreateSeed,
) (protocol.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[seed.SessionID]; exists {
		return protocol.SessionSummary{}, errors.New("session already exists")
	}
	now := time.Now().UTC()
	summary := protocol.SessionSummary{
		Version: protocol.SessionLifecycleVersion, Revision: 1,
		SessionID: seed.SessionID, ThreadID: seed.ThreadID,
		Title: seed.Title, Status: protocol.SessionStatusIdle,
		Isolation: seed.Isolation, WorkspaceRoot: seed.WorkspaceRoot,
		WorkspaceLabel: seed.WorkspaceLabel,
		Provider:       seed.Provider, Model: seed.Model,
		ExecutionTarget: "local", CreatedAt: now, UpdatedAt: now,
	}
	s.sessions[seed.SessionID] = summary
	s.threads[seed.ThreadID] = seed.SessionID
	return summary, nil
}

func (s *capacityLifecycleStore) ListLifecycle(
	_ context.Context,
	query protocol.SessionListQuery,
) (protocol.SessionList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := protocol.SessionList{Version: protocol.SessionLifecycleVersion}
	for _, summary := range s.sessions {
		if query.WorkspaceRoot != "" && summary.WorkspaceRoot != query.WorkspaceRoot {
			continue
		}
		if !query.IncludeArchived && summary.Archived {
			continue
		}
		if query.PinnedOnly && !summary.Pinned {
			continue
		}
		result.Sessions = append(result.Sessions, summary)
		if query.Limit > 0 && len(result.Sessions) == query.Limit {
			break
		}
	}
	return result, nil
}

func (s *capacityLifecycleStore) GetLifecycle(
	_ context.Context,
	sessionID string,
) (protocol.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, exists := s.sessions[sessionID]
	if !exists {
		return protocol.SessionSummary{}, errors.New("session not found")
	}
	return summary, nil
}

func (s *capacityLifecycleStore) ThreadIDs(
	_ context.Context,
	sessionID string,
) ([]protocol.ThreadID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, exists := s.sessions[sessionID]
	if !exists {
		return nil, errors.New("session not found")
	}
	return []protocol.ThreadID{summary.ThreadID}, nil
}

func (s *capacityLifecycleStore) SessionForThread(
	_ context.Context,
	threadID protocol.ThreadID,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID, exists := s.threads[threadID]
	if !exists {
		return "", errors.New("thread not found")
	}
	return sessionID, nil
}

func (s *capacityLifecycleStore) ActivateThread(
	_ context.Context,
	sessionID string,
	threadID protocol.ThreadID,
) (protocol.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, exists := s.sessions[sessionID]
	if !exists {
		return protocol.SessionSummary{}, errors.New("session not found")
	}
	delete(s.threads, summary.ThreadID)
	summary.ParentThreadID = summary.ThreadID
	summary.ThreadID = threadID
	summary.Revision++
	summary.UpdatedAt = time.Now().UTC()
	s.sessions[sessionID] = summary
	s.threads[threadID] = sessionID
	return summary, nil
}

func (s *capacityLifecycleStore) UpdateLifecycle(
	_ context.Context,
	sessionID string,
	expectedRevision uint64,
	patch protocol.SessionLifecyclePatch,
) (protocol.SessionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, exists := s.sessions[sessionID]
	if !exists || summary.Revision != expectedRevision {
		return protocol.SessionSummary{}, errors.New("session lifecycle conflict")
	}
	if patch.Archived != nil {
		summary.Archived = *patch.Archived
	}
	summary.Revision++
	summary.UpdatedAt = time.Now().UTC()
	s.sessions[sessionID] = summary
	return summary, nil
}

func (s *capacityLifecycleStore) DeleteLifecycle(
	_ context.Context,
	sessionID string,
	expectedRevision uint64,
) (protocol.SessionDeleteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary, exists := s.sessions[sessionID]
	if !exists || summary.Revision != expectedRevision {
		return protocol.SessionDeleteResult{}, errors.New("session lifecycle conflict")
	}
	delete(s.sessions, sessionID)
	delete(s.threads, summary.ThreadID)
	return protocol.SessionDeleteResult{
		Version: protocol.SessionLifecycleVersion, SessionID: sessionID,
		ThreadID: summary.ThreadID, DeletedAt: time.Now().UTC(),
	}, nil
}

func (s *capacityLifecycleStore) activeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, summary := range s.sessions {
		if !summary.Archived {
			count++
		}
	}
	return count
}
