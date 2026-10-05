package protocol

import "testing"

func TestKindOfNilData(t *testing.T) {
	for _, tc := range []struct {
		name string
		data EventData
		want EventKind
	}{
		{"nil interface", nil, ""},
		{"nil known payload", (*TurnCompletedData)(nil), EventTurnCompleted},
		{"nil unknown payload", (*UnknownEventData)(nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.data); got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}
