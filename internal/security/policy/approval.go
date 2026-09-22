package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

type ApprovalScope string

const (
	ApprovalOnce    ApprovalScope = "once"
	ApprovalSession ApprovalScope = "session"
	ApprovalAlways  ApprovalScope = "always"
)

type ApprovalRequest struct {
	RequestID       string          `json:"request_id"`
	CallID          string          `json:"call_id"`
	Tool            string          `json:"tool"`
	Arguments       json.RawMessage `json:"arguments"`
	ArgumentsDigest string          `json:"arguments_digest"`
	Resources       []tool.Resource `json:"resources"`
	Scope           ApprovalScope   `json:"scope"`
	Fingerprint     string          `json:"fingerprint"`
	ExpiresAt       time.Time       `json:"expires_at"`
	Grant           *Grant          `json:"grant,omitempty"`
}

type approvalEntry struct {
	expiresAt       time.Time
	once            bool
	baseFingerprint string
	sequence        uint64
	// prefix and scope carry the reusable shell grant's static argv and
	// scope fingerprint so later commands extending the approved argv
	// match without a fresh approval.
	prefix []string
	scope  string
}

type ApprovalCache struct {
	mu        sync.Mutex
	entries   map[string]approvalEntry
	grantKeys map[string]approvalEntry
	limit     int
	next      uint64
}

const defaultApprovalCacheLimit = 1024

func NewApprovalCache() *ApprovalCache {
	return NewApprovalCacheWithLimit(defaultApprovalCacheLimit)
}

func NewApprovalCacheWithLimit(limit int) *ApprovalCache {
	if limit < 1 {
		limit = 1
	}
	return &ApprovalCache{
		entries: make(map[string]approvalEntry), grantKeys: make(map[string]approvalEntry), limit: limit,
	}
}

func NewApprovalRequest(invocation Invocation, expiresAt time.Time) (ApprovalRequest, error) {
	return NewApprovalRequestForScope(invocation, ApprovalOnce, expiresAt)
}

func NewApprovalRequestForScope(
	invocation Invocation, scope ApprovalScope, expiresAt time.Time,
) (ApprovalRequest, error) {
	arguments, err := canonicalJSON(invocation.Arguments)
	if err != nil {
		return ApprovalRequest{}, err
	}
	resources := append([]tool.Resource(nil), invocation.Resources...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Key() < resources[j].Key() })
	resources = compactResources(resources)
	argumentsHash := sha256.Sum256(arguments)
	request := ApprovalRequest{
		CallID: invocation.CallID, Tool: invocation.Tool, Arguments: arguments,
		ArgumentsDigest: hex.EncodeToString(argumentsHash[:]),
		Resources:       resources, Scope: scope, ExpiresAt: expiresAt,
	}
	if grant, ok := GrantForInvocation(invocation); ok {
		request.Grant = &grant
	}
	request.Fingerprint = approvalFingerprint(request)
	return request, nil
}

func (c *ApprovalCache) Add(request ApprovalRequest, scope ApprovalScope) error {
	if c == nil {
		return errors.New("approval cache is required")
	}
	if request.Fingerprint == "" || request.Fingerprint != approvalFingerprint(request) {
		return errors.New("approval fingerprint does not match request")
	}
	if scope != ApprovalOnce && scope != ApprovalSession && scope != ApprovalAlways {
		return errors.New("approval scope must be once, session, or always")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]approvalEntry)
	}
	if c.grantKeys == nil {
		c.grantKeys = make(map[string]approvalEntry)
	}
	if c.limit < 1 {
		c.limit = defaultApprovalCacheLimit
	}
	c.next++
	request.Scope = scope
	request.Fingerprint = approvalFingerprint(request)
	entry := approvalEntry{
		expiresAt: request.ExpiresAt, once: scope == ApprovalOnce,
		baseFingerprint: approvalBaseFingerprint(request), sequence: c.next,
	}
	c.entries[request.Fingerprint] = entry
	if scope == ApprovalSession || scope == ApprovalAlways {
		if request.Grant == nil || request.Grant.Key == "" {
			return errors.New("reusable approval requires a typed grant")
		}
		c.grantKeys[request.Grant.Key] = approvalEntry{
			expiresAt: request.ExpiresAt, sequence: c.next,
			prefix: request.Grant.Prefix, scope: request.Grant.scope,
		}
	}
	pruneApprovalEntries(c.entries, c.limit)
	pruneApprovalEntries(c.grantKeys, c.limit)
	return nil
}

func pruneApprovalEntries(entries map[string]approvalEntry, limit int) {
	for len(entries) > limit {
		var oldestKey string
		var oldestSequence uint64
		for key, value := range entries {
			if oldestKey == "" || value.sequence < oldestSequence {
				oldestKey, oldestSequence = key, value.sequence
			}
		}
		delete(entries, oldestKey)
	}
}

func (c *ApprovalCache) MatchInvocation(invocation Invocation, now time.Time) bool {
	if c == nil {
		return false
	}
	if c.matchInvocationExact(invocation, now) {
		return true
	}
	return c.matchGrant(invocation, now)
}

func (c *ApprovalCache) matchGrant(invocation Invocation, now time.Time) bool {
	grant, ok := GrantForInvocation(invocation)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.grantKeys == nil {
		return false
	}
	entry, exists := c.grantKeys[grant.Key]
	if exists {
		if !approvalExpired(entry, now) {
			return true
		}
		delete(c.grantKeys, grant.Key)
	}
	if len(grant.Prefix) == 0 {
		return false
	}
	// Prefix matching: an approved single-segment command covers later
	// commands that extend its argv in the same scope (cwd and resources).
	// Composite candidates never carry a prefix, so an approved prefix can
	// never absorb a piped or chained command.
	for key, entry := range c.grantKeys {
		if approvalExpired(entry, now) {
			delete(c.grantKeys, key)
			continue
		}
		if len(entry.prefix) != 0 && entry.scope == grant.scope &&
			argvPrefix(grant.Prefix, entry.prefix) {
			return true
		}
	}
	return false
}

func (c *ApprovalCache) matchInvocationExact(invocation Invocation, now time.Time) bool {
	request, err := NewApprovalRequestForScope(invocation, ApprovalOnce, time.Time{})
	if err != nil {
		return false
	}
	base := approvalBaseFingerprint(request)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if approvalExpired(entry, now) {
			delete(c.entries, key)
			continue
		}
		if entry.baseFingerprint != base {
			continue
		}
		if entry.once {
			delete(c.entries, key)
		}
		return true
	}
	return false
}

func (c *ApprovalCache) Purge(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if approvalExpired(entry, now) {
			delete(c.entries, key)
		}
	}
	for key, entry := range c.grantKeys {
		if approvalExpired(entry, now) {
			delete(c.grantKeys, key)
		}
	}
}

func approvalExpired(entry approvalEntry, now time.Time) bool {
	return !entry.expiresAt.IsZero() && !entry.expiresAt.After(now)
}

func approvalFingerprint(request ApprovalRequest) string {
	hash := sha256.New()
	writeFingerprintField(hash, approvalBaseFingerprint(request))
	writeFingerprintField(hash, string(request.Scope))
	writeFingerprintField(hash, request.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return hex.EncodeToString(hash.Sum(nil))
}

func approvalBaseFingerprint(request ApprovalRequest) string {
	hash := sha256.New()
	writeFingerprintField(hash, request.Tool)
	arguments, err := canonicalJSON(request.Arguments)
	if err != nil {
		return ""
	}
	writeFingerprintField(hash, string(arguments))
	resources := append([]tool.Resource(nil), request.Resources...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].Key() < resources[j].Key() })
	for _, resource := range compactResources(resources) {
		encoded, _ := json.Marshal(resource)
		writeFingerprintField(hash, string(encoded))
	}
	if request.Grant != nil {
		writeFingerprintField(hash, request.Grant.Kind)
		writeFingerprintField(hash, request.Grant.Key)
		writeFingerprintField(hash, request.Grant.Summary)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func compactResources(values []tool.Resource) []tool.Resource {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1].Key() != value.Key() {
			result = append(result, value)
		}
	}
	return result
}

type byteWriter interface {
	Write([]byte) (int, error)
}

func writeFingerprintField(writer byteWriter, value string) {
	var length [8]byte
	size := uint64(len(value))
	for index := range length {
		length[7-index] = byte(size)
		size >>= 8
	}
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("multiple JSON values")
	}
	return json.Marshal(value)
}
