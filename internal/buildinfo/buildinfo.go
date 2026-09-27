// Package buildinfo exposes metadata injected into Tailge binaries at build time.
package buildinfo

import "strings"

const RepositoryURL = "https://github.com/arrokh/tailge"

// Commit is set by the repository build targets. Direct Go builds retain the
// development fallback.
var Commit = "dev"

// ShortCommit returns a safe, concise commit identifier for user-facing output.
func ShortCommit() string {
	commit := strings.TrimSpace(Commit)
	if commit == "dev" || commit == "" {
		return "dev"
	}

	runes := []rune(commit)
	if len(runes) < 7 || len(runes) > 40 {
		return "dev"
	}
	for _, r := range runes {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "dev"
		}
	}
	if len(runes) > 12 {
		commit = string(runes[:12])
	}
	return commit
}
