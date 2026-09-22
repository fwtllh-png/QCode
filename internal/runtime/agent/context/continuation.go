package agentcontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/durablecodec"
)

// ContinuationVersion is the schema version of TurnContinuation records.
const ContinuationVersion = 2

// ErrTurnContinuationUnavailable reports that a referenced continuation
// record cannot be read back from the content store.
var ErrTurnContinuationUnavailable = errors.New(
	"turn continuation content is unavailable",
)

// TurnContinuation is the durable conversation snapshot of a running Turn.
// The engine stores it at semantic acceptance boundaries (an accepted model
// sample, an accepted tool-result batch) so a restarted process can rebuild
// the in-turn conversation that never reached a terminal SessionDelta.
// Content is written to the context CAS first; the kernel commits only the
// content reference afterwards, so a committed cursor always resolves.
type TurnContinuation struct {
	Version           int                `json:"version"`
	TurnID            string             `json:"turn_id"`
	Sequence          uint64             `json:"sequence"`
	TurnNumber        uint64             `json:"turn_number"`
	SessionRevision   uint64             `json:"session_revision"`
	StateEpoch        uint64             `json:"state_epoch"`
	SampleID          string             `json:"sample_id,omitempty"`
	Step              int                `json:"step"`
	WorkspaceIdentity string             `json:"workspace_identity"`
	ProfileRevision   uint64             `json:"profile_revision"`
	Provider          string             `json:"provider"`
	Model             string             `json:"model"`
	Messages          []provider.Message `json:"messages"`
}

type storedContinuation struct {
	TurnContinuation
	MessageRefs []ContentRef `json:"message_refs"`
}

// ContinuationEnvironment is the execution identity a restored Turn must
// still match before its continuation conversation may be reused.
type ContinuationEnvironment struct {
	WorkspaceIdentity string
	ProfileRevision   uint64
	Provider          string
	Model             string
}

func (c TurnContinuation) Validate() error {
	if c.Version != ContinuationVersion {
		return fmt.Errorf(
			"turn continuation version %d is unsupported",
			c.Version,
		)
	}
	if strings.TrimSpace(c.TurnID) == "" || c.Sequence == 0 {
		return errors.New("turn continuation identity is incomplete")
	}
	if c.TurnNumber == 0 || c.ProfileRevision == 0 {
		return errors.New("turn continuation anchors are incomplete")
	}
	if strings.TrimSpace(c.WorkspaceIdentity) == "" ||
		strings.TrimSpace(c.Provider) == "" ||
		strings.TrimSpace(c.Model) == "" {
		return errors.New("turn continuation environment is incomplete")
	}
	if len(c.Messages) == 0 {
		return errors.New("turn continuation has no accepted messages")
	}
	return nil
}

// EnvironmentDrift returns one human-readable reason per field whose value
// changed between the stored continuation and the current environment. A
// non-empty result means the record must not seed the restored conversation.
func (c TurnContinuation) EnvironmentDrift(
	env ContinuationEnvironment,
) []string {
	var drift []string
	if strings.TrimSpace(env.WorkspaceIdentity) != "" &&
		env.WorkspaceIdentity != c.WorkspaceIdentity {
		drift = append(
			drift,
			fmt.Sprintf(
				"workspace identity changed from %q to %q",
				c.WorkspaceIdentity,
				env.WorkspaceIdentity,
			),
		)
	}
	if env.ProfileRevision != 0 &&
		env.ProfileRevision != c.ProfileRevision {
		drift = append(
			drift,
			fmt.Sprintf(
				"profile revision changed from %d to %d",
				c.ProfileRevision,
				env.ProfileRevision,
			),
		)
	}
	if strings.TrimSpace(env.Provider) != "" && env.Provider != c.Provider {
		drift = append(
			drift,
			fmt.Sprintf(
				"provider changed from %q to %q",
				c.Provider,
				env.Provider,
			),
		)
	}
	if strings.TrimSpace(env.Model) != "" && env.Model != c.Model {
		drift = append(
			drift,
			fmt.Sprintf(
				"model changed from %q to %q",
				c.Model,
				env.Model,
			),
		)
	}
	return drift
}

// StoreTurnContinuation writes the record content and returns the content
// reference the caller commits as the continuation cursor. The write happens
// before any fact claims it, so a committed cursor never points at missing
// content.
func StoreTurnContinuation(
	ctx context.Context,
	store BlobStore,
	record TurnContinuation,
) (ContentRef, error) {
	if store == nil {
		return ContentRef{}, errors.New("turn continuation store is nil")
	}
	if err := record.Validate(); err != nil {
		return ContentRef{}, err
	}
	stored := storedContinuation{TurnContinuation: record}
	stored.Messages = nil
	var children []string
	for _, message := range record.Messages {
		ref, err := stageValue(ctx, store, "continuation-message", message)
		if err != nil {
			return ContentRef{}, err
		}
		stored.MessageRefs = append(stored.MessageRefs, ref)
		children = append(children, ref.Handle)
	}
	ref, err := stageValue(ctx, store, "turn-continuation", stored)
	if err != nil {
		return ContentRef{}, err
	}
	if graph, ok := store.(interface {
		LinkContent(context.Context, string, []string) error
	}); ok {
		if err := graph.LinkContent(ctx, ref.Handle, children); err != nil {
			return ContentRef{}, err
		}
	}
	return ref, nil
}

// LoadTurnContinuation reads a cursor-referenced record back and validates
// its content against the committed digest. The cursor carries no byte
// length, so the digest alone gates integrity.
func LoadTurnContinuation(
	ctx context.Context,
	store BlobStore,
	handle string,
	digest string,
) (TurnContinuation, error) {
	if store == nil {
		return TurnContinuation{}, errors.New("turn continuation store is nil")
	}
	if strings.TrimSpace(handle) == "" || strings.TrimSpace(digest) == "" {
		return TurnContinuation{}, errors.New(
			"turn continuation cursor is incomplete",
		)
	}
	raw, err := store.Get(ctx, handle)
	if err != nil {
		return TurnContinuation{}, fmt.Errorf(
			"%w: %s",
			ErrTurnContinuationUnavailable,
			err,
		)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != digest {
		return TurnContinuation{}, fmt.Errorf(
			"%w: content digest mismatch",
			ErrTurnContinuationUnavailable,
		)
	}
	decoded, err := durablecodec.Decompress(raw)
	if err != nil {
		return TurnContinuation{}, fmt.Errorf(
			"%w: %s",
			ErrTurnContinuationUnavailable,
			err,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var stored storedContinuation
	if err := decoder.Decode(&stored); err != nil {
		return TurnContinuation{}, fmt.Errorf(
			"%w: %s",
			ErrTurnContinuationUnavailable,
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return TurnContinuation{}, fmt.Errorf(
				"%w: trailing JSON",
				ErrTurnContinuationUnavailable,
			)
		}
		return TurnContinuation{}, fmt.Errorf(
			"%w: %s",
			ErrTurnContinuationUnavailable,
			err,
		)
	}
	record := stored.TurnContinuation
	if len(record.Messages) != 0 || len(stored.MessageRefs) == 0 {
		return TurnContinuation{}, errors.New("turn continuation message references are invalid")
	}
	for _, ref := range stored.MessageRefs {
		var message provider.Message
		if err := readValue(ctx, store, ref, &message); err != nil {
			return TurnContinuation{}, fmt.Errorf("%w: %s", ErrTurnContinuationUnavailable, err)
		}
		record.Messages = append(record.Messages, message)
	}
	if err := record.Validate(); err != nil {
		return TurnContinuation{}, err
	}
	record.Messages = CloneMessages(record.Messages)
	return record, nil
}
