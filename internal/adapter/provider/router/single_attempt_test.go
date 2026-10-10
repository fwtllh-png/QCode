package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
)

type guardianSessionAdapter struct {
	testAdapter
	sessions int
}

func (a *guardianSessionAdapter) TrySession(context.Context, provider.ModelRequest, providerwire.PreparedCall, providerwire.SessionTransport) (provider.Stream, bool, error) {
	a.sessions++
	return nil, true, errors.New("session path used")
}

type guardianSessionTransport struct{ testTransport }

func (*guardianSessionTransport) BeginSession(context.Context, model.ReadyRoute, providerwire.PreparedCall) (providerwire.SessionAttempt, error) {
	return nil, errors.New("unexpected session")
}

func TestSingleAttemptSkipsSessionTransport(t *testing.T) {
	a := &guardianSessionAdapter{testAdapter: testAdapter{id: model.AdapterOpenAI}}
	registry, err := NewRegistry(a)
	if err != nil {
		t.Fatal(err)
	}
	route := testRoute(t, model.AdapterOpenAI, model.ProtocolOpenAIResponses)
	caps := route.Model().Capabilities
	caps.IncrementalResponses = true
	route = route.WithCapabilities(caps)
	routes, _ := model.NewRouteSet(route, nil, false)
	transport := &guardianSessionTransport{}
	router, err := New(registry, routes, transport)
	if err != nil {
		t.Fatal(err)
	}
	r := provider.ModelRequest{Route: route, Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "review")}, MaxOutputTokens: 16, SingleAttempt: true}
	stream, err := router.Stream(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if transport.calls != 1 || a.sessions != 0 {
		t.Fatal("single attempt entered a replaying session")
	}
	encoded, err := json.Marshal(r)
	if err != nil || strings.Contains(strings.ToLower(string(encoded)), "singleattempt") {
		t.Fatal("local transport policy serialized into wire request")
	}
	r.SingleAttempt = false
	if _, err := router.Stream(t.Context(), r); err == nil || a.sessions != 1 || transport.calls != 1 {
		t.Fatal("session control did not exercise session adapter")
	}
}
