package process

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestHeadTailBufferPreservesSmallOutputExactly(t *testing.T) {
	buffer := newHeadTailBuffer(10)
	for _, value := range []string{"abc", "def", "ghij"} {
		if _, err := buffer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.String(); got != "abcdefghij" {
		t.Fatalf("output = %q", got)
	}
	if receipt := buffer.Receipt(); receipt != (StreamReceipt{
		TotalBytes: 10, RetainedBytes: 10,
	}) {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestHeadTailBufferAllocatesLazilyForSmallOutput(t *testing.T) {
	buffer := newHeadTailBuffer(DefaultOutputLimitBytes)
	if _, err := buffer.Write([]byte("small")); err != nil {
		t.Fatal(err)
	}
	if allocated := cap(buffer.head) + cap(buffer.tail); allocated > 64 {
		t.Fatalf("small output allocated %d bytes", allocated)
	}
}

func TestHeadTailBufferRetainsEdgesAndReportsOmission(t *testing.T) {
	buffer := newHeadTailBuffer(10)
	if _, err := buffer.Write([]byte("abcdefghijklmnopqrstuvwxyz")); err != nil {
		t.Fatal(err)
	}
	output := buffer.String()
	if !strings.HasPrefix(output, "abcde\n... [output truncated: 16 bytes omitted] ...\n") ||
		!strings.HasSuffix(output, "vwxyz") {
		t.Fatalf("output = %q", output)
	}
	if receipt := buffer.Receipt(); receipt != (StreamReceipt{
		TotalBytes: 26, RetainedBytes: 10, OmittedBytes: 16,
	}) {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestHeadTailBufferBoundsOneGiBSyntheticStream(t *testing.T) {
	const (
		total = uint64(1 << 30)
		limit = 1 << 20
	)
	buffer := newHeadTailBuffer(limit)
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for written := uint64(0); written < total; written += uint64(len(chunk)) {
		if _, err := buffer.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	receipt := buffer.Receipt()
	if receipt.TotalBytes != total ||
		receipt.RetainedBytes != limit ||
		receipt.OmittedBytes != total-limit {
		t.Fatalf("receipt = %+v", receipt)
	}
	if output := buffer.String(); len(output) > limit+128 {
		t.Fatalf("retained output bytes = %d", len(output))
	}
	if cap(buffer.head)+cap(buffer.tail) != limit {
		t.Fatalf(
			"collector capacity = %d, want %d",
			cap(buffer.head)+cap(buffer.tail),
			limit,
		)
	}
}

func TestArchiveStateRecordsFirstConcurrentFailure(t *testing.T) {
	archiveErr := errors.New("archive unavailable")
	archive := &archiveState{append: func(Chunk) error { return archiveErr }}
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			archive.write(Chunk{Data: []byte("x")})
		}()
	}
	wait.Wait()
	if got := archive.errorString(); got != archiveErr.Error() {
		t.Fatalf("archive error = %q", got)
	}
}
