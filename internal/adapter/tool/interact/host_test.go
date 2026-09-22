package interact

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostNotifiesExpiryHandlerOnTimeout(t *testing.T) {
	host := NewHost(time.Millisecond)
	host.SetEmitter(func(context.Context, Request) error {
		return nil
	})
	expired := make(chan Request, 1)
	host.SetExpiryHandler(func(request Request) {
		expired <- request
	})
	_, err := host.Wait(t.Context(), "call-expired", "continue?", nil)
	if err == nil || err.Error() != "input request expired" {
		t.Fatalf("wait error = %v", err)
	}
	select {
	case request := <-expired:
		if request.CallID != "call-expired" {
			t.Fatalf("expiry request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry handler was not notified")
	}
}

func TestHostNotifiesExpiryHandlerOnCancel(t *testing.T) {
	host := NewHost(time.Minute)
	host.SetEmitter(func(context.Context, Request) error {
		return nil
	})
	expired := make(chan Request, 1)
	host.SetExpiryHandler(func(request Request) {
		expired <- request
	})
	ctx, cancel := context.WithCancel(t.Context())
	go cancel()
	_, err := host.Wait(ctx, "call-canceled", "continue?", nil)
	if err == nil {
		t.Fatal("canceled wait returned no error")
	}
	select {
	case request := <-expired:
		if request.CallID != "call-canceled" {
			t.Fatalf("expiry request = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("expiry handler was not notified on cancellation")
	}
}

func TestC5HostRestoresInputWaitWithoutDuplicateEmission(t *testing.T) {
	host := NewHost(time.Minute)
	var emissions atomic.Int32
	var restores atomic.Int32
	host.SetEmitter(func(context.Context, Request) error {
		emissions.Add(1)
		return nil
	})
	request := Request{
		RequestID: "input-restored",
		CallID:    "call-restored",
		Tool:      "request_user_input",
		Prompt:    "continue?",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	host.SetRecoveryHandler(func(restored Request) error {
		if restored.RequestID != request.RequestID ||
			restored.CallID != request.CallID {
			t.Fatalf("restored request = %+v, want %+v", restored, request)
		}
		restores.Add(1)
		return nil
	})
	if err := host.RestoreRequest(request); err != nil {
		t.Fatal(err)
	}
	result := make(chan Reply, 1)
	errs := make(chan error, 1)
	go func() {
		reply, err := host.Wait(
			t.Context(),
			request.CallID,
			request.Prompt,
			nil,
		)
		if err != nil {
			errs <- err
			return
		}
		result <- reply
	}()
	deadline := time.Now().Add(time.Second)
	for {
		err := host.StageReply(Reply{
			RequestID: request.RequestID,
			Answer:    "yes",
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := host.Resume(request.RequestID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	case reply := <-result:
		if reply.RequestID != request.RequestID ||
			reply.Answer != "yes" ||
			emissions.Load() != 0 ||
			restores.Load() != 1 {
			t.Fatalf(
				"reply=%+v emissions=%d restores=%d",
				reply,
				emissions.Load(),
				restores.Load(),
			)
		}
	case <-time.After(time.Second):
		t.Fatal("restored input wait did not resume")
	}
}
