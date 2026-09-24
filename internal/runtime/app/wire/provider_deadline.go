package wire

import (
	"context"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider/httpclient"
	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/observability/telemetry"
	"github.com/fwtllh-png/QCode/internal/security/credential"
	"github.com/fwtllh-png/QCode/internal/security/egress"
)

func configureProviderClient(
	execution *config.Execution,
	egressGate *egress.Gate,
	metrics *telemetry.Metrics,
	control *credential.Control,
	controlled model.CredentialRef,
) *httpclient.Client {
	client := httpclient.New()
	if control != nil {
		controls := map[model.CredentialRef]*credential.Control{controlled: control}
		client.Credentials = liveCredentialResolver{
			controls: controls,
			fallback: httpclient.DefaultCredentials(),
		}
	}
	client.Egress, client.Metrics = egressGate, metrics
	client.SetDeadlineConfig(httpclient.DeadlineConfig{
		Connection: effectiveProviderDeadline(
			execution.ConnectionTimeout,
			execution.Timeout,
		),
		TLSHandshake: effectiveProviderDeadline(
			execution.TLSHandshakeTimeout,
			execution.Timeout,
		),
		ResponseHeaders: effectiveProviderDeadline(
			execution.ResponseHeaderTimeout,
			execution.Timeout,
		),
	})
	client.IdleTimeout = execution.IdleTimeout
	client.MaxConcurrent = execution.MaxConcurrent
	client.RequestsPerSecond = execution.RateLimit
	return client
}

// liveCredentialResolver 按“引用 → Control”映射热解析多连接凭证：受管
// 引用走各自命名空间的 Control（支持轮换），其余引用走静态解析器。
type liveCredentialResolver struct {
	controls  map[model.CredentialRef]*credential.Control
	fallback  httpclient.Credentials
}

func (r liveCredentialResolver) Resolve(
	ctx context.Context,
	reference model.CredentialRef,
) (string, error) {
	if control, ok := r.controls[reference]; ok {
		current, err := control.Reference(ctx)
		if err != nil {
			return "", err
		}
		reference = model.CredentialRef{
			Kind: current.Kind,
			Name: current.Name,
		}
	}
	return r.fallback.Resolve(ctx, reference)
}

func effectiveProviderDeadline(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
