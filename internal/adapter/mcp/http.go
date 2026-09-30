package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

var (
	errLegacyRequired     = errors.New("MCP server requires legacy SSE")
	errLegacyStreamClosed = errors.New("MCP legacy SSE stream closed")
	errStaleSession       = errors.New("MCP session is stale")
)

type httpMode uint8

const (
	httpModeUnknown httpMode = iota
	httpModeStreamable
	httpModeLegacy
)

type HTTPTransport struct {
	config ServerConfig
	client *http.Client
	oauth  OAuthProvider

	nextID atomic.Uint64
	closed atomic.Bool

	mu               sync.RWMutex
	mode             httpMode
	session          string
	generation       uint64
	initializeParams any
	onNotification   func(Notification)

	reconnectMu sync.Mutex

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	legacyMu       sync.Mutex
	legacyEndpoint string
	legacyBody     io.ReadCloser
	legacyCancel   context.CancelFunc
	legacyPending  map[string]chan Response
	legacyDone     chan struct{}
	legacyErr      error
}

func (t *HTTPTransport) SetNotificationHandler(handler func(Notification)) {
	t.mu.Lock()
	t.onNotification = handler
	t.mu.Unlock()
}

type sseEvent struct {
	event string
	data  []byte
}

func NewHTTPTransport(ctx context.Context, config ServerConfig) (*HTTPTransport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.URL) == "" {
		return nil, errors.New("MCP HTTP URL is required")
	}
	if err := validHTTPURL(config.URL); err != nil {
		return nil, errors.New("MCP HTTP URL is invalid")
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 10 * time.Second
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = 30 * time.Second
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = 4 << 20
	}
	if config.MaxChunkBytes <= 0 {
		config.MaxChunkBytes = 1 << 20
	}
	if config.InboundQueue <= 0 {
		config.InboundQueue = 64
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   config.ConnectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
		}}
	}
	oauth := config.OAuthProvider
	if oauth == nil && config.OAuth != nil {
		manager, err := NewOAuthManager(*config.OAuth, client, nil, nil)
		if err != nil {
			return nil, err
		}
		oauth = manager
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	transport := &HTTPTransport{
		config:          config,
		client:          client,
		oauth:           oauth,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		legacyPending:   make(map[string]chan Response),
		legacyDone:      make(chan struct{}),
	}
	if config.Transport == "sse" {
		transport.mode = httpModeLegacy
	}
	return transport, nil
}

func NewDefaultTransport(
	ctx context.Context,
	_ string,
	config ServerConfig,
) (Transport, error) {
	if config.Transport == "stdio" {
		return nil, errors.New("stdio MCP transport requires runtime authority")
	}
	return NewHTTPTransport(ctx, config)
}

func NewAuthorizedTransportFactory(runtime *RuntimeAuthority) TransportFactory {
	return func(
		ctx context.Context,
		name string,
		config ServerConfig,
	) (Transport, error) {
		if config.Transport == "stdio" {
			return NewAuthorizedStdioTransport(ctx, name, config, runtime)
		}
		return NewHTTPTransport(ctx, config)
	}
}

func (t *HTTPTransport) Request(
	ctx context.Context,
	method string,
	params any,
	result any,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.closed.Load() {
		return errors.New("MCP HTTP transport is closed")
	}
	if method == "" {
		return errors.New("MCP request method is required")
	}
	id := json.RawMessage(strconv.FormatUint(t.nextID.Add(1), 10))
	rawParams, err := MarshalParams(params)
	if err != nil {
		return err
	}
	rawParams = withTraceMetadata(ctx, rawParams)
	request := Request{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Method:  method,
		Params:  rawParams,
	}
	if method == "initialize" {
		t.mu.Lock()
		t.initializeParams = params
		t.mu.Unlock()
	}
	generation := t.currentGeneration()
	response, err := t.requestOnce(ctx, request)
	if err != nil && isStaleSession(err) {
		var reconnectErr error
		if method == "initialize" {
			reconnectErr = t.reconnectInitial(ctx, generation)
		} else {
			reconnectErr = t.reconnect(ctx, generation)
		}
		if reconnectErr != nil {
			return fmt.Errorf("reconnect MCP session: %w", reconnectErr)
		}
		if !safeReconnectReplay(method) {
			return fmt.Errorf("%w: MCP session reconnected; business request was not replayed", err)
		}
		response, err = t.requestOnce(ctx, request)
	}
	if err != nil {
		if ctx.Err() != nil {
			t.sendCancellation(id, ctx.Err())
			return ctx.Err()
		}
		return err
	}
	return decodeResult(response, result)
}

func safeReconnectReplay(method string) bool {
	switch method {
	case "initialize", "ping", "tools/list", "resources/list",
		"resources/templates/list", "prompts/list":
		return true
	default:
		return false
	}
}

func (t *HTTPTransport) reconnectInitial(
	ctx context.Context,
	observedGeneration uint64,
) error {
	t.reconnectMu.Lock()
	defer t.reconnectMu.Unlock()
	if t.currentGeneration() != observedGeneration {
		return nil
	}
	t.mu.RLock()
	mode := t.mode
	t.mu.RUnlock()
	if mode == httpModeLegacy {
		t.closeLegacyStream()
		if err := t.connectLegacy(ctx); err != nil {
			return err
		}
		t.mu.Lock()
		t.generation++
		t.mu.Unlock()
		return nil
	}
	t.mu.Lock()
	t.session = ""
	t.generation++
	t.mu.Unlock()
	return nil
}

func (t *HTTPTransport) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rawParams, err := MarshalParams(params)
	if err != nil {
		return err
	}
	rawParams = withTraceMetadata(ctx, rawParams)
	request := Request{JSONRPC: JSONRPCVersion, Method: method, Params: rawParams}
	generation := t.currentGeneration()
	_, err = t.requestOnce(ctx, request)
	if err != nil && method != "notifications/initialized" && isStaleSession(err) {
		if reconnectErr := t.reconnect(ctx, generation); reconnectErr != nil {
			return fmt.Errorf("reconnect MCP session: %w", reconnectErr)
		}
		_, err = t.requestOnce(ctx, request)
	}
	return err
}

func (t *HTTPTransport) requestOnce(ctx context.Context, request Request) (Response, error) {
	t.mu.RLock()
	mode := t.mode
	t.mu.RUnlock()
	if mode == httpModeLegacy {
		return t.requestLegacy(ctx, request)
	}
	response, err := t.requestStreamable(ctx, request)
	if errors.Is(err, errLegacyRequired) && mode == httpModeUnknown {
		if connectErr := t.connectLegacy(ctx); connectErr != nil {
			return Response{}, errors.Join(err, connectErr)
		}
		t.mu.Lock()
		t.mode = httpModeLegacy
		t.mu.Unlock()
		return t.requestLegacy(ctx, request)
	}
	if err == nil && mode == httpModeUnknown {
		t.mu.Lock()
		t.mode = httpModeStreamable
		t.mu.Unlock()
	}
	return response, err
}

func (t *HTTPTransport) requestStreamable(
	ctx context.Context,
	rpcRequest Request,
) (Response, error) {
	body, err := json.Marshal(rpcRequest)
	if err != nil {
		return Response{}, err
	}
	if int64(len(body)) > t.config.MaxBodyBytes {
		return Response{}, errors.New("MCP HTTP request exceeds body limit")
	}
	readCtx, cancel := withTimeoutIfSooner(ctx, t.config.ReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		readCtx,
		http.MethodPost,
		t.config.URL,
		bytes.NewReader(body),
	)
	if err != nil {
		return Response{}, errors.New("create MCP HTTP request")
	}
	if err := t.applyHeaders(readCtx, request, "application/json, text/event-stream"); err != nil {
		return Response{}, err
	}
	tracecontext.InjectHTTP(readCtx, request.Header)
	request.Header.Set("Content-Type", "application/json")
	t.mu.RLock()
	session := t.session
	t.mu.RUnlock()
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	httpResponse, err := t.client.Do(request)
	if err != nil {
		if readCtx.Err() != nil {
			return Response{}, readCtx.Err()
		}
		return Response{}, errors.New("execute MCP HTTP request")
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode == http.StatusMethodNotAllowed ||
		httpResponse.StatusCode == http.StatusNotAcceptable ||
		httpResponse.StatusCode == http.StatusUnsupportedMediaType ||
		(httpResponse.StatusCode == http.StatusNotFound && session == "") {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes))
		return Response{}, errLegacyRequired
	}
	if httpResponse.StatusCode == http.StatusNotFound || httpResponse.StatusCode == http.StatusGone {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes))
		return Response{}, errStaleSession
	}
	if httpResponse.StatusCode == http.StatusAccepted && len(rpcRequest.ID) == 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes))
		return Response{}, nil
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes))
		return Response{}, fmt.Errorf("MCP HTTP request failed with status %d", httpResponse.StatusCode)
	}
	if len(rpcRequest.ID) == 0 {
		t.captureSession(httpResponse.Header)
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes))
		return Response{}, nil
	}
	mediaType, _, _ := mime.ParseMediaType(httpResponse.Header.Get("Content-Type"))
	var response Response
	switch mediaType {
	case "text/event-stream":
		response, err = readSSEResponse(
			httpResponse.Body,
			rpcRequest.ID,
			t.config.MaxBodyBytes,
			t.config.MaxChunkBytes,
			t.handleNotification,
		)
	case "application/json", "":
		err = decodeBoundedJSON(httpResponse.Body, t.config.MaxBodyBytes, &response)
	default:
		err = fmt.Errorf("unsupported MCP HTTP response content type %q", mediaType)
	}
	if err != nil {
		return Response{}, err
	}
	if staleResponse(response) {
		return Response{}, errStaleSession
	}
	t.captureSession(httpResponse.Header)
	return response, nil
}

func (t *HTTPTransport) captureSession(headers http.Header) {
	session := headers.Get("Mcp-Session-Id")
	if session == "" {
		return
	}
	t.mu.Lock()
	t.session = session
	t.mu.Unlock()
}

func (t *HTTPTransport) currentGeneration() uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.generation
}

func (t *HTTPTransport) reconnect(ctx context.Context, observedGeneration uint64) error {
	t.reconnectMu.Lock()
	defer t.reconnectMu.Unlock()
	if t.currentGeneration() != observedGeneration {
		return nil
	}
	t.mu.RLock()
	mode := t.mode
	initializeParams := t.initializeParams
	t.mu.RUnlock()
	if initializeParams == nil {
		return errors.New("MCP session cannot reconnect before initialize")
	}
	if mode == httpModeLegacy {
		t.closeLegacyStream()
		if err := t.connectLegacy(ctx); err != nil {
			return err
		}
		t.mu.Lock()
		t.generation++
		t.mu.Unlock()
	} else {
		t.mu.Lock()
		t.session = ""
		t.generation++
		t.mu.Unlock()
	}
	initializeID := json.RawMessage(strconv.FormatUint(t.nextID.Add(1), 10))
	params, err := MarshalParams(initializeParams)
	if err != nil {
		return err
	}
	response, err := t.requestOnce(ctx, Request{
		JSONRPC: JSONRPCVersion,
		ID:      initializeID,
		Method:  "initialize",
		Params:  params,
	})
	if err != nil {
		return err
	}
	if err := decodeResult(response, &InitializeResult{}); err != nil {
		return err
	}
	_, err = t.requestOnce(ctx, Request{
		JSONRPC: JSONRPCVersion,
		Method:  "notifications/initialized",
		Params:  json.RawMessage(`{}`),
	})
	return err
}

func (t *HTTPTransport) connectLegacy(ctx context.Context) error {
	t.legacyMu.Lock()
	if t.legacyBody != nil && t.legacyErr == nil {
		t.legacyMu.Unlock()
		return nil
	}
	t.legacyMu.Unlock()

	connectTimeout := t.config.ConnectTimeout
	if deadline, ok := ctx.Deadline(); ok {
		connectTimeout = min(connectTimeout, max(time.Millisecond, time.Until(deadline)))
	}
	streamCtx, streamCancel := context.WithCancel(t.lifecycleCtx)
	connectDone := make(chan struct{})
	timer := time.NewTimer(connectTimeout)
	go func() {
		select {
		case <-ctx.Done():
			streamCancel()
		case <-timer.C:
			streamCancel()
		case <-connectDone:
		}
	}()
	request, err := http.NewRequestWithContext(
		streamCtx,
		http.MethodGet,
		t.config.URL,
		nil,
	)
	if err != nil {
		close(connectDone)
		timer.Stop()
		streamCancel()
		return errors.New("create MCP legacy SSE request")
	}
	if err := t.applyHeaders(ctx, request, "text/event-stream"); err != nil {
		close(connectDone)
		timer.Stop()
		streamCancel()
		return err
	}
	tracecontext.InjectHTTP(ctx, request.Header)
	response, err := t.client.Do(request)
	close(connectDone)
	timer.Stop()
	if err != nil {
		streamCancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("connect MCP legacy SSE stream")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		streamCancel()
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
			return errStaleSession
		}
		return fmt.Errorf("MCP legacy SSE request failed with status %d", response.StatusCode)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		response.Body.Close()
		streamCancel()
		return errors.New("MCP legacy endpoint did not return an event stream")
	}
	t.captureSession(response.Header)
	endpoint := make(chan string, 1)
	inbound := make(chan sseEvent, t.config.InboundQueue)
	streamDone := make(chan struct{})

	t.legacyMu.Lock()
	t.legacyBody = response.Body
	t.legacyCancel = streamCancel
	t.legacyDone = streamDone
	t.legacyErr = nil
	t.legacyEndpoint = ""
	t.legacyMu.Unlock()
	go t.readLegacyEvents(response.Body, inbound, streamDone)
	go t.dispatchLegacyEvents(inbound, endpoint, streamDone)

	select {
	case discovered := <-endpoint:
		t.legacyMu.Lock()
		t.legacyEndpoint = discovered
		t.legacyMu.Unlock()
		return nil
	case <-streamDone:
		return t.legacyStreamError()
	case <-ctx.Done():
		t.closeLegacyStream()
		return ctx.Err()
	}
}

func (t *HTTPTransport) readLegacyEvents(
	body io.Reader,
	inbound chan<- sseEvent,
	done chan struct{},
) {
	err := scanSSE(body, t.config.MaxBodyBytes, t.config.MaxChunkBytes, func(event sseEvent) error {
		select {
		case inbound <- event:
			return nil
		default:
			return errors.New("MCP legacy SSE inbound queue is full")
		}
	})
	t.legacyMu.Lock()
	if err == nil {
		err = errLegacyStreamClosed
	}
	t.legacyErr = err
	t.legacyMu.Unlock()
	close(inbound)
	close(done)
}

func (t *HTTPTransport) dispatchLegacyEvents(
	inbound <-chan sseEvent,
	endpoint chan<- string,
	done <-chan struct{},
) {
	for event := range inbound {
		switch event.event {
		case "endpoint":
			discovered, err := resolveEndpoint(t.config.URL, strings.TrimSpace(string(event.data)))
			if err != nil {
				t.legacyMu.Lock()
				t.legacyErr = err
				t.legacyMu.Unlock()
				t.closeLegacyStream()
				return
			}
			select {
			case endpoint <- discovered:
			default:
			}
		default:
			var message wireMessage
			if DecodeStrict(event.data, &message) != nil {
				continue
			}
			if message.Method != "" && len(message.ID) == 0 {
				t.mu.RLock()
				handler := t.onNotification
				t.mu.RUnlock()
				if handler != nil {
					handler(Notification{Method: message.Method, Params: message.Params})
				}
				continue
			}
			response := Response{
				JSONRPC: message.JSONRPC, ID: message.ID,
				Result: message.Result, Error: message.Error,
			}
			if len(response.ID) == 0 {
				continue
			}
			t.legacyMu.Lock()
			pending := t.legacyPending[string(response.ID)]
			t.legacyMu.Unlock()
			if pending != nil {
				select {
				case pending <- response:
				case <-done:
				}
			}
		}
	}
}

func (t *HTTPTransport) requestLegacy(
	ctx context.Context,
	rpcRequest Request,
) (Response, error) {
	t.legacyMu.Lock()
	connected := t.legacyBody != nil && t.legacyErr == nil && t.legacyEndpoint != ""
	t.legacyMu.Unlock()
	if !connected {
		if err := t.connectLegacy(ctx); err != nil {
			return Response{}, err
		}
	}
	body, err := json.Marshal(rpcRequest)
	if err != nil {
		return Response{}, err
	}
	if int64(len(body)) > t.config.MaxBodyBytes {
		return Response{}, errors.New("MCP legacy request exceeds body limit")
	}
	var responseChannel chan Response
	key := string(rpcRequest.ID)
	if len(rpcRequest.ID) != 0 {
		responseChannel = make(chan Response, 1)
		t.legacyMu.Lock()
		t.legacyPending[key] = responseChannel
		t.legacyMu.Unlock()
		defer func() {
			t.legacyMu.Lock()
			delete(t.legacyPending, key)
			t.legacyMu.Unlock()
		}()
	}
	t.legacyMu.Lock()
	endpoint := t.legacyEndpoint
	streamDone := t.legacyDone
	t.legacyMu.Unlock()
	readCtx, cancel := withTimeoutIfSooner(ctx, t.config.ReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		readCtx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return Response{}, errors.New("create MCP legacy POST request")
	}
	if err := t.applyHeaders(readCtx, request, "application/json, text/event-stream"); err != nil {
		return Response{}, err
	}
	tracecontext.InjectHTTP(readCtx, request.Header)
	request.Header.Set("Content-Type", "application/json")
	t.mu.RLock()
	session := t.session
	t.mu.RUnlock()
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	httpResponse, err := t.client.Do(request)
	if err != nil {
		if readCtx.Err() != nil {
			return Response{}, readCtx.Err()
		}
		return Response{}, errors.New("execute MCP legacy POST request")
	}
	t.captureSession(httpResponse.Header)
	_, readErr := io.Copy(io.Discard, io.LimitReader(httpResponse.Body, t.config.MaxBodyBytes+1))
	closeErr := httpResponse.Body.Close()
	if readErr != nil || closeErr != nil {
		return Response{}, errors.New("read MCP legacy POST response")
	}
	if httpResponse.StatusCode == http.StatusNotFound || httpResponse.StatusCode == http.StatusGone {
		return Response{}, errStaleSession
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{}, fmt.Errorf("MCP legacy POST failed with status %d", httpResponse.StatusCode)
	}
	if len(rpcRequest.ID) == 0 {
		return Response{}, nil
	}
	select {
	case response := <-responseChannel:
		if staleResponse(response) {
			return Response{}, errStaleSession
		}
		return response, nil
	case <-streamDone:
		return Response{}, t.legacyStreamError()
	case <-readCtx.Done():
		return Response{}, readCtx.Err()
	}
}

func (t *HTTPTransport) legacyStreamError() error {
	t.legacyMu.Lock()
	defer t.legacyMu.Unlock()
	if t.legacyErr != nil {
		return fmt.Errorf("%w: %v", errLegacyStreamClosed, t.legacyErr)
	}
	return errLegacyStreamClosed
}

func (t *HTTPTransport) closeLegacyStream() {
	t.legacyMu.Lock()
	body := t.legacyBody
	cancel := t.legacyCancel
	t.legacyBody = nil
	t.legacyCancel = nil
	t.legacyEndpoint = ""
	t.legacyErr = errLegacyStreamClosed
	t.legacyMu.Unlock()
	if body != nil {
		_ = body.Close()
	}
	if cancel != nil {
		cancel()
	}
}

func (t *HTTPTransport) sendCancellation(id json.RawMessage, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	params, _ := MarshalParams(CancelledParams{RequestID: id, Reason: cause.Error()})
	request := Request{
		JSONRPC: JSONRPCVersion,
		Method:  "notifications/cancelled",
		Params:  params,
	}
	_, _ = t.requestOnce(ctx, request)
}

func (t *HTTPTransport) Close(ctx context.Context) error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	defer t.lifecycleCancel()
	t.mu.RLock()
	mode := t.mode
	session := t.session
	t.mu.RUnlock()
	var closeErr error
	if mode == httpModeStreamable && session != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.config.URL, nil)
		if err == nil {
			err = t.applyHeaders(ctx, request, "application/json")
		}
		if err == nil {
			tracecontext.InjectHTTP(ctx, request.Header)
			request.Header.Set("Mcp-Session-Id", session)
			response, requestErr := t.client.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, t.config.MaxBodyBytes))
				response.Body.Close()
				if response.StatusCode != http.StatusMethodNotAllowed &&
					response.StatusCode != http.StatusNotImplemented &&
					response.StatusCode >= 200 && response.StatusCode < 300 {
					t.closeLegacyStream()
					return nil
				}
			}
		}
	}
	shutdownID := json.RawMessage(strconv.FormatUint(t.nextID.Add(1), 10))
	params := json.RawMessage(`{}`)
	_, closeErr = t.requestOnce(ctx, Request{
		JSONRPC: JSONRPCVersion,
		ID:      shutdownID,
		Method:  "shutdown",
		Params:  params,
	})
	t.closeLegacyStream()
	if mode == httpModeLegacy && isStaleSession(closeErr) {
		return nil
	}
	return closeErr
}

func (t *HTTPTransport) StderrTail() string {
	return ""
}

func (t *HTTPTransport) applyHeaders(
	ctx context.Context,
	request *http.Request,
	accept string,
) error {
	request.Header.Set("Accept", accept)
	for name, value := range t.config.Headers {
		request.Header.Set(name, value)
	}
	for name, envName := range t.config.HeaderEnv {
		if request.Header.Get(name) != "" {
			continue
		}
		value := os.Getenv(envName)
		if value == "" {
			return fmt.Errorf("MCP header environment variable %q is empty", envName)
		}
		request.Header.Set(name, value)
	}
	if request.Header.Get("Authorization") != "" {
		return nil
	}
	if t.config.BearerTokenEnv != "" {
		token := os.Getenv(t.config.BearerTokenEnv)
		if token == "" {
			return fmt.Errorf(
				"MCP bearer token environment variable %q is empty",
				t.config.BearerTokenEnv,
			)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	if t.oauth != nil {
		authorization, err := t.oauth.Authorization(ctx)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", authorization)
	}
	return nil
}

func decodeBoundedJSON(reader io.Reader, limit int64, target any) error {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errors.New("MCP HTTP response exceeds body limit")
	}
	return DecodeStrict(body, target)
}

func (t *HTTPTransport) handleNotification(notification Notification) {
	t.mu.RLock()
	handler := t.onNotification
	t.mu.RUnlock()
	if handler != nil {
		handler(notification)
	}
}

func readSSEResponse(
	reader io.Reader,
	id json.RawMessage,
	bodyLimit int64,
	chunkLimit int,
	onNotification func(Notification),
) (Response, error) {
	var result Response
	err := scanSSE(reader, bodyLimit, chunkLimit, func(event sseEvent) error {
		if len(event.data) == 0 {
			return nil
		}
		var message wireMessage
		if err := DecodeStrict(event.data, &message); err != nil {
			return fmt.Errorf("decode MCP SSE response: %w", err)
		}
		if message.Method != "" && len(message.ID) == 0 {
			if onNotification != nil {
				onNotification(Notification{Method: message.Method, Params: message.Params})
			}
			return nil
		}
		response := Response{
			JSONRPC: message.JSONRPC, ID: message.ID,
			Result: message.Result, Error: message.Error,
		}
		if string(response.ID) == string(id) {
			result = response
			return io.EOF
		}
		return nil
	})
	if errors.Is(err, io.EOF) && len(result.ID) != 0 {
		return result, nil
	}
	if err != nil {
		return Response{}, err
	}
	return Response{}, errors.New("MCP SSE stream ended before response")
}

func scanSSE(
	reader io.Reader,
	bodyLimit int64,
	chunkLimit int,
	consume func(sseEvent) error,
) error {
	if bodyLimit <= 0 || chunkLimit <= 0 {
		return errors.New("MCP SSE limits must be positive")
	}
	limited := &io.LimitedReader{R: reader, N: bodyLimit + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, min(chunkLimit, 4096)), chunkLimit)
	eventName := ""
	var data bytes.Buffer
	emit := func() error {
		if data.Len() == 0 {
			eventName = ""
			return nil
		}
		payload := append([]byte(nil), data.Bytes()...)
		if len(payload) != 0 && payload[len(payload)-1] == '\n' {
			payload = payload[:len(payload)-1]
		}
		event := sseEvent{event: eventName, data: payload}
		eventName = ""
		data.Reset()
		return consume(event)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := emit(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventName = value
		case "data":
			if data.Len()+len(value)+1 > chunkLimit {
				return errors.New("MCP SSE event exceeds chunk limit")
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		if limited.N <= 0 {
			return errors.New("MCP SSE stream exceeds body limit")
		}
		return err
	}
	if limited.N <= 0 {
		return errors.New("MCP SSE stream exceeds body limit")
	}
	return emit()
}

func resolveEndpoint(baseURL, endpoint string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.New("parse MCP legacy base URL")
	}
	discovered, err := url.Parse(endpoint)
	if err != nil {
		return "", errors.New("parse MCP legacy endpoint")
	}
	resolved := base.ResolveReference(discovered)
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return "", errors.New("MCP legacy endpoint changed origin")
	}
	return resolved.String(), nil
}

func staleResponse(response Response) bool {
	if response.Error == nil {
		return false
	}
	return response.Error.Code == -32001
}

func isStaleSession(err error) bool {
	return errors.Is(err, errStaleSession) || errors.Is(err, errLegacyStreamClosed)
}

func withTimeoutIfSooner(
	parent context.Context,
	timeout time.Duration,
) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}
