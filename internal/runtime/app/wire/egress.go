package wire

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	webtool "github.com/fwtllh-png/QCode/internal/adapter/tool/web"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

// Tool approvals never mutate provider grants or the workspace process Gate.
// Process targets bind to a Session Gate when the process starts.
func toolNetworkAllow(webGate *egress.Gate) toolguard.NetworkAllow {
	return func(capability tool.Capability, target egress.Target) {
		if capability == tool.CapabilityNetwork {
			webGate.AllowTarget(target)
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
