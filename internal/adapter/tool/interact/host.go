// Package interact provides mid-turn user input host and related tools.
package interact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultTTL = 10 * time.Minute

type Request struct {
	RequestID string
	CallID    string
	Tool      string
	Prompt    string
	Options   []string
	ExpiresAt time.Time
}

type Reply struct {
	RequestID string
	Answer    string
	Values    map[string]string
}

type HostUnavailableError struct{}

func (HostUnavailableError) Error() string {
	return "input_host_unavailable: input host is not connected"
}

type Host struct {
	mu        sync.Mutex
	ttl       time.Duration
	now       func() time.Time
	emit      func(context.Context, Request) error
	expiry    func(Request)
	pending   map[string]*pending
	recovered map[string]Request
	restore   func(Request) error
}

type pending struct {
	callID string
	reply  chan Reply
	resume chan struct{}
}

func NewHost(ttl time.Duration) *Host {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Host{
		ttl: ttl, now: time.Now, pending: make(map[string]*pending),
		recovered: make(map[string]Request),
	}
}

func (h *Host) SetEmitter(emit func(context.Context, Request) error) {
	h.mu.Lock()
	h.emit = emit
	h.mu.Unlock()
}

// SetExpiryHandler registers the cleanup for an input wait that ends
// without a user reply (TTL expiry or context cancellation). The engine
// resolves the kernel input wait there so the turn keeps a legal phase
// instead of failing on the tool result that follows.
func (h *Host) SetExpiryHandler(handler func(Request)) {
	h.mu.Lock()
	h.expiry = handler
	h.mu.Unlock()
}

func (h *Host) notifyExpiry(request Request) {
	h.mu.Lock()
	expiry := h.expiry
	h.mu.Unlock()
	if expiry != nil {
		expiry(request)
	}
}

func (h *Host) Wait(
	ctx context.Context, callID, prompt string, options []string,
) (Reply, error) {
	h.mu.Lock()
	emit := h.emit
	ttl := h.ttl
	now := h.now
	h.mu.Unlock()
	if emit == nil {
		return Reply{}, HostUnavailableError{}
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Reply{}, errors.New("prompt is required")
	}
	requestID := randomID("input_")
	expiresAt := now().Add(ttl)
	h.mu.Lock()
	recovered, recovering := h.recovered[callID]
	if recovering {
		delete(h.recovered, callID)
		requestID = recovered.RequestID
		expiresAt = recovered.ExpiresAt
	}
	h.mu.Unlock()
	entry := &pending{
		callID: callID, reply: make(chan Reply, 1), resume: make(chan struct{}),
	}
	h.mu.Lock()
	h.pending[requestID] = entry
	h.mu.Unlock()
	defer h.finish(requestID)

	req := Request{
		RequestID: requestID, CallID: callID, Tool: "request_user_input",
		Prompt: prompt, Options: append([]string(nil), options...), ExpiresAt: expiresAt,
	}
	if recovering {
		h.mu.Lock()
		restore := h.restore
		h.mu.Unlock()
		if restore == nil {
			return Reply{}, errors.New("input recovery handler is unavailable")
		}
		if err := restore(req); err != nil {
			return Reply{}, fmt.Errorf("restore input wait: %w", err)
		}
	} else {
		if err := emit(ctx, req); err != nil {
			return Reply{}, fmt.Errorf("emit input request: %w", err)
		}
	}
	timer := time.NewTimer(max(time.Millisecond, expiresAt.Sub(now())))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		h.notifyExpiry(req)
		return Reply{}, ctx.Err()
	case <-timer.C:
		h.notifyExpiry(req)
		return Reply{}, errors.New("input request expired")
	case reply := <-entry.reply:
		select {
		case <-ctx.Done():
			h.notifyExpiry(req)
			return Reply{}, ctx.Err()
		case <-entry.resume:
		}
		reply.RequestID = requestID
		return reply, nil
	}
}

func (h *Host) RestoreRequest(request Request) error {
	if request.RequestID == "" || request.CallID == "" ||
		request.ExpiresAt.IsZero() {
		return errors.New("restored input request is incomplete")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recovered[request.CallID] = request
	return nil
}

// SetRecoveryHandler restores turn-local wait bookkeeping without re-emitting
// an input.required event that is already durable.
func (h *Host) SetRecoveryHandler(handler func(Request) error) {
	h.mu.Lock()
	h.restore = handler
	h.mu.Unlock()
}

func (h *Host) StageReply(reply Reply) error {
	if reply.RequestID == "" {
		return errors.New("input request id is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.pending[reply.RequestID]
	if entry == nil {
		return errors.New("input request is unknown")
	}
	select {
	case entry.reply <- reply:
		return nil
	default:
		return errors.New("input reply is duplicate or late")
	}
}

func (h *Host) Resume(requestID string) error {
	h.mu.Lock()
	entry := h.pending[requestID]
	h.mu.Unlock()
	if entry == nil {
		return errors.New("input request is unknown")
	}
	select {
	case <-entry.resume:
	default:
		close(entry.resume)
	}
	return nil
}

func (h *Host) Reply(reply Reply) error {
	if err := h.StageReply(reply); err != nil {
		return err
	}
	return h.Resume(reply.RequestID)
}

func (h *Host) finish(requestID string) {
	h.mu.Lock()
	delete(h.pending, requestID)
	h.mu.Unlock()
}

func randomID(prefix string) string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return prefix + hex.EncodeToString(buf[:])
}
