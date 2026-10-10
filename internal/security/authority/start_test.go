package authority

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionLeasePhysicalStartIsSingleUseAndChecksExpiry(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "expired_after_consume"}[expire], func(t *testing.T) {
			now := time.Now()
			a := NewLeaseAuthority(LeaseAuthorityOptions{Now: func() time.Time { return now }})
			op, profile := fixtureLeaseInputs(t)
			lease, err := a.Issue(LeaseIssueRequest{Operation: op, Profile: profile, PolicyRevision: 7, SandboxPolicyID: "sandbox-policy", Attempt: 1, ExpiresAt: now.Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Consume(lease, fixtureLeaseValidation(op)); err != nil {
				t.Fatal(err)
			}
			if expire {
				now = now.Add(2 * time.Minute)
			}
			var starts, admitted atomic.Int32
			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					if err := a.Start(lease, func() error { starts.Add(1); return nil }); err == nil {
						admitted.Add(1)
					}
				})
			}
			wg.Wait()
			want := int32(1)
			if expire {
				want = 0
			}
			if starts.Load() != want || admitted.Load() != want {
				t.Fatalf("starts=%d admitted=%d want=%d", starts.Load(), admitted.Load(), want)
			}
		})
	}
}
