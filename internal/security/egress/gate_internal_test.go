package egress

import (
	"net"
	"net/url"
	"testing"
)

func TestNonPublicIPCoversCGNATBenchmarkAndReservedRanges(t *testing.T) {
	for _, raw := range []string{
		"100.64.0.1", "100.127.255.254", // RFC 6598 CGNAT
		"198.18.0.1", "198.19.255.254", // RFC 2544 benchmarking
		"240.0.0.1", "255.255.255.254", // reserved / broadcast
		"10.0.0.1", "169.254.169.254", "127.0.0.1",
	} {
		if !nonPublicIP(net.ParseIP(raw)) {
			t.Fatalf("%s was treated as public", raw)
		}
	}
	for _, raw := range []string{"203.0.113.10", "1.1.1.1", "93.184.216.34"} {
		if nonPublicIP(net.ParseIP(raw)) {
			t.Fatalf("%s was treated as non-public", raw)
		}
	}
}

func TestRequestPortRejectsExplicitZero(t *testing.T) {
	parsed, err := url.Parse("http://example.com:0/path")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := requestPort(parsed); err == nil {
		t.Fatal("explicit :0 was silently defaulted")
	}
	defaulted, err := url.Parse("http://example.com/path")
	if err != nil {
		t.Fatal(err)
	}
	if port, err := requestPort(defaulted); err != nil || port != 80 {
		t.Fatalf("scheme default port = %d err=%v", port, err)
	}
}

func TestReceiptLogIsBounded(t *testing.T) {
	gate := &Gate{Enforce: true}
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
