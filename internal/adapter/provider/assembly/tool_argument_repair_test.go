package assembly

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerfixture "github.com/fwtllh-png/QCode/internal/adapter/provider/fixture"
)

func rejectedToolStream(arguments string) provider.Stream {
	return &providerfixture.SliceStream{Events: []provider.StreamEvent{
		{Type: EventTextDelta, Text: "retained progress"},
		{Type: EventUsage, Usage: &Usage{InputTokens: 20, OutputTokens: 3}},
		{Type: EventToolCallDelta, ToolCall: &ToolCallFragment{ID: "bad", Name: "write", Arguments: arguments}},
		{Type: EventMessageStop, StopReason: StopReasonToolUse},
	}}
}

func TestToolArgumentRepairAuthorizationSurvivesRestartAndExhausts(t *testing.T) {
	for _, limit := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			assembly := NewResponseAssembly("repair")
			for rejected := 0; rejected <= limit; rejected++ {
				result, err := ConsumeStream(rejectedToolStream(`{"text":"first","text":"second"}`), assembly, ConsumeConfig{})
				if err == nil || !assembly.ToolArgumentsRejected() || len(result.Calls) != 0 || len(assembly.IncompleteToolFragments()) != 0 {
					t.Fatalf("rejected response leaked calls or continuation: %+v %v", result, err)
				}
				before := CloneResponseAssembly(assembly)
				if err := assembly.BeginTransport(TransportMetadata{}); err == nil {
					t.Fatal("failed response resumed without authorization")
				}
				err = assembly.AuthorizeToolArgumentRepair(limit)
				if rejected == limit {
					if !errors.Is(err, ErrToolArgumentRepairLimit) {
						t.Fatalf("budget error=%v", err)
					}
					break
				}
				if err != nil || assembly.ValidateExtension(before) != nil {
					t.Fatalf("authorization failed: %v", err)
				}
				assembly = CloneResponseAssembly(assembly)
				if err := assembly.AuthorizeToolArgumentRepair(limit); err != nil || len(assembly.ToolArgumentRepairs) != rejected+1 {
					t.Fatalf("restart spent the authorization twice: %+v %v", assembly.ToolArgumentRepairs, err)
				}
				if !reflect.DeepEqual(before.Segments, assembly.Segments) {
					t.Fatal("authorization rewrote the rejected response")
				}
			}
			if got := assembly.TotalUsage().InputTokens; got != uint64(20*(limit+1)) {
				t.Fatalf("rejected usage lost: %d", got)
			}
		})
	}
}

func TestToolArgumentRepairUsesOnlyFreshCalls(t *testing.T) {
	for _, arguments := range []string{`{"text":1,"text":2}`, `{"text":`} {
		t.Run(arguments, func(t *testing.T) {
			assembly := NewResponseAssembly("repair")
			if _, err := ConsumeStream(rejectedToolStream(arguments), assembly, ConsumeConfig{}); err == nil {
				t.Fatal("invalid completed call accepted")
			}
			if err := assembly.AuthorizeToolArgumentRepair(1); err != nil {
				t.Fatal(err)
			}
			before := CloneResponseAssembly(assembly)
			stream := &providerfixture.SliceStream{Events: []StreamEvent{
				{Type: EventToolCallDelta, ToolCall: &ToolCallFragment{ID: "good", Name: "write", Arguments: `{"text":"valid"}`}},
				{Type: EventMessageStop, StopReason: StopReasonToolUse},
			}}
			result, err := ConsumeStream(stream, assembly, ConsumeConfig{})
			if err != nil || len(result.Calls) != 1 || result.Calls[0].ID != "good" || assembly.ValidateExtension(before) != nil {
				t.Fatalf("fresh response=%+v err=%v", result, err)
			}
			if !reflect.DeepEqual(CloneResponseAssembly(assembly).Segments[0], before.Segments[0]) {
				t.Fatal("fresh response changed rejected segment")
			}
		})
	}
}

func TestToolArgumentRepairRejectsForgedOrRewrittenAuthorization(t *testing.T) {
	assembly := NewResponseAssembly("repair")
	_, _ = ConsumeStream(rejectedToolStream(`{"text":1,"text":2}`), assembly, ConsumeConfig{})
	if err := assembly.AuthorizeToolArgumentRepair(1); err != nil {
		t.Fatal(err)
	}
	for _, repairs := range [][]uint32{nil, {2}, {1, 1}} {
		changed := CloneResponseAssembly(assembly)
		changed.ToolArgumentRepairs = repairs
		if err := changed.ValidateExtension(assembly); err == nil {
			t.Fatalf("repair authorization rewrite accepted: %v", repairs)
		}
	}
	failed := NewResponseAssembly("contract-failure")
	if err := failed.BeginTransport(TransportMetadata{}); err != nil {
		t.Fatal(err)
	}
	_ = failed.Fail(errors.New("changed provider event identity"))
	if err := failed.AuthorizeToolArgumentRepair(3); err == nil {
		t.Fatal("non-argument contract failure became regenerable")
	}
}
