package exposuredata

import (
	"strings"
	"testing"
)

func TestNormalizeHTTPPathRequiresOneNamedSlug(t *testing.T) {
	for _, input := range []string{"api", "/api", "service-v2", "my-site-3000"} {
		got, err := NormalizeHTTPPath(input)
		if err != nil || !strings.HasPrefix(got, "/") {
			t.Errorf("NormalizeHTTPPath(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "/", "api/v2", "My API", "-api", "api-", strings.Repeat("a", 64)} {
		if got, err := NormalizeHTTPPath(input); err == nil {
			t.Errorf("NormalizeHTTPPath(%q) = %q, want error", input, got)
		}
	}
}

func TestGeneratedHTTPPathUsesNormalizedProcessAndPort(t *testing.T) {
	cases := []struct {
		name, process string
		port          int
		want          string
	}{
		{name: "normalized process", process: "My Node/Server", port: 4321, want: "/my-node-server-4321"},
		{name: "fallback", process: "---", port: 8080, want: "/service-8080"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := GeneratedHTTPPath(test.process, test.port)
			if err != nil || got != test.want {
				t.Fatalf("GeneratedHTTPPath(%q, %d) = %q, %v; want %q", test.process, test.port, got, err, test.want)
			}
		})
	}
	if _, err := GeneratedHTTPPath("api", 0); err == nil {
		t.Fatal("invalid local port was accepted")
	}
}
