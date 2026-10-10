package httpclient

import (
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	providerratelimit "github.com/fwtllh-png/QCode/internal/adapter/provider/ratelimit"
)

func (c *Client) DecideThroughput(
	route model.ReadyRoute,
	required uint64,
	operatorLimit uint64,
) providerratelimit.Decision {
	return c.limits.Decide(
		providerratelimit.Key(route),
		required,
		operatorLimit,
		time.Now(),
	)
}

func (c *Client) ReserveThroughput(route model.ReadyRoute, tokens uint64) {
	c.limits.Reserve(providerratelimit.Key(route), tokens, time.Now())
}

func (c *Client) TryReserveThroughput(route model.ReadyRoute, tokens, operatorLimit uint64) providerratelimit.Decision {
	return c.limits.TryReserve(providerratelimit.Key(route), tokens, operatorLimit, time.Now())
}
