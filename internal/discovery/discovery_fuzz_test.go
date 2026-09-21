package discovery

import (
	"testing"
	"time"
)

func FuzzParseLsofNeverPanics(f *testing.F) {
	for _, seed := range []string{"p1\ncapp\nn127.0.0.1:80\n", "", "ninvalid\n", "p-1\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = ParseLsof(text, time.Unix(1, 0))
	})
}

func FuzzParseSSNeverPanics(f *testing.F) {
	for _, seed := range []string{"tcp LISTEN 0 128 127.0.0.1:80 * users:((\"app\",pid=1,fd=3))", "", "tcp"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = ParseSS(text, time.Unix(1, 0))
	})
}
