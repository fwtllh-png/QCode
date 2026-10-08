package assembly

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestJSONMemberTrackerAcrossFragmentBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		duplicate bool
	}{
		{"degenerate unfinished call", `{"changes":[],"dry_run":false,"dry_run"`, true},
		{"nested duplicate", `{"changes":[{"path":"a","path":"b"}]}`, true},
		{"escaped member", `{"dry_run":false,"dry\u005frun":true}`, true},
		{"unicode member", `{"中文":1,"\u4e2d\u6587":2}`, true},
		{"surrogate pair", `{"😀":1,"\ud83d\ude00":2}`, true},
		{"escaped quote", `{"a\"b":1,"a\u0022b":2}`, true},
		{"empty member", `{"":1,"":2}`, true},
		{"different objects", `{"changes":[{"path":"a","dry_run":false},{"path":"b","dry_run":false}],"dry_run":false}`, false},
		{"nested scopes", `{"key":{"key":0},"items":[{"key":1},[null,{"key":2}]]}`, false},
		{"source strings", `{"code":"{\"dry_run\":false,\"dry_run\":false}","path":"C:\\src\\a.go","dry_run":false}`, false},
		{"string values", `{"a":"a","b":["b","b",{"b":"b"}]}`, false},
		{"distinct unicode", `{"é":1,"e\u0301":2}`, false},
		{"scalars", ` [true,false,null,-0.12e+3,{},[],"x"] `, false},
		{"incomplete member", `{"dry_run":false,"dry_run`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A transport can split anywhere, including inside an escape or
			// UTF-8 sequence. Check every two-fragment boundary and bytewise.
			for split := 0; split <= len(test.arguments); split++ {
				var tracker jsonMemberTracker
				err := tracker.Append(test.arguments[:split])
				if err == nil {
					err = tracker.Append(test.arguments[split:])
				}
				if errors.Is(err, errDuplicateJSONMember) != test.duplicate || err != nil && !test.duplicate {
					t.Fatalf("split %d: error = %v, duplicate = %t", split, err, test.duplicate)
				}
			}
			var tracker jsonMemberTracker
			var err error
			for i := 0; i < len(test.arguments) && err == nil; i++ {
				err = tracker.Append(test.arguments[i : i+1])
			}
			if errors.Is(err, errDuplicateJSONMember) != test.duplicate || err != nil && !test.duplicate {
				t.Fatalf("bytewise error = %v, duplicate = %t", err, test.duplicate)
			}
		})
	}
}

func TestJSONMemberTrackerDoesNotRetainSourceValues(t *testing.T) {
	var tracker jsonMemberTracker
	if err := tracker.Append(`{"content":"`); err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if err := tracker.Append(strings.Repeat(`\"dry_run\":false,`, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if len(tracker.name) != 0 || len(tracker.scopes) != 1 || len(tracker.scopes[0].names) != 1 {
		t.Fatal("source value was retained as member tracking state")
	}
	if err := tracker.Append(`","dry_run":false}`); err != nil {
		t.Fatal(err)
	}
	if len(tracker.scopes) != 0 {
		t.Fatal("closed object scope was retained")
	}
}

// Compare the incremental tracker with the standard decoder's complete token
// stream, so fuzzing also guards against rejecting valid generated source code.
func FuzzJSONMemberTracker(f *testing.F) {
	for _, seed := range []string{
		`{"dry_run":false,"dry_run":false}`, `{"x":[{"a":1},{"a":2}]}`,
		`{"a":"{\"a\":1,\"a\":2}","b":null}`, `{"\u0061":1,"a":2}`,
		`{"😀":1,"\ud83d\ude00":2}`, ` [1,2,"3"] `, `{"x":1E1000}`,
	} {
		f.Add(seed, uint16(1))
	}
	f.Fuzz(func(t *testing.T, data string, size uint16) {
		if !json.Valid([]byte(data)) {
			return
		}
		decoder := json.NewDecoder(strings.NewReader(data))
		decoder.UseNumber()
		want := decodedDuplicateMember(t, decoder)
		var tracker jsonMemberTracker
		var err error
		chunk := int(size) + 1
		for offset := 0; offset < len(data) && err == nil; offset += chunk {
			err = tracker.Append(data[offset:min(offset+chunk, len(data))])
		}
		if errors.Is(err, errDuplicateJSONMember) != want || err != nil && !want {
			t.Fatalf("error = %v, want duplicate = %t", err, want)
		}
	})
}

func decodedDuplicateMember(t *testing.T, decoder *json.Decoder) bool {
	t.Helper()
	token, err := decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	delim, container := token.(json.Delim)
	if !container {
		return false
	}
	names := make(map[string]bool)
	duplicate := false
	for decoder.More() {
		if delim == '{' {
			key, err := decoder.Token()
			if err != nil {
				t.Fatal(err)
			}
			name := key.(string)
			duplicate = duplicate || names[name]
			names[name] = true
		}
		child := decodedDuplicateMember(t, decoder)
		duplicate = duplicate || child
	}
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	return duplicate
}

func TestExecutableToolCallsRejectsPersistedDuplicateMembers(t *testing.T) {
	assembly := NewResponseAssembly("persisted")
	if err := assembly.BeginTransport(TransportMetadata{}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []StreamEvent{
		{Type: EventToolCallDelta, ToolCall: &ToolCallFragment{
			ID: "call", Name: "file_apply", Arguments: `{"dry_run":true,"dry_run":false}`,
		}},
		{Type: EventMessageStop, StopReason: StopReasonToolUse},
	} {
		if _, err := assembly.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
	if calls, err := CloneResponseAssembly(assembly).ExecutableToolCalls(); len(calls) != 0 || !errors.Is(err, errDuplicateJSONMember) {
		t.Fatalf("calls = %+v, error = %v", calls, err)
	}
}

func BenchmarkJSONMemberTracker(b *testing.B) {
	for _, size := range []int{1024, 65536, 1048576} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			data := `{"content":"` + strings.Repeat("x", size) + `","dry_run":false}`
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				var tracker jsonMemberTracker
				for start := 0; start < len(data); start += 128 {
					if err := tracker.Append(data[start:min(start+128, len(data))]); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
