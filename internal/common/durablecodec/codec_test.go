package durablecodec

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestJSONEnvelopeCompressesAndReadsLegacyJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"base_revision": 479,
		"history":       bytes.Repeat([]byte("say hello\nhello\n"), 960),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeJSON(raw, 479)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)*5 >= len(raw) {
		t.Fatalf("compressed JSON = %d bytes, raw = %d", len(encoded), len(raw))
	}
	var envelope JSONEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.BaseRevision != 479 {
		t.Fatalf("base revision = %d", envelope.BaseRevision)
	}
	for name, value := range map[string][]byte{
		"compressed": encoded,
		"legacy":     raw,
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := DecodeJSON(value)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, raw) {
				t.Fatal("decoded JSON differs from source")
			}
		})
	}
	if _, err := DecodeJSON([]byte(
		`{"schema_version":1,"encoding":"unknown","data":"eA=="}`,
	)); err == nil {
		t.Fatal("unknown durable JSON encoding was accepted")
	}
	if decoded, err := DecodeJSON(nil); err != nil || decoded != nil {
		t.Fatalf("nil durable JSON = %q, %v", decoded, err)
	}
	if _, err := EncodeJSON(raw, 478); err == nil {
		t.Fatal("mismatched durable JSON revision was accepted")
	}
}

func TestCompressionIsDeterministicAndBounded(t *testing.T) {
	raw := bytes.Repeat([]byte("checkpoint history\n"), 4096)
	first, err := Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !IsCompressed(first) {
		t.Fatal("durable compression is not deterministic")
	}
	decoded, err := Decompress(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("decompressed bytes differ from source")
	}
}
