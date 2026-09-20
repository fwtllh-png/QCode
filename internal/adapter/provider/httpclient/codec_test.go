package httpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func (c *Client) Stream(
	ctx context.Context,
	request provider.ModelRequest,
) (provider.Stream, error) {
	adapter, err := testAdapter(request.Route.Adapter())
	if err != nil {
		return nil, err
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		return nil, protocol.NewProblem(
			protocol.CodeInvalidArgument, err.Error(), false, err,
		)
	}
	if sessionAdapter, ok := adapter.(providerwire.SessionAdapter); ok {
		stream, handled, err := sessionAdapter.TrySession(
			ctx, request, call, c,
		)
		if handled || err != nil {
			return stream, err
		}
	}
	return c.Execute(ctx, request, call, adapter)
}

func encodeRequest(
	request provider.ModelRequest,
) ([]byte, string, error) {
	adapter, err := testAdapter(request.Route.Adapter())
	if err != nil {
		return nil, "", err
	}
	call, err := adapter.Prepare(request)
	return call.Body, call.Path, err
}

func mustEncodeRequest(
	t testing.TB,
	request provider.ModelRequest,
	target any,
) string {
	t.Helper()
	data, path, err := encodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAdapter(id model.AdapterID) (providerwire.Adapter, error) {
	adapter, err := openai.NewAdapter(id)
	if err != nil {
		return nil, fmt.Errorf("test adapter: %w", err)
	}
	return adapter, nil
}
