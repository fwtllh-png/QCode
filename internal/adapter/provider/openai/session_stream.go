package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func newResponsesSocketStream(
	ctx context.Context,
	session *responsesSession,
	input []json.RawMessage,
	property string,
	projection provider.ProjectionReceipt,
	requestProjection provider.ProjectionContext,
	idleTimeout time.Duration,
) *responsesSocketStream {
	return &responsesSocketStream{
		ctx: ctx, session: session, conn: session.conn,
		input: input, property: property,
		routeDigest: projection.RouteDigest,
		windowID:    requestProjection.WindowID,
		recoveryID:  requestProjection.RecoveryID,
		idleTimeout: idleTimeout, decoder: ResponsesDecoder{
			CaptureState: true, CaptureReplay: true,
		},
	}
}

func sessionMetadata(
	request provider.ModelRequest,
	call providerwire.PreparedCall,
	payload []byte,
	incremental bool,
	projection provider.ProjectionReceipt,
) provider.TransportMetadata {
	metadata := providerwire.MetadataWithProjection(
		call.Body,
		payload,
		incremental,
		projection,
	)
	metadata.LogicalRequestID = request.LogicalRequestID
	metadata.Attempt = request.TransportAttempt
	return metadata
}

// read is called with s.mu held and releases it only while blocked on the
// socket. A Close during that window hands the session to the next request,
// so the late result must not touch session state.
func (s *responsesSocketStream) read() error {
	s.mu.Unlock()
	data, err := s.conn.Read(s.ctx)
	s.mu.Lock()
	if s.closed {
		return io.EOF
	}
	if err != nil {
		s.session.forceHTTP = true
		s.session.invalidate()
		if errors.Is(err, io.EOF) {
			return protocol.NewProblem(
				protocol.CodeUnavailable,
				"Responses WebSocket ended before completion",
				true,
				io.ErrUnexpectedEOF,
			)
		}
		return err
	}
	events, err := s.decoder.Decode(data)
	if err != nil {
		s.session.forceHTTP = true
		s.session.invalidate()
		return err
	}
	s.queue = append(s.queue, events...)
	return nil
}

func (s *responsesSocketStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.session.lastUsed = time.Now()
	if s.session.idle != nil {
		s.session.idle.Stop()
	}
	if !s.stopped {
		// The response may still be streaming on this socket; reusing it would
		// hand its remaining frames to the next request. The close handshake
		// can wait on a stalled peer, so it runs detached; completing it also
		// unblocks any abandoned Read.
		conn := s.session.conn
		s.session.conn = nil
		s.session.invalidate()
		if conn != nil {
			go func() { _ = conn.Close() }()
		}
	} else if s.idleTimeout > 0 {
		session := s.session
		session.idle = time.AfterFunc(s.idleTimeout, func() {
			session.mu.Lock()
			defer session.mu.Unlock()
			if time.Since(session.lastUsed) >= s.idleTimeout {
				session.invalidate()
			}
		})
	}
	s.session.mu.Unlock()
	return nil
}

var _ provider.Stream = (*responsesSocketStream)(nil)
