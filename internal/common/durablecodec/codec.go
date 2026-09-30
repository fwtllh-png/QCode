// Package durablecodec provides bounded deterministic encoding for large
// immutable runtime snapshots.
package durablecodec

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
)

const (
	SchemaVersion   = 1
	Encoding        = "gzip+base64"
	MaxDecodedBytes = 64 << 20
)

type JSONEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Encoding      string `json:"encoding"`
	BaseRevision  uint64 `json:"base_revision"`
	Data          string `json:"data"`
}

func EncodeJSON(raw []byte, baseRevision uint64) ([]byte, error) {
	if err := validateBaseRevision(raw, baseRevision); err != nil {
		return nil, err
	}
	compressed, err := Compress(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(JSONEnvelope{
		SchemaVersion: SchemaVersion,
		Encoding:      Encoding,
		BaseRevision:  baseRevision,
		Data:          base64.StdEncoding.EncodeToString(compressed),
	})
}

func DecodeJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var envelope JSONEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Encoding == "" {
		return append([]byte(nil), raw...), nil
	}
	if envelope.SchemaVersion != SchemaVersion ||
		envelope.Encoding != Encoding ||
		envelope.Data == "" ||
		len(envelope.Data) > MaxDecodedBytes*2 {
		return nil, errors.New("durable JSON envelope is invalid")
	}
	compressed, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil {
		return nil, errors.New("durable JSON payload is invalid")
	}
	decoded, err := Decompress(compressed)
	if err != nil {
		return nil, err
	}
	if err := validateBaseRevision(decoded, envelope.BaseRevision); err != nil {
		return nil, err
	}
	return decoded, nil
}

func Compress(raw []byte) ([]byte, error) {
	var compressed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func Decompress(raw []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("durable compression is invalid")
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, MaxDecodedBytes+1))
	if err != nil || len(decoded) > MaxDecodedBytes {
		return nil, errors.New("durable payload exceeds its decode budget")
	}
	return decoded, nil
}

func IsCompressed(raw []byte) bool {
	return len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b
}

func validateBaseRevision(raw []byte, expected uint64) error {
	var identity struct {
		BaseRevision uint64 `json:"base_revision"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return errors.New("durable JSON identity is invalid")
	}
	if identity.BaseRevision != expected {
		return errors.New("durable JSON revision is invalid")
	}
	return nil
}
