package model

import (
	"strings"
	"testing"
)

func TestTargetNormalizationAndScopes(t *testing.T) {
	tests := []struct {
		input, wantAddress string
		scope              NetworkScope
	}{
		{"localhost", "127.0.0.1", ScopeLoopback},
		{"[::]", "::", ScopeWildcard},
		{"0.0.0.0", "0.0.0.0", ScopeWildcard},
		{"192.168.1.7", "192.168.1.7", ScopeLocal},
	}
	for _, test := range tests {
		target := Target{Address: test.input, Port: 80, Protocol: "TCP"}.Normalized()
		if target.Address != test.wantAddress || ScopeForAddress(target.Address) != test.scope {
			t.Errorf("%q normalized to %#v scope=%s", test.input, target, ScopeForAddress(target.Address))
		}
	}
}

func TestTargetValidationRejectsUnsafeAddressText(t *testing.T) {
	if err := (Target{Address: "127.0.0.1\n--help", Port: 8080, Protocol: "tcp"}).Validate(); err == nil {
		t.Fatal("unsafe address text was accepted")
	}
}

func TestParseTargetAcceptsPortsAndURLsButValidatesBounds(t *testing.T) {
	if target, err := ParseTarget("8080", "tcp"); err != nil || target.Address != "127.0.0.1" || target.Port != 8080 {
		t.Fatalf("bare port parse = %#v, err=%v", target, err)
	}
	if target, err := ParseTarget("http://localhost:3000/path", "tcp"); err != nil || target.Address != "127.0.0.1" || target.Port != 3000 {
		t.Fatalf("URL parse = %#v, err=%v", target, err)
	}
	if target, err := ParseTarget("http://::1:4322", "tcp"); err != nil || target.Address != "::1" || target.Port != 4322 {
		t.Fatalf("unbracketed IPv6 URL parse = %#v, err=%v", target, err)
	}
	if target, err := ParseTarget("tcp://127.0.0.1:3000", "tcp"); err != nil || target.Address != "127.0.0.1" || target.Port != 3000 {
		t.Fatalf("raw TCP URL parse = %#v, err=%v", target, err)
	}
	for _, input := range []string{"0", "65536", "localhost", "127.0.0.1:abc", "127.0.0.1:8080\n", "http://user:secret@127.0.0.1:3000", "http://127.0.0.1:3000?token=secret", "file://127.0.0.1:3000"} {
		if _, err := ParseTarget(input, "tcp"); err == nil {
			t.Errorf("accepted invalid target %q", input)
		}
	}
}

func TestParseTargetErrorsDoNotEchoSensitiveInput(t *testing.T) {
	for _, input := range []string{"http://user:secret@127.0.0.1:3000", "127.0.0.1:token=secret"} {
		_, err := ParseTarget(input, "tcp")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("target error leaked input %q: %v", input, err)
		}
	}
}

func FuzzParseTargetNeverPanics(f *testing.F) {
	for _, seed := range []string{"8080", "127.0.0.1:3000", "[::1]:443", "", "token=secret"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_, _ = ParseTarget(value, "tcp")
	})
}
