package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func estimatorMessage(units int) provider.Message {
	return messageWithText(
		provider.RoleUser, strings.Repeat("a", units*4), 1,
	)
}

func TestCalibratedTokenEstimatorAdoptsObservedRatio(t *testing.T) {
	estimator := &calibratedTokenEstimator{inner: HeuristicTokenEstimator{}}
	base, err := estimator.Estimate([]provider.Message{estimatorMessage(1000)})
	if err != nil || base != 1000 {
		t.Fatalf("uncalibrated estimate = %d, %v", base, err)
	}
	estimator.Observe(1000, 1500)
	scaled, err := estimator.Estimate([]provider.Message{estimatorMessage(1000)})
	if err != nil || scaled != 1500 {
		t.Fatalf("calibrated estimate = %d, %v", scaled, err)
	}
	// Later feedback keeps learning against the raw heuristic: once the
	// calibrated estimate equals the reported usage, the ratio must hold
	// instead of collapsing to 1 and re-inflating the next estimate.
	estimator.Observe(1500, 1500)
	held, err := estimator.Estimate([]provider.Message{estimatorMessage(1000)})
	if err != nil || held != 1500 {
		t.Fatalf("re-observed estimate = %d, %v", held, err)
	}
}

func TestCalibratedTokenEstimatorConvergesAcrossRepeatedFeedback(t *testing.T) {
	estimator := &calibratedTokenEstimator{inner: HeuristicTokenEstimator{}}
	message := []provider.Message{estimatorMessage(1120)}
	var estimates []uint64
	for round := 0; round < 6; round++ {
		estimated, err := estimator.Estimate(message)
		if err != nil {
			t.Fatal(err)
		}
		estimates = append(estimates, estimated)
		estimator.Observe(estimated, 2240)
	}
	if estimates[0] != 1120 {
		t.Fatalf("uncalibrated estimate = %d, want 1120", estimates[0])
	}
	for round, estimate := range estimates[1:] {
		if estimate != 2240 {
			t.Fatalf(
				"round %d estimate = %d, want the observed 2240 (estimates=%v)",
				round+1, estimate, estimates,
			)
		}
	}
}

func TestCalibratedTokenEstimatorIgnoresImplausibleRatios(t *testing.T) {
	estimator := &calibratedTokenEstimator{inner: HeuristicTokenEstimator{}}
	estimator.Observe(1000, 10000)
	if base, _ := estimator.Estimate([]provider.Message{estimatorMessage(1000)}); base != 1000 {
		t.Fatalf("out-of-range high ratio learned: %d", base)
	}
	estimator.Observe(1000, 10)
	if base, _ := estimator.Estimate([]provider.Message{estimatorMessage(1000)}); base != 1000 {
		t.Fatalf("out-of-range low ratio learned: %d", base)
	}
	estimator.Observe(0, 100)
	estimator.Observe(100, 0)
	// Boundary ratios are exactly the documented envelope.
	lower := &calibratedTokenEstimator{inner: HeuristicTokenEstimator{}}
	lower.Observe(1000, 250)
	if scaled, _ := lower.Estimate([]provider.Message{estimatorMessage(1000)}); scaled != 250 {
		t.Fatalf("lower boundary ratio rejected: %d", scaled)
	}
	upper := &calibratedTokenEstimator{inner: HeuristicTokenEstimator{}}
	upper.Observe(1000, 8000)
	if scaled, _ := upper.Estimate([]provider.Message{estimatorMessage(1000)}); scaled != 8000 {
		t.Fatalf("upper boundary ratio rejected: %d", scaled)
	}
}

type failingEstimator struct{}

func (failingEstimator) Estimate([]provider.Message) (uint64, error) {
	return 0, errors.New("estimator unavailable")
}

func TestCalibratedTokenEstimatorForwardsInnerErrors(t *testing.T) {
	estimator := &calibratedTokenEstimator{inner: failingEstimator{}}
	estimator.Observe(1000, 1500)
	if _, err := estimator.Estimate([]provider.Message{estimatorMessage(1)}); err == nil {
		t.Fatal("inner estimator error was swallowed")
	}
}

func TestEngineObservationCalibratesSessionEstimator(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.observeTokenWindow(
		&protocol.SampleContextData{EstimatedTokens: 1000}, 1500, 0,
	)
	estimate, err := engine.options.TokenEstimator.Estimate(
		[]provider.Message{estimatorMessage(1000)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if estimate != 1500 {
		t.Fatalf("session estimator not calibrated: %d", estimate)
	}
}

// Same immutable context and constant provider usage across requests: the
// calibrated estimate and the window projection must settle on the reported
// usage instead of alternating between the raw heuristic and its multiple.
func TestEngineTokenCalibrationConvergesWindowProjection(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	attachTestScope(t, engine)
	const actualInput = uint64(2240)
	var estimates, projections []uint64
	for round := 0; round < 5; round++ {
		estimated, err := engine.options.TokenEstimator.Estimate(
			[]provider.Message{estimatorMessage(1120)},
		)
		if err != nil {
			t.Fatal(err)
		}
		sample := protocol.SampleContextData{
			ContextDigest: "sha256:stable", EstimatedTokens: estimated,
		}
		projection := engine.prepareTokenWindow(&sample, 0)
		estimates = append(estimates, estimated)
		projections = append(projections, projection.FullActiveTokens)
		engine.observeTokenWindow(&sample, actualInput, 0)
	}
	if estimates[0] != 1120 || projections[0] != 1120 {
		t.Fatalf(
			"uncalibrated round: estimates=%v projections=%v", estimates, projections,
		)
	}
	for round := 1; round < len(estimates); round++ {
		if estimates[round] != actualInput || projections[round] != actualInput {
			t.Fatalf(
				"round %d diverged (want estimate and projection %d): estimates=%v projections=%v",
				round, actualInput, estimates, projections,
			)
		}
	}
}

// Capacity decisions before the first provider observation run on the
// dense-script baseline, and the same decisions must adopt the learned ratio
// afterwards instead of staying on the baseline.
func TestEngineEstimateMessageTokensAdoptsCalibration(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, nil)
	messages := []provider.Message{
		provider.TextMessage(provider.RoleUser, "配置文件内容"),
	}
	base := engine.EstimateMessageTokens(messages)
	if base != 6 {
		t.Fatalf("baseline estimate = %d, want 6 dense-script tokens", base)
	}
	engine.tokenCalibration.Observe(base, base*2)
	if got := engine.EstimateMessageTokens(messages); got != base*2 {
		t.Fatalf("calibrated estimate = %d, want %d", got, base*2)
	}
}
