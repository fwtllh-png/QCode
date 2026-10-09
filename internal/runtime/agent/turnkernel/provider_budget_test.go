package turnkernel

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestProviderWaitReservationSurvivesRestart(t *testing.T) {
	store := NewMemoryTerminalEnvelopeStore(nil, nil)
	runtime, err := NewStoreCoordinatorRuntime(store)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *RuntimeKernel {
		kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "wait-budget", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
		if err != nil {
			t.Fatal(err)
		}
		return kernel
	}
	kernel := open()
	delay, until := time.Minute, time.Now().Add(time.Minute)
	if err := kernel.ReserveProviderWait("sample", delay, until); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Release(t.Context(), "wait-budget"); err != nil {
		t.Fatal(err)
	}
	kernel = open()
	before := kernel.ProviderRetryBudget("sample")
	if before.RateLimitWaited != delay || !before.WaitUntil.Equal(until) || before.RateLimitRetries != 0 || before.TransientRetries != 0 {
		t.Fatalf("restored wait = %+v", before)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := kernel.BeginModelSample(ctx, "sample"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait bypassed cancellation: %v", err)
	}
	if !reflect.DeepEqual(before, kernel.ProviderRetryBudget("sample")) {
		t.Fatal("cancellation refunded wait budget")
	}
	if err := kernel.ReserveProviderWait("sample", delay, until); err == nil {
		t.Fatal("duplicate wait charged twice")
	}
}

func TestProviderBudgetStorageFailureDoesNotConsumeReservation(t *testing.T) {
	for _, rateLimit := range []bool{false, true} {
		failed := false
		store := &failingDomainFactStore{TerminalEnvelopeStore: NewMemoryTerminalEnvelopeStore(nil, nil), fail: &failed}
		runtime, err := NewStoreCoordinatorRuntime(store)
		if err != nil {
			t.Fatal(err)
		}
		kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "wait-store", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), runtime)
		if err != nil {
			t.Fatal(err)
		}
		if err := kernel.BeginModelSample(t.Context(), "sample"); err != nil {
			t.Fatal(err)
		}
		if !rateLimit {
			if err := kernel.FinishModelTransport("sample"); err != nil {
				t.Fatal(err)
			}
		}
		before := kernel.Snapshot()
		failed = true
		if rateLimit {
			err = kernel.ScheduleProviderRetry("sample", ProviderRetryRequested{Retry: 1, Failure: provider.Failure{Code: provider.FailureRateLimit, Message: "limited"}, EffectiveDelay: time.Millisecond, RetryAt: time.Now().Add(time.Millisecond), PolicyRevision: "test"})
		} else {
			err = kernel.ReserveProviderWait("sample", time.Millisecond, time.Now().Add(time.Millisecond))
		}
		if err == nil || !reflect.DeepEqual(before, kernel.Snapshot()) {
			t.Fatalf("failed commit changed state: %v", err)
		}
	}
}

func TestProviderBudgetRejectsInvalidAccounting(t *testing.T) {
	kernel, err := NewRuntimeKernel(KernelIdentity{TurnID: "invalid-budget", ProfileRevision: 1}, protocol.TurnIntentAnswer, "act", nil, false, nil, nil, nil, nil, nil, DefaultPolicy(), NewEphemeralCoordinatorRuntime())
	if err != nil {
		t.Fatal(err)
	}
	if err := kernel.BeginModelSample(t.Context(), "sample"); err != nil {
		t.Fatal(err)
	}
	command := ProviderRetryRequested{Retry: 1, Failure: provider.Failure{Code: provider.FailureRateLimit, Message: "limited"}, EffectiveDelay: time.Millisecond + time.Nanosecond, RetryAt: time.Now().Add(time.Second), PolicyRevision: "test"}
	if err := kernel.ScheduleProviderRetry("sample", command); err != nil {
		t.Fatal(err)
	}
	if kernel.ProviderRetryBudget("sample").RateLimitWaited != command.EffectiveDelay {
		t.Fatal("wait duration lost precision")
	}
	for _, corrupt := range []func(*ModelSampleState){
		func(s *ModelSampleState) { s.RetryBudget.RateLimitRetries = 0 },
		func(s *ModelSampleState) { s.RetryBudget.TransientRetries = math.MaxUint32 },
		func(s *ModelSampleState) { s.RetryBudget.RateLimitWaited = -1 },
		func(s *ModelSampleState) { s.RetryBudget.WaitUntil = time.Time{} },
	} {
		state := kernel.Snapshot()
		sample := state.SampleLedger["sample"]
		corrupt(&sample)
		state.SampleLedger["sample"] = sample
		if Validate(state) == nil {
			t.Fatalf("accepted corrupt budget: %+v", sample)
		}
	}
}

func TestProviderBudgetAbsentAuditFactsRemainReadable(t *testing.T) {
	const original = `{"id":"old","attempt":1,"status":"failed","provider_retries":1,"error":"failed"}`
	var sample ModelSampleState
	if err := json.Unmarshal([]byte(original), &sample); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sample)
	if err != nil || string(encoded) != original {
		t.Fatalf("changed existing audit encoding: %s, %v", encoded, err)
	}
	state := NewState(protocol.TurnIntentAnswer, "act", 1)
	state.SampleLedger["old"] = sample
	if err := Validate(state); err != nil {
		t.Fatalf("old audit rejected: %v", err)
	}
	kernel := &RuntimeKernel{state: state}
	err = kernel.BeginModelSample(t.Context(), "old")
	if problem := protocol.ProblemOf(err); problem == nil || problem.Fault.Origin != protocol.FaultOriginKernel || problem.Fault.RecoveryAction == "" {
		t.Fatalf("missing accounting silently reset: %v", err)
	}
}
