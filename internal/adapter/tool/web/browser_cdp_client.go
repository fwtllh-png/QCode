package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Chrome's remote-debugging-pipe protocol uses inherited descriptors 3/4 and
// NUL-terminated JSON (Chromium DevToolsPipeHandler). Unlike a TCP debugger,
// these descriptors cannot be opened by a sibling loopback-capable command.
// A permanent reader dispatches auth events even while no tool call is active.
type cdpClient struct {
	reader     io.ReadCloser
	writer     io.WriteCloser
	ctx        context.Context
	cancel     context.CancelCauseFunc
	closeOnce  sync.Once
	writeMu    sync.Mutex
	mu         sync.Mutex
	nextID     int64
	pending    map[int64]chan cdpMessage
	proxyPort  uint16
	credential string
	// Only the reader accesses challenges. Entries end at the response pause.
	challenges map[string]bool
}

func newCDPClient(reader io.ReadCloser, writer io.WriteCloser, port uint16, credential string) *cdpClient {
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &cdpClient{reader: reader, writer: writer, ctx: ctx, cancel: cancel,
		pending: make(map[int64]chan cdpMessage), proxyPort: port, credential: credential,
		challenges: make(map[string]bool)}
	go c.readLoop()
	return c
}

func (c *cdpClient) close(err error) {
	c.closeOnce.Do(func() {
		c.cancel(err)
		_ = c.reader.Close()
		_ = c.writer.Close()
	})
}

func (c *cdpClient) Close() { c.close(errors.New("browser debugger closed")) }

func (c *cdpClient) send(ctx context.Context, session, method string, params any, response chan cdpMessage) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := context.Cause(c.ctx); err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.pending[id] = response
	c.mu.Unlock()
	message := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		message["sessionId"] = session
	}
	data, err := json.Marshal(message)
	if err == nil {
		// Canceling a blocked pipe write must also release its reader/callers.
		stop := context.AfterFunc(ctx, func() { c.close(ctx.Err()) })
		c.writeMu.Lock()
		_, err = c.writer.Write(append(data, 0))
		c.writeMu.Unlock()
		stop()
	}
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.close(err)
	}
	return id, err
}

func (c *cdpClient) call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	response := make(chan cdpMessage, 1)
	id, err := c.send(ctx, session, method, params, response)
	if err != nil {
		return nil, err
	}
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	select {
	case message := <-response:
		if message.Error != nil {
			return nil, fmt.Errorf("CDP %s failed (%d)", method, message.Error.Code)
		}
		return message.Result, nil
	case <-ctx.Done():
		c.close(ctx.Err())
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, context.Cause(c.ctx)
	}
}

func (c *cdpClient) readLoop() {
	reader := bufio.NewReader(c.reader)
	for {
		data, err := reader.ReadBytes(0)
		if err != nil {
			c.close(err)
			return
		}
		var message cdpMessage
		if err := json.Unmarshal(data[:len(data)-1], &message); err != nil {
			c.close(err)
			return
		}
		if message.ID != 0 {
			c.mu.Lock()
			response, found := c.pending[message.ID]
			delete(c.pending, message.ID)
			c.mu.Unlock()
			if response != nil {
				response <- message
			} else if found && message.Error != nil {
				c.close(fmt.Errorf("browser request interception failed (%d)", message.Error.Code))
				return
			}
			continue
		}
		if err := c.handleEvent(message); err != nil {
			c.close(err)
			return
		}
	}
}

func (c *cdpClient) handleEvent(message cdpMessage) error {
	if message.Method != "Fetch.requestPaused" && message.Method != "Fetch.authRequired" {
		return nil
	}
	var event struct {
		RequestID           string `json:"requestId"`
		ResponseStatusCode  int    `json:"responseStatusCode"`
		ResponseErrorReason string `json:"responseErrorReason"`
		AuthChallenge       struct {
			Source string `json:"source"`
			Origin string `json:"origin"`
			Scheme string `json:"scheme"`
			Realm  string `json:"realm"`
		} `json:"authChallenge"`
	}
	if err := json.Unmarshal(message.Params, &event); err != nil {
		return err
	}
	if event.RequestID == "" || message.SessionID == "" {
		return errors.New("browser interception event is incomplete")
	}
	key := message.SessionID + ":" + event.RequestID
	if message.Method == "Fetch.requestPaused" {
		if event.ResponseStatusCode != 0 || event.ResponseErrorReason != "" {
			delete(c.challenges, key)
		}
		_, err := c.send(c.ctx, message.SessionID, "Fetch.continueRequest", map[string]any{"requestId": event.RequestID}, nil)
		return err
	}
	answer := map[string]any{"response": "CancelAuth"}
	origin, err := url.Parse(event.AuthChallenge.Origin)
	if err == nil && origin.Scheme == "http" && origin.User == nil &&
		origin.Hostname() == "127.0.0.1" && origin.Port() == strconv.Itoa(int(c.proxyPort)) &&
		event.AuthChallenge.Source == "Proxy" && strings.EqualFold(event.AuthChallenge.Scheme, "basic") &&
		event.AuthChallenge.Realm == "qcode-process-proxy" && c.credential != "" && !c.challenges[key] {
		answer["response"] = "ProvideCredentials"
		answer["username"], answer["password"] = sandbox.ManagedProxyUser, c.credential
		c.challenges[key] = true
	}
	_, err = c.send(c.ctx, message.SessionID, "Fetch.continueWithAuth", map[string]any{
		"requestId": event.RequestID, "authChallengeResponse": answer,
	}, nil)
	return err
}
