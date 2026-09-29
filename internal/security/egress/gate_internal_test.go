package egress

import "testing"

func TestReceiptLogIsBounded(t *testing.T) {
	gate := NewStaticGate()
	gate.AllowTarget(Target{Host: "example.com", Protocol: "https", Port: 443})
	authorized := Target{
		Host: "example.com", Protocol: "https", Port: 443,
		Methods: []string{"GET"},
	}
	for range maxReceipts * 2 {
		if _, err := gate.Authorize(t.Context(), authorized, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if receipts := gate.Receipts(); len(receipts) > maxReceipts {
		t.Fatalf("receipt log grew to %d beyond the %d cap", len(receipts), maxReceipts)
	}
}
