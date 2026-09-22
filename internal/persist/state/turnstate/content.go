package turnstate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fwtllh-png/QCode/internal/persist/state/cas"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/durablecodec"
)

// Payload and response assembly are opaque schema fields: their bytes do not
// participate in field-delta decisions. References exist only in the storage
// encoding; kernel state and digest computation always see the original JSON.
func externalizeContent(ctx context.Context, tx *sql.Tx, raw []byte, kind, owner string) ([]byte, error) {
	return transformContent(raw, func(value json.RawMessage) (json.RawMessage, error) {
		data, err := durablecodec.Compress(value)
		if err != nil {
			return nil, err
		}
		id := cas.ID(data)
		if err := cas.PutTx(ctx, tx, id, data); err != nil {
			return nil, err
		}
		if err := cas.BindTx(ctx, tx, kind, owner, id); err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Content string `json:"$content"`
		}{id})
	}, false)
}

func hydrateContent(ctx context.Context, q cas.Queryer, raw []byte) ([]byte, error) {
	return transformContent(raw, func(value json.RawMessage) (json.RawMessage, error) {
		var ref struct {
			Content string `json:"$content"`
		}
		if err := json.Unmarshal(value, &ref); err != nil {
			return nil, err
		}
		if ref.Content == "" {
			return value, nil
		}
		data, err := cas.GetTx(ctx, q, ref.Content)
		if err != nil {
			return nil, err
		}
		return durablecodec.Decompress(data)
	}, true)
}

func transformContent(raw []byte, field func(json.RawMessage) (json.RawMessage, error), read bool) ([]byte, error) {
	if !json.Valid(raw) {
		return nil, errors.New("content encoding is invalid JSON")
	}
	var walk func(json.RawMessage) (json.RawMessage, error)
	walk = func(value json.RawMessage) (json.RawMessage, error) {
		trimmed := bytes.TrimSpace(value)
		if trimmed[0] != '{' && trimmed[0] != '[' {
			return value, nil
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		// Splice only changed values. Re-marshalling enclosing objects would
		// reorder RawMessage fields such as operation receipts, changing the
		// terminal digest even after all content references were hydrated.
		var result []byte
		offset := 0
		for decoder.More() {
			key := ""
			if trimmed[0] == '{' {
				token, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key = token.(string)
			}
			var child json.RawMessage
			if err := decoder.Decode(&child); err != nil {
				return nil, err
			}
			var next json.RawMessage
			var err error
			switch key {
			case "payload", "assembly":
				if bytes.Equal(child, []byte("null")) || (read && !isJSONObject(child)) {
					continue
				}
				next, err = field(child)
			case "session_delta":
				// This is a queryable context manifest, not kernel data.
				continue
			default:
				next, err = walk(child)
			}
			if err != nil {
				return nil, err
			}
			if bytes.Equal(child, next) {
				continue
			}
			end := int(decoder.InputOffset())
			result = append(result, value[offset:end-len(child)]...)
			result = append(result, next...)
			offset = end
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		if offset == 0 {
			return value, nil
		}
		return append(result, value[offset:]...), nil
	}
	return walk(raw)
}

func persistFactContent(ctx context.Context, tx *sql.Tx, fact turnkernel.DomainFact, raw []byte) ([]byte, error) {
	owner := fmt.Sprintf("%s:%d", fact.TurnID, fact.Sequence)
	if fact.State.Continuation != nil {
		if err := cas.BindTx(ctx, tx, "fact", owner, fact.State.Continuation.Handle); err != nil {
			return nil, err
		}
	}
	return externalizeContent(ctx, tx, raw, "fact", owner)
}

func bindTerminalContext(ctx context.Context, tx *sql.Tx, envelope turnkernel.TerminalEnvelope) error {
	if len(envelope.SessionDelta) == 0 {
		return nil
	}
	// Some standalone kernel users persist a legacy SessionDelta; only the
	// explicitly versioned context envelope has external content references.
	var value struct {
		Manifest *agentcontext.ContextManifest `json:"manifest"`
	}
	if err := json.Unmarshal(envelope.SessionDelta, &value); err != nil {
		return err
	}
	if value.Manifest == nil {
		return nil
	}
	return cas.BindTx(ctx, tx, "terminal", envelope.TurnID, value.Manifest.ContentIDs()...)
}
