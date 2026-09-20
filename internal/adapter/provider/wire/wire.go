package wire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

type AuthStyle string

const (
	AuthBearer AuthStyle = "bearer"
)

type PreparedCall struct {
	Method, Path string
	Body         []byte
	Headers      http.Header
	Auth         AuthStyle
	Adapter      model.AdapterID
	Protocol     model.WireProtocol
	Projection   provider.ProjectionReceipt
}
type HTTPFailure struct {
	ProviderID string
	Status     int
	Header     http.Header
	Body       string
}
type Adapter interface {
	ID() model.AdapterID
	Supports(model.WireProtocol) bool
	Prepare(provider.ModelRequest) (PreparedCall, error)
	OpenStream(io.ReadCloser, PreparedCall) (provider.Stream, error)
	ClassifyHTTP(HTTPFailure) error
}
type Transport interface {
	Execute(context.Context, provider.ModelRequest, PreparedCall, Adapter) (provider.Stream, error)
}
type Socket interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}
type SessionAttempt interface {
	Dial(string) (Socket, context.CancelFunc, error)
	ProviderRequest()
	IdleTimeout() time.Duration
	Wrap(provider.Stream, provider.TransportMetadata) provider.Stream
	Close()
}
type SessionTransport interface {
	BeginSession(context.Context, model.ReadyRoute, PreparedCall) (SessionAttempt, error)
}
type SessionAdapter interface {
	TrySession(context.Context, provider.ModelRequest, PreparedCall, SessionTransport) (provider.Stream, bool, error)
}

func MetadataWithProjection(
	logical []byte,
	payload []byte,
	incremental bool,
	projection provider.ProjectionReceipt,
) provider.TransportMetadata {
	if projection.Mode == "" {
		projection = provider.CompleteProjection(
			provider.ProjectionContext{},
			provider.ProjectionFallbackCompleteRequest,
		)
	}
	if incremental {
		projection.Mode = provider.ProjectionModeIncrementalSession
		projection.IncrementalEligible = true
		projection.FallbackReason = ""
	}
	return provider.TransportMetadata{
		RequestBytes: uint64(len(payload)), LogicalRequestDigest: Digest(logical),
		TransportPayloadDigest: Digest(payload), Incremental: incremental,
		Projection: projection,
	}
}
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
