package netpolicy

import (
	"net"
	"net/url"
	"testing"
)

func TestClassifyCoversSpecialPurposeRanges(t *testing.T) {
	for _, raw := range []string{
		"100.64.0.1", "100.127.255.254", // RFC 6598 CGNAT
		"198.18.0.1", "198.19.255.254", // RFC 2544 benchmarking
		"240.0.0.1", "255.255.255.254", // reserved / broadcast
		"10.0.0.1", "169.254.169.254", "127.0.0.1",
		"0.0.0.0", "0.0.0.1", "0.255.255.255", // RFC 791 "this network"
		"192.0.0.8", "192.0.2.1", "198.51.100.1", "203.0.113.10", "192.88.99.1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", // IPv4-mapped
		"::10.0.0.1", "::0.0.0.2", // IPv4-compatible
		"64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", // NAT64 into private/metadata/loopback
		"64:ff9b:1::1",                      // RFC 8215 local-use NAT64
		"2002:a00:1::1",                     // 6to4 wrapping 10.0.0.1
		"2002:7f00:1::1",                    // 6to4 wrapping 127.0.0.1
		"2001::1", "2001:db8::1", "3fff::1", // Teredo, documentation
		"fec0::1", "fc00::1", "fe80::1", "ff02::1", "100::1", "::1", "::",
	} {
		if Classify(net.ParseIP(raw)) == Public {
			t.Fatalf("%s was treated as public", raw)
		}
	}
	for _, raw := range []string{
		"1.1.1.1", "93.184.216.34", "::ffff:1.1.1.1",
		"64:ff9b::101:101", "2002:101:101::1", "2606:4700:4700::1111",
	} {
		if reach := Classify(net.ParseIP(raw)); reach != Public {
			t.Fatalf("%s was treated as %s", raw, reach)
		}
	}
}

func TestClassifySeesThroughEmbeddedIPv4(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "0.0.0.1", "169.254.169.254", "::1", "fe80::1",
		"::ffff:127.0.0.1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::1", "::127.0.0.1",
		"224.0.0.251", "ff02::fb", "ff01::1",
	} {
		if reach := Classify(net.ParseIP(raw)); reach != HostLocal {
			t.Fatalf("%s was treated as %s, want host_local", raw, reach)
		}
	}
	for _, raw := range []string{"10.0.0.1", "64:ff9b::a00:1", "1.1.1.1", "fc00::1"} {
		if Classify(net.ParseIP(raw)) == HostLocal {
			t.Fatalf("%s was treated as host-local", raw)
		}
	}
}

func TestClassifySeparatesPrivateFromReserved(t *testing.T) {
	for raw, want := range map[string]Reach{
		"10.1.2.3":        Private,
		"172.16.0.1":      Private,
		"192.168.1.1":     Private,
		"100.64.0.1":      Private,
		"fd12::1":         Private,
		"2002:c0a8:101::": Private,
		"192.0.2.1":       Reserved,
		"198.18.0.1":      Reserved,
		"240.0.0.1":       Reserved,
		"fec0::1":         Reserved,
		"2001:db8::1":     Reserved,
		"239.1.1.1":       Reserved,
	} {
		if got := Classify(net.ParseIP(raw)); got != want {
			t.Fatalf("%s = %s, want %s", raw, got, want)
		}
	}
	if got := Classify(nil); got != HostLocal {
		t.Fatalf("unparseable address = %s, want host_local", got)
	}
}

func TestNamesHostLocal(t *testing.T) {
	for _, host := range []string{
		"localhost", "LOCALHOST.", "api.localhost", "127.0.0.1", "[::1]",
		"0.0.0.0", "0.0.0.1", "169.254.169.254", "fe80::1%en0",
		"::ffff:127.0.0.1", "64:ff9b::7f00:1",
	} {
		if !NamesHostLocal(host) {
			t.Fatalf("%q does not name the host", host)
		}
	}
	for _, host := range []string{
		"example.com", "localhost.example.com", "10.0.0.1", "fc00::1", "",
	} {
		if NamesHostLocal(host) {
			t.Fatalf("%q was treated as naming the host", host)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		raw  string
		want Target
		ok   bool
	}{
		{"https://Example.COM/path", Target{"https", "example.com", 443}, true},
		{"http://127.0.0.1:8080/x", Target{"http", "127.0.0.1", 8080}, true},
		{"http://[::1]/", Target{"http", "::1", 80}, true},
		{"https://example.com./", Target{"https", "example.com", 443}, true},
		{"example.com", Target{"https", "example.com", 443}, true},
		{"example.com:8443", Target{"https", "example.com", 8443}, true},
		{"localhost", Target{"https", "localhost", 443}, true},
		{"[::1]:9000", Target{"https", "::1", 9000}, true},
		{"ftp://example.com:21/", Target{"ftp", "example.com", 21}, true},
		{"ftp://example.com/", Target{}, false},
		{"chrome://settings", Target{}, false},
		{"http://example.com:0/", Target{}, false},
		{"file:///etc/passwd", Target{}, false},
		{"user@example.com", Target{}, false},
		{"example.com:0", Target{}, false},
		{"hello", Target{}, false},
		{"golang docs", Target{}, false},
		{"", Target{}, false},
	}
	for _, test := range cases {
		got, err := ParseTarget(test.raw)
		if (err == nil) != test.ok {
			t.Fatalf("%q err=%v want ok=%v", test.raw, err, test.ok)
		}
		if test.ok && got != test.want {
			t.Fatalf("%q = %+v want %+v", test.raw, got, test.want)
		}
	}
	if key := (Target{"https", "::1", 443}).Key(); key != "https://[::1]:443" {
		t.Fatalf("key = %q", key)
	}
}

func TestURLPortRejectsExplicitZeroAndUnknownScheme(t *testing.T) {
	for raw, want := range map[string]uint16{
		"http://example.com/path":       80,
		"HTTPS://example.com/path":      443,
		"http://example.com:8080/path":  8080,
		"http://example.com:0/path":     0,
		"ws://example.com/socket":       0,
		"gopher://example.com:70/":      70,
		"http://example.com:65536/path": 0,
	} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		port, err := URLPort(parsed)
		if want == 0 {
			if err == nil {
				t.Fatalf("%s resolved to port %d", raw, port)
			}
			continue
		}
		if err != nil || port != want {
			t.Fatalf("%s port=%d err=%v want %d", raw, port, err, want)
		}
	}
}

func TestSplitAuthority(t *testing.T) {
	for _, test := range []struct {
		value string
		host  string
		port  uint16
		ok    bool
	}{
		{"example.com", "example.com", 443, true},
		{"example.com:8443", "example.com", 8443, true},
		{"[::1]", "::1", 443, true},
		{"[::1]:80", "::1", 80, true},
		{"example.com:0", "", 0, false},
		{"example.com:http", "", 0, false},
		{"::1", "", 0, false},
	} {
		host, port, err := SplitAuthority(test.value, 443)
		if (err == nil) != test.ok || host != test.host || port != test.port {
			t.Fatalf("%q = %q %d %v", test.value, host, port, err)
		}
	}
}

func TestNormalizeMethods(t *testing.T) {
	methods, err := NormalizeMethods([]string{"get", " POST ", "", "GET"})
	if err != nil || len(methods) != 2 || methods[0] != "GET" || methods[1] != "POST" {
		t.Fatalf("methods=%v err=%v", methods, err)
	}
	if _, err := NormalizeMethods([]string{"GET X"}); err == nil {
		t.Fatal("method with whitespace was accepted")
	}
}
