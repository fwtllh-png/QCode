package wire

import (
	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	providerrouter "github.com/fwtllh-png/QCode/internal/adapter/provider/modelcatalog"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/openai"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
)

func newProviderRouter(
	client *httpclient.Client,
	routes model.RouteSet,
) (provider.Provider, error) {
	openAI, err := openai.NewAdapter(model.AdapterOpenAI)
	if err != nil {
		return nil, err
	}
	compatible, err := openai.NewAdapter(model.AdapterOpenAICompatible)
	if err != nil {
		return nil, err
	}
	adapters := []providerwire.Adapter{
		openAI, compatible,
	}
	registry, err := providerrouter.NewRegistry(adapters...)
	if err != nil {
		return nil, err
	}
	return providerrouter.New(registry, routes, client)
}
