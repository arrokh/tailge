package exposuredata

import (
	"fmt"
	"regexp"
	"strings"
)

var invalidPathSlug = regexp.MustCompile(`[^a-z0-9]+`)
var validPathSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// NormalizeHTTPPath validates a user-supplied single-segment service slug and
// returns its canonical mount path. Root and nested paths are deliberately not
// accepted: named services own one explicit path segment.
func NormalizeHTTPPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "/")
	if len(value) == 0 || len(value) > 63 || strings.Contains(value, "/") || !validPathSlug.MatchString(value) {
		return "", fmt.Errorf("HTTP service path must be a 1-63 character lowercase slug (letters, digits, hyphens), not the root path")
	}
	return "/" + value, nil
}

// GeneratedHTTPPath creates a stable default path from the discovered process
// name and local port. The returned path is persisted in provider state, so
// future process-name changes cannot silently rename an existing route.
func GeneratedHTTPPath(processName string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("HTTP service path requires a valid listener port")
	}
	name := strings.ToLower(strings.TrimSpace(processName))
	name = invalidPathSlug.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "service"
	}
	suffix := fmt.Sprintf("-%d", port)
	if len(name)+len(suffix) > 63 {
		name = strings.TrimRight(name[:63-len(suffix)], "-")
	}
	return "/" + name + suffix, nil
}
