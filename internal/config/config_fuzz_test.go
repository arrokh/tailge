package config

import "testing"

func FuzzParseNeverPanics(f *testing.F) {
	for _, seed := range []string{"version: 1\n", "refresh_interval: 5s\n", "version: [", "token: secret\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _, _ = Parse(text)
	})
}
