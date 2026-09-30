package tracecontext

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestW3CInjectExtractAndChild(t *testing.T) {
	root, err := NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rootLink, ok := Current(root)
	if !ok {
		t.Fatal("root trace context is missing")
	}
	child, err := Child(root)
	if err != nil {
		t.Fatal(err)
	}
	childLink, ok := Current(child)
	if !ok || childLink.TraceID != rootLink.TraceID ||
		childLink.SpanID == rootLink.SpanID {
		t.Fatalf("root=%+v child=%+v", rootLink, childLink)
	}
	header := make(http.Header)
	if !InjectHTTP(child, header) {
		t.Fatal("trace context was not injected")
	}
	extracted, err := ExtractHTTP(context.Background(), header)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := Current(extracted)
	if !ok || got.TraceID != childLink.TraceID ||
		got.SpanID != childLink.SpanID {
		t.Fatalf("injected=%+v extracted=%+v", childLink, got)
	}
}

func TestMalformedTraceContextFailsOpen(t *testing.T) {
	original := context.WithValue(t.Context(), contextKey{}, "kept")
	header := http.Header{
		HeaderTraceParent: []string{"00-invalid-invalid-01"},
	}
	extracted, err := ExtractHTTP(original, header)
	if err == nil || extracted.Value(contextKey{}) != "kept" {
		t.Fatalf("context=%v error=%v", extracted, err)
	}
	if _, ok := Current(extracted); ok {
		t.Fatal("malformed traceparent became a span context")
	}

	root, err := NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	header = make(http.Header)
	InjectHTTP(root, header)
	header.Set(HeaderTraceState, strings.Repeat("x", 513))
	if extracted, err = ExtractHTTP(original, header); err == nil ||
		extracted.Value(contextKey{}) != "kept" {
		t.Fatalf("tracestate context=%v error=%v", extracted, err)
	}
}

func TestEnvironmentRequiresValidContext(t *testing.T) {
	if values := Environment(t.Context()); len(values) != 0 {
		t.Fatalf("untraced environment = %v", values)
	}
	root, err := NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	values := Environment(root)
	if len(values) == 0 ||
		!strings.HasPrefix(values[0], EnvironmentTraceParent+"=") {
		t.Fatalf("environment = %v", values)
	}
}

func TestTraceContextRejectsInvalidIdentifiersAndTraceState(t *testing.T) {
	validParent := "00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"
	tests := []http.Header{
		{HeaderTraceParent: []string{"01-" + strings.Repeat("1", 32) + "-" + strings.Repeat("2", 16) + "-01"}},
		{HeaderTraceParent: []string{"00-" + strings.Repeat("0", 32) + "-" + strings.Repeat("2", 16) + "-01"}},
		{HeaderTraceParent: []string{"00-" + strings.Repeat("1", 32) + "-" + strings.Repeat("0", 16) + "-01"}},
		{HeaderTraceParent: []string{validParent}, HeaderTraceState: []string{"vendor=value,vendor=other"}},
		{HeaderTraceParent: []string{validParent}, HeaderTraceState: []string{"Vendor=value"}},
	}
	for _, header := range tests {
		if _, err := ExtractHTTP(t.Context(), header); err == nil {
			t.Fatalf("accepted invalid trace context: %v", header)
		}
	}
}

func BenchmarkSO5W3CInjectExtract(b *testing.B) {
	root, err := NewRoot(b.Context())
	if err != nil {
		b.Fatal(err)
	}
	header := make(http.Header)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		clear(header)
		if !InjectHTTP(root, header) {
			b.Fatal("trace context was not injected")
		}
		if _, err := ExtractHTTP(context.Background(), header); err != nil {
			b.Fatal(err)
		}
	}
}

type contextKey struct{}
