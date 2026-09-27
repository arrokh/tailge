package buildinfo

import "testing"

func TestCommitDefaultsToDevelopmentBuild(t *testing.T) {
	if Commit != "dev" {
		t.Fatalf("default commit=%q, want dev", Commit)
	}
}

func TestShortCommit(t *testing.T) {
	original := Commit
	t.Cleanup(func() { Commit = original })

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty fallback", value: "", want: "dev"},
		{name: "development fallback", value: "dev", want: "dev"},
		{name: "short hash", value: "3d16efb", want: "3d16efb"},
		{name: "long hash", value: "3d16efbb9058b146749de0d81aa7dd5eede3e9da", want: "3d16efbb9058"},
		{name: "invalid value", value: "not-a-hash\x1b[31m", want: "dev"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			Commit = test.value
			if got := ShortCommit(); got != test.want {
				t.Fatalf("ShortCommit()=%q, want %q", got, test.want)
			}
		})
	}
}
