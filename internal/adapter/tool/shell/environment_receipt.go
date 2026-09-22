package shell

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/environment"
	"github.com/fwtllh-png/QCode/internal/security/egress"
	"github.com/fwtllh-png/QCode/internal/security/goproxy"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

func (p *commandProtocol) rememberNetwork(id string, session egress.ProcessSession) {
	if p == nil || id == "" || session == nil {
		return
	}
	p.mu.Lock()
	if p.networks == nil {
		p.networks = map[string]egress.ProcessSession{}
	}
	p.networks[id] = session
	p.mu.Unlock()
}

func (p *commandProtocol) forgetNetwork(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	delete(p.networks, id)
	p.mu.Unlock()
}

func (p *commandProtocol) sessionNetwork(id string) egress.ProcessSession {
	if p == nil || id == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.networks[id]
}

func (p *commandProtocol) sessionEnvironmentFacts(
	ctx context.Context,
	sessionID string,
	priorAuth []environment.Fact,
) []environment.Fact {
	var facts []environment.Fact
	if session := p.sessionNetwork(sessionID); session != nil {
		if gate := session.Gate(); gate != nil {
			facts = environmentFactsFromReceipts(gate.Receipts())
		}
	}
	return append(facts, authServiceFactsSince(ctx, priorAuth)...)
}

func authServiceFactsSince(ctx context.Context, prior []environment.Fact) []environment.Fact {
	var facts []environment.Fact
	// Bind-time facts predate the command, so they are not part of the
	// service cursor: they attach whenever a process result fails.
	if report := goproxy.BindReportFrom(ctx); report != nil {
		facts = append(facts, report.Facts()...)
	}
	if service := goproxy.ServiceFrom(ctx); service != nil {
		facts = append(facts, serviceFactsSinceCursor(prior, service.Facts())...)
	}
	return facts
}

// serviceFactsSinceCursor reports the facts that postdate a prior snapshot.
// Length alone lies when the service's list is ever truncated or reset: the
// prefix is compared, and a rotated list re-reports everything rather than
// silently dropping facts the model has not seen.
func serviceFactsSinceCursor(prior, current []environment.Fact) []environment.Fact {
	if len(current) <= len(prior) {
		if len(current) == 0 {
			return nil
		}
		return current
	}
	for index := range prior {
		if prior[index] != current[index] {
			return current
		}
	}
	return current[len(prior):]
}

func inheritedEnvironmentNetwork(backend sandbox.Backend) []egress.Target {
	policy, ok := sandbox.BackendPolicy(backend)
	if !ok {
		return nil
	}
	return environmentNetworkTargets(policy.EnvironmentNetwork)
}

func environmentNetworkTargets(
	targets []sandbox.EnvironmentNetworkTarget,
) []egress.Target {
	if len(targets) == 0 {
		return nil
	}
	out := make([]egress.Target, 0, len(targets))
	for _, target := range targets {
		if strings.TrimSpace(target.Host) == "" {
			continue
		}
		out = append(out, tunnelEndpointTarget(egress.Target{
			Host:         target.Host,
			Protocol:     target.Protocol,
			Port:         target.Port,
			Methods:      append([]string(nil), target.Methods...),
			AllowPrivate: target.AllowPrivate,
		}))
	}
	return out
}

func resolveProcessNetworkTargets(
	backend sandbox.Backend,
	declared []tool.DeclaredNetworkTarget,
) []egress.Target {
	if targets := declaredNetworkTargets(declared); len(targets) > 0 {
		return targets
	}
	return inheritedEnvironmentNetwork(backend)
}

func openProcessNetwork(
	backend sandbox.Backend,
	denyNetwork bool,
	targets []egress.Target,
	needSession bool,
) (egress.ProcessSession, error) {
	if backend == nil {
		return nil, nil
	}
	if !needSession && (denyNetwork || len(targets) == 0) {
		return nil, nil
	}
	if !sandbox.SupportsManagedNetworkProxy() {
		return nil, fmt.Errorf(
			"%w: %s",
			egress.ErrProcessSessionUnsupported,
			environment.CategoryBackendCapabilityUnsupported,
		)
	}
	if !backend.Capability().ManagedProxy {
		if needSession {
			return nil, fmt.Errorf(
				"%w: %s",
				egress.ErrProcessSessionUnsupported,
				environment.CategoryBackendCapabilityUnsupported,
			)
		}
		return nil, nil
	}
	opener, ok := egress.LookupProcessSessionOpener(backend)
	if !ok {
		return nil, fmt.Errorf(
			"%w: %s",
			egress.ErrProcessSessionUnsupported,
			environment.CategoryBackendCapabilityUnsupported,
		)
	}
	return opener.OpenProcessSession(targets)
}

func declaredNetworkTargets(targets []tool.DeclaredNetworkTarget) []egress.Target {
	if len(targets) == 0 {
		return nil
	}
	out := make([]egress.Target, 0, len(targets))
	for _, target := range targets {
		out = append(out, tunnelEndpointTarget(egress.Target{
			Host: target.Host, Protocol: target.Protocol, Port: target.Port,
			Methods:      append([]string(nil), target.Methods...),
			AllowPrivate: target.AllowPrivate,
		}))
	}
	return out
}

// tunnelEndpointTarget drops per-method grants for HTTPS origins. The
// managed proxy only ever sees the CONNECT tunnel endpoint and cannot
// restrict methods inside TLS, so an HTTPS origin grant is endpoint-wide
// regardless of which method names were declared. HTTP origins keep
// per-method grants because plaintext forward requests are visible.
func tunnelEndpointTarget(target egress.Target) egress.Target {
	if target.Protocol != "https" {
		return target
	}
	target.Methods = nil
	return target
}

func environmentFactsFromReceipts(receipts []egress.Receipt) []environment.Fact {
	facts := make([]environment.Fact, 0)
	for _, receipt := range receipts {
		if fact, ok := factFromDeniedReceipt(receipt); ok {
			facts = append(facts, fact)
		}
	}
	return facts
}

func factFromDeniedReceipt(receipt egress.Receipt) (environment.Fact, bool) {
	if receipt.Decision != "deny" || receipt.Category == "" {
		return environment.Fact{}, false
	}
	source := receipt.Source
	if source == "" {
		source = environment.SourceGate
	}
	return environment.Fact{
		Source:         source,
		Category:       receipt.Category,
		RequiredAction: receipt.RequiredAction,
		Resource: receipt.Protocol + "://" + net.JoinHostPort(
			receipt.Host,
			strconv.Itoa(int(receipt.Port)),
		),
		HasSideEffects: true,
		Detail:         receipt.Reason,
	}, true
}

// attachMissingCapability writes the authoritative missing-capability
// category onto a failed process result. Existing categories such as
// process_still_running are kept. Output text is never inspected.
func unsupportedSessionNetworkResult(err error) (tool.Result, bool) {
	if !errors.Is(err, egress.ErrProcessSessionUnsupported) {
		return tool.Result{}, false
	}
	result := tool.Result{
		Content:  err.Error(),
		IsError:  true,
		Metadata: map[string]any{},
	}
	attachMissingCapability(&result, []environment.Fact{{
		Source:   environment.SourceOSBackend,
		Category: environment.CategoryBackendCapabilityUnsupported,
		Detail:   err.Error(),
	}})
	return result, true
}

func attachMissingCapability(result *tool.Result, facts []environment.Fact) {
	if result == nil {
		return
	}
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	if existing, _ := result.Metadata["error_category"].(string); existing != "" {
		return
	}
	if !result.IsError {
		return
	}
	category, action := environment.ClassifyFromAuthority(facts)
	result.Metadata["error_category"] = category
	if action != "" {
		result.Metadata["required_action"] = action
	}
	if detail := firstTypedFactDetail(facts); detail != "" {
		result.Metadata["environment_detail"] = detail
	}
	result.Metadata["retry_original"] = false
}

// firstTypedFactDetail returns the first typed fact's explanation, aligned
// with ClassifyFromAuthority's ordering: the authoritative component's own
// wording (toolchain switch guidance, netrc pointers) must not be dropped
// when only the category travels.
func firstTypedFactDetail(facts []environment.Fact) string {
	for _, fact := range facts {
		if fact.Category == "" || fact.Category == environment.CategoryUnknown {
			continue
		}
		return fact.Detail
	}
	return ""
}
