// Package tracecontext owns W3C Trace Context propagation without making
// tracing an execution authority.
package tracecontext

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	HeaderTraceParent      = "traceparent"
	HeaderTraceState       = "tracestate"
	EnvironmentTraceParent = "TRACEPARENT"
	EnvironmentTraceState  = "TRACESTATE"
)

var (
	simpleStateKey = regexp.MustCompile(`^[a-z][a-z0-9_\-*/]{0,255}$`)
	tenantStateKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_\-*/]{0,240}@[a-z][a-z0-9_\-*/]{0,13}$`)
)

type Link struct {
	TraceID    string `json:"trace_id"`
	SpanID     string `json:"span_id"`
	TraceState string `json:"trace_state,omitempty"`
	TraceFlags byte   `json:"trace_flags,omitempty"`
}

type linkKey struct{}

func NewRoot(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parent, _ := Current(ctx)
	traceID := parent.TraceID
	if traceID == "" {
		var err error
		if traceID, err = randomHex(16); err != nil {
			return ctx, fmt.Errorf("generate trace id: %w", err)
		}
	}
	spanID, err := randomHex(8)
	if err != nil {
		return ctx, fmt.Errorf("generate span id: %w", err)
	}
	flags := parent.TraceFlags
	if parent.TraceID == "" {
		flags = 1
	}
	return context.WithValue(ctx, linkKey{}, Link{
		TraceID: traceID, SpanID: spanID,
		TraceState: parent.TraceState, TraceFlags: flags,
	}), nil
}

func Child(ctx context.Context) (context.Context, error) { return NewRoot(ctx) }

func Current(ctx context.Context) (Link, bool) {
	if ctx == nil {
		return Link{}, false
	}
	link, ok := ctx.Value(linkKey{}).(Link)
	if !ok || validateLink(link) != nil {
		return Link{}, false
	}
	return link, true
}

func WithLink(ctx context.Context, link Link, _ bool) (context.Context, error) {
	if err := validateLink(link); err != nil {
		return ctx, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, linkKey{}, link), nil
}

func InjectHTTP(ctx context.Context, header http.Header) bool {
	link, ok := Current(ctx)
	if header == nil || !ok {
		return false
	}
	header.Set(HeaderTraceParent,
		fmt.Sprintf("00-%s-%s-%02x", link.TraceID, link.SpanID, link.TraceFlags))
	if link.TraceState == "" {
		header.Del(HeaderTraceState)
	} else {
		header.Set(HeaderTraceState, link.TraceState)
	}
	return true
}

func ExtractHTTP(ctx context.Context, header http.Header) (context.Context, error) {
	parent := strings.TrimSpace(headerValue(header, HeaderTraceParent))
	if parent == "" {
		return ctx, nil
	}
	link, err := parseTraceParent(parent)
	if err != nil {
		return ctx, err
	}
	link.TraceState = strings.TrimSpace(headerValue(header, HeaderTraceState))
	if err := validateTraceState(link.TraceState); err != nil {
		return ctx, err
	}
	return WithLink(ctx, link, true)
}

func headerValue(header http.Header, name string) string {
	if header == nil {
		return ""
	}
	if value := header.Get(name); value != "" {
		return value
	}
	if values := header[name]; len(values) != 0 {
		return values[0]
	}
	return ""
}

func InjectMap(ctx context.Context, target map[string]string) bool {
	if target == nil {
		return false
	}
	header := make(http.Header, 2)
	if !InjectHTTP(ctx, header) {
		return false
	}
	target[HeaderTraceParent] = header.Get(HeaderTraceParent)
	if state := header.Get(HeaderTraceState); state != "" {
		target[HeaderTraceState] = state
	} else {
		delete(target, HeaderTraceState)
	}
	return true
}

func ExtractMap(ctx context.Context, source map[string]string) (context.Context, error) {
	header := make(http.Header, 2)
	if source != nil {
		header.Set(HeaderTraceParent, source[HeaderTraceParent])
		header.Set(HeaderTraceState, source[HeaderTraceState])
	}
	return ExtractHTTP(ctx, header)
}

func Environment(ctx context.Context) []string {
	values := make(map[string]string, 2)
	if !InjectMap(ctx, values) {
		return nil
	}
	result := []string{EnvironmentTraceParent + "=" + values[HeaderTraceParent]}
	if values[HeaderTraceState] != "" {
		result = append(result, EnvironmentTraceState+"="+values[HeaderTraceState])
	}
	return result
}

func parseTraceParent(value string) (Link, error) {
	if len(value) != 55 || value[2] != '-' || value[35] != '-' || value[52] != '-' ||
		value[:2] != "00" || !validID(value[3:35], 32) || !validID(value[36:52], 16) {
		return Link{}, errors.New("traceparent is invalid")
	}
	flags, err := strconv.ParseUint(value[53:], 16, 8)
	if err != nil || strings.ToLower(value[53:]) != value[53:] {
		return Link{}, errors.New("traceparent is invalid")
	}
	return Link{
		TraceID: value[3:35], SpanID: value[36:52], TraceFlags: byte(flags),
	}, nil
}

func validateLink(link Link) error {
	if !validID(link.TraceID, 32) {
		return errors.New("trace context trace id is invalid")
	}
	if !validID(link.SpanID, 16) {
		return errors.New("trace context span id is invalid")
	}
	if err := validateTraceState(link.TraceState); err != nil {
		return err
	}
	return nil
}

func validID(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return false
	}
	for _, item := range decoded {
		if item != 0 {
			return true
		}
	}
	return false
}

func validateTraceState(value string) error {
	if len(value) > 512 {
		return errors.New("tracestate exceeds 512 bytes")
	}
	if value == "" {
		return nil
	}
	members := strings.Split(value, ",")
	if len(members) > 32 {
		return errors.New("tracestate has too many members")
	}
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		key, item, ok := strings.Cut(strings.TrimSpace(member), "=")
		validKey := simpleStateKey.MatchString(key) || tenantStateKey.MatchString(key)
		validValue := item != "" && len(item) <= 256 && item[len(item)-1] != ' '
		for _, char := range item {
			validValue = validValue && char >= 0x20 && char <= 0x7e &&
				char != ',' && char != '='
		}
		if !ok || !validKey || !validValue {
			return errors.New("tracestate is invalid")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("tracestate contains duplicate keys")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func randomHex(bytes int) (string, error) {
	for {
		value := make([]byte, bytes)
		if _, err := rand.Read(value); err != nil {
			return "", err
		}
		encoded := hex.EncodeToString(value)
		if validID(encoded, bytes*2) {
			return encoded, nil
		}
	}
}
