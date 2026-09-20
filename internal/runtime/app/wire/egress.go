package wire

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

// Tool approvals never mutate provider grants or cross the web/process boundary.
func toolNetworkAllow(webGate, processGate *egress.Gate) toolguard.NetworkAllow {
	return func(capability tool.Capability, target egress.Target) {
		switch capability {
		case tool.CapabilityNetwork:
			webGate.AllowTarget(target)
		case tool.CapabilityProcess:
			processGate.AllowTarget(target)
		}
	}
}

func grantRouteHosts(gate *egress.Gate, routes model.RouteSet) {
	if gate == nil || !routes.Ready() {
		return
	}
	gate.AllowURL(routes.Act().Endpoint())
	for _, purpose := range routes.Slots() {
		route, err := routes.For(purpose)
		if err != nil {
			continue
		}
		gate.AllowURL(route.Endpoint())
	}
}

func grantWebBackendHosts(gate *egress.Gate, options webtool.Options) {
	if gate == nil {
		return
	}
	for _, raw := range []string{
		options.PrimaryURL, options.FallbackURL, options.TavilyURL,
		options.SearXNGURL, options.BochaURL, options.SearchURL,
	} {
		gate.AllowURL(raw)
	}
}
