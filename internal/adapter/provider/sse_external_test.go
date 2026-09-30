package provider_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestFaultInjectionSSEDisconnect(t *testing.T) {
	reader := io.MultiReader(
		bytes.NewReader([]byte("data: {\"text\":\"hel\"}\n\n")),
		&errReader{err: io.ErrUnexpectedEOF},
	)
	decoder := provider.NewSSEDecoder(reader)
	record, err := decoder.Next()
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	if record.Data != `{"text":"hel"}` {
		t.Fatalf("data = %q", record.Data)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("disconnect error = %v, want ErrUnexpectedEOF", err)
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }
