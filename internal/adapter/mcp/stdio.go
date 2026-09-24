package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	platformprocess "github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/processbroker"
)

const defaultStderrTailBytes = 32 << 10

type StdioTransport struct {
	lifecycle *processbroker.Lifecycle
	stdin     io.WriteCloser

	writeMu        sync.Mutex
	mu             sync.Mutex
	pending        map[string]chan Response
	closed         bool
	readErr        error
	onNotification func(Notification)
	onFailure      func(error)
	nextID         atomic.Uint64

	stderr      *tailBuffer
	done        chan struct{}
	processDone chan struct{}
	once        sync.Once
}

func (t *StdioTransport) SetNotificationHandler(handler func(Notification)) {
	t.mu.Lock()
	t.onNotification = handler
	t.mu.Unlock()
}

func (t *StdioTransport) SetFailureHandler(handler func(error)) {
	t.mu.Lock()
	t.onFailure = handler
	t.mu.Unlock()
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func NewAuthorizedStdioTransport(
	ctx context.Context,
	name string,
	config ServerConfig,
	runtime *RuntimeAuthority,
) (*StdioTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = "stdio"
	}
	// 只校验声明的环境条目；原始声明传给进程边界，由
	// process.NewCommand 统一做宿主继承合并与沙盒策略覆盖。在这里
	// 预先合成完整环境会把继承的 HOME/TMPDIR 误当成声明再次消毒。
	if err := platformprocess.ValidateDeclaredEnvironment(config.Env); err != nil {
		return nil, fmt.Errorf("sanitize MCP environment: %w", err)
	}
	lifecycle, err := runtime.Start(ctx, name, config, config.Env)
	if err != nil {
		return nil, fmt.Errorf("start MCP stdio server: %w", err)
	}
	stdin, err := lifecycle.Stdin()
	if err != nil {
		return nil, err
	}
	stdout, err := lifecycle.Stdout()
	if err != nil {
		_ = lifecycle.Close(context.Background())
		return nil, err
	}
	stderrReader, err := lifecycle.Stderr()
	if err != nil {
		_ = lifecycle.Close(context.Background())
		return nil, err
	}
	stderr := newTailBuffer(defaultStderrTailBytes)
	transport := &StdioTransport{
		lifecycle:   lifecycle,
		stdin:       stdin,
		pending:     make(map[string]chan Response),
		stderr:      stderr,
		done:        make(chan struct{}),
		processDone: make(chan struct{}),
	}
	go transport.readLoop(stdout)
	go func() { _, _ = io.Copy(stderr, stderrReader) }()
	go func() {
		err := lifecycle.Wait(context.Background())
		transport.finish(err)
		close(transport.processDone)
	}()
	return transport, nil
}

func (t *StdioTransport) Request(
	ctx context.Context,
	method string,
	params any,
	result any,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method == "" {
		return errors.New("MCP request method is required")
	}
	id := t.nextID.Add(1)
	rawID := json.RawMessage(strconv.FormatUint(id, 10))
	rawParams, err := MarshalParams(params)
	if err != nil {
		return err
	}
	rawParams = withTraceMetadata(ctx, rawParams)
	responseChannel := make(chan Response, 1)
	key := string(rawID)
	t.mu.Lock()
	if t.closed {
		err := t.transportErrorLocked()
		t.mu.Unlock()
		return err
	}
	t.pending[key] = responseChannel
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
	}()

	if err := t.write(Request{
		JSONRPC: JSONRPCVersion,
		ID:      rawID,
		Method:  method,
		Params:  rawParams,
	}); err != nil {
		return err
	}
	select {
	case response := <-responseChannel:
		return decodeResult(response, result)
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = t.Notify(cancelCtx, "notifications/cancelled", CancelledParams{
			RequestID: rawID,
			Reason:    ctx.Err().Error(),
		})
		cancel()
		return ctx.Err()
	case <-t.done:
		t.mu.Lock()
		err := t.transportErrorLocked()
		t.mu.Unlock()
		return err
	}
}

func (t *StdioTransport) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rawParams, err := MarshalParams(params)
	if err != nil {
		return err
	}
	rawParams = withTraceMetadata(ctx, rawParams)
	return t.write(Request{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  rawParams,
	})
}

func (t *StdioTransport) write(request Request) error {
	return t.writeValue(request)
}

func (t *StdioTransport) writeValue(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	if t.closed {
		err := t.transportErrorLocked()
		t.mu.Unlock()
		return err
	}
	t.mu.Unlock()
	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("write MCP stdio request: %w", err)
	}
	return nil
}

func (t *StdioTransport) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var message wireMessage
		if err := DecodeStrict(line, &message); err != nil {
			t.finish(fmt.Errorf("decode MCP stdio response: %w", err))
			return
		}
		if message.JSONRPC != JSONRPCVersion {
			t.finish(errors.New("invalid MCP JSON-RPC response"))
			return
		}
		if message.Method != "" {
			if len(message.ID) != 0 {
				_ = t.writeValue(Response{
					JSONRPC: JSONRPCVersion,
					ID:      message.ID,
					Error:   &RPCError{Code: -32601, Message: "method not found"},
				})
			} else {
				t.mu.Lock()
				handler := t.onNotification
				t.mu.Unlock()
				if handler != nil {
					handler(Notification{Method: message.Method, Params: message.Params})
				}
			}
			continue
		}
		if len(message.ID) == 0 ||
			(message.Error == nil && len(message.Result) == 0) ||
			(message.Error != nil && len(message.Result) != 0) {
			t.finish(errors.New("invalid MCP JSON-RPC response"))
			return
		}
		response := Response{
			JSONRPC: message.JSONRPC,
			ID:      message.ID,
			Result:  message.Result,
			Error:   message.Error,
		}
		t.mu.Lock()
		channel := t.pending[string(response.ID)]
		t.mu.Unlock()
		if channel != nil {
			channel <- response
		}
	}
	if err := scanner.Err(); err != nil {
		t.finish(fmt.Errorf("read MCP stdio response: %w", err))
	}
}

func (t *StdioTransport) finish(err error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.readErr = err
		handler := t.onFailure
		idle := len(t.pending) == 0
		t.mu.Unlock()
		close(t.done)
		if err != nil && handler != nil && idle {
			handler(err)
		}
	})
}

func (t *StdioTransport) Close(ctx context.Context) error {
	t.mu.Lock()
	alreadyClosed := t.closed
	t.onFailure = nil
	t.mu.Unlock()
	if !alreadyClosed && ctx.Err() == nil {
		requestTimeout := time.Second
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining > 0 {
				requestTimeout = min(requestTimeout, max(time.Millisecond, remaining/2))
			}
		}
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		var ignored map[string]any
		_ = t.Request(requestCtx, "shutdown", map[string]any{}, &ignored)
		cancel()
	}
	_ = t.stdin.Close()
	if waitForProcess(t.processDone, ctx, 100*time.Millisecond) {
		return nil
	}

	_ = t.lifecycle.Signal(os.Interrupt)
	if waitForProcess(t.processDone, ctx, 250*time.Millisecond) {
		return nil
	}
	_ = t.lifecycle.Close(context.WithoutCancel(ctx))
	select {
	case <-t.processDone:
		return nil
	case <-time.After(time.Second):
		return errors.New("MCP stdio process did not exit after kill")
	}
}

func waitForProcess(done <-chan struct{}, ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (t *StdioTransport) StderrTail() string {
	return t.stderr.String()
}

func (t *StdioTransport) transportErrorLocked() error {
	message := "MCP stdio transport closed"
	if t.readErr != nil {
		message += ": " + t.readErr.Error()
	}
	if stderr := strings.TrimSpace(t.stderr.String()); stderr != "" {
		message += "; stderr tail: " + stderr
	}
	return errors.New(message)
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(value)
	if len(value) >= b.limit {
		b.data = append(b.data[:0], value[len(value)-b.limit:]...)
		return original, nil
	}
	overflow := len(b.data) + len(value) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, value...)
	return original, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}
