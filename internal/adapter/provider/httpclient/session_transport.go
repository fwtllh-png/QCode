package httpclient

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/observability/tracecontext"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

type sessionAttempt struct {
	client       *Client
	call         providerwire.PreparedCall
	credential   string
	ctx          context.Context
	cancel       context.CancelFunc
	traceHeader  http.Header
	requestID    string
	transferred  bool
	cooldownWait time.Duration
}

func (c *Client) BeginSession(
	ctx context.Context,
	route model.ReadyRoute,
	call providerwire.PreparedCall,
) (providerwire.SessionAttempt, error) {
	requestContext, cancel, credential, cooldownWait, err := c.begin(ctx, route)
	if err != nil {
		return nil, err
	}
	traceHeader := make(http.Header)
	tracecontext.InjectHTTP(ctx, traceHeader)
	return &sessionAttempt{
		client: c, call: call, credential: credential,
		ctx: requestContext, cancel: cancel,
		traceHeader:  traceHeader,
		cooldownWait: cooldownWait,
	}, nil
}
func (a *sessionAttempt) Dial(endpoint string) (providerwire.Socket, context.CancelFunc, error) {
	a.ProviderRequest()
	headers := a.call.Headers.Clone()
	for name, values := range a.traceHeader {
		headers[name] = append([]string(nil), values...)
	}
	applyAuth(headers, a.call.Auth, a.credential)
	dialContext, cancel := context.WithCancel(a.ctx)
	conn, _, err := websocket.Dial(dialContext, endpoint, &websocket.DialOptions{
		HTTPClient: a.client.httpClient(), HTTPHeader: headers,
	})
	if err != nil {
		cancel()
		if a.ctx.Err() != nil {
			return nil, nil, operationContextFault(a.ctx.Err(), a.requestID)
		}
		return nil, nil, protocol.NewFault(
			protocol.CodeUnavailable,
			"provider WebSocket connection failed",
			retryableTransportError(err),
			protocol.FaultMetadata{
				Origin:      protocol.FaultOriginProvider,
				Stage:       protocol.FaultStageConnection,
				OperationID: a.requestID,
				RetryOwner:  protocol.FaultRetryOwnerEngine,
				ResumeHint:  protocol.FaultResumeRetryStep,
				Disposition: protocol.FaultRetryStep,
				SideEffects: protocol.SideEffectUnchanged,
				Deadline: &protocol.DeadlineMetadata{
					Scope: protocol.DeadlineProviderConnection,
					TimeoutMS: uint64(
						a.client.deadlines.Connection / time.Millisecond,
					),
				},
			},
			err,
		)
	}
	return socket{conn: conn}, cancel, nil
}
func (a *sessionAttempt) ProviderRequest() {
	if a.requestID == "" {
		a.requestID = requestKey(a.call.Body)
	}
}
func (a *sessionAttempt) IdleTimeout() time.Duration { return a.client.IdleTimeout }
func (a *sessionAttempt) Wrap(stream provider.Stream, metadata provider.TransportMetadata) provider.Stream {
	a.transferred = true
	if a.requestID == "" {
		a.ProviderRequest()
	}
	metadata.TransportRequestID = a.requestID
	metadata.RouteCooldownWaitMS = uint64(a.cooldownWait / time.Millisecond)
	return a.client.wrapStream(stream, metadata, a.cancel)
}
func (a *sessionAttempt) Close() {
	if !a.transferred {
		a.cancel()
		a.client.release()
	}
}

type socket struct{ conn *websocket.Conn }

func (s socket) Read(ctx context.Context) ([]byte, error) {
	for {
		kind, data, err := s.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if kind == websocket.MessageText {
			return data, nil
		}
	}
}
func (s socket) Write(ctx context.Context, data []byte) error {
	return s.conn.Write(ctx, websocket.MessageText, data)
}
func (s socket) Close() error { return s.conn.Close(websocket.StatusNormalClosure, "") }
func applyHeaders(request *http.Request, call providerwire.PreparedCall, credential string) {
	request.Header = call.Headers.Clone()
	applyAuth(request.Header, call.Auth, credential)
}
func applyAuth(header http.Header, style providerwire.AuthStyle, credential string) {
	if credential == "" {
		return
	}
	switch style {
	case providerwire.AuthBearer:
		header.Set("Authorization", "Bearer "+credential)
	}
}
func (c *Client) wrapStream(
	stream provider.Stream,
	metadata provider.TransportMetadata,
	cancel context.CancelFunc,
) provider.Stream {
	return &managedStream{
		stream: &metadataStream{Stream: stream, metadata: metadata},
		cancel: cancel, release: c.release, idleTimeout: c.IdleTimeout,
		success: c.recordSuccess, failure: c.recordFailure,
	}
}

var _ providerwire.SessionTransport = (*Client)(nil)
