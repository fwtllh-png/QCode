package goproxy

import "testing"

func TestParseRequestAcceptsGoproxyShapes(t *testing.T) {
	request, err := ParseRequest("/example.com/qcode/testmod/@v/v1.2.3.info")
	if err != nil || request.Module != "example.com/qcode/testmod" ||
		request.Version != "v1.2.3" || request.Kind != KindInfo {
		t.Fatalf("info = %+v err=%v", request, err)
	}
	list, err := ParseRequest("/example.com/!azure/mod/@v/list")
	if err != nil || list.Module != "example.com/Azure/mod" || list.Kind != KindList {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if _, err := ParseRequest("/example.com/mod/@v/v1.info?host=evil"); err == nil {
		t.Fatal("query should be rejected")
	}
	if _, err := ParseRequest("/../evil/@v/v1.info"); err == nil {
		t.Fatal("parent path should be rejected")
	}
}

func TestMatchPrefix(t *testing.T) {
	if !MatchPrefix("example.com/qcode/testmod", []string{"example.com/qcode/"}) {
		t.Fatal("prefix should match nested module")
	}
	if MatchPrefix("evil.com/mod", []string{"example.com/qcode/"}) {
		t.Fatal("unrelated module matched")
	}
	if !MatchPrefix("evil.com/mod", []string{"*"}) {
		t.Fatal("host GOPROXY star prefix must match every module")
	}
}
