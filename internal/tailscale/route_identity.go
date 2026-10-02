package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/arrokh/tailge/internal/exposuredata"
	nettarget "github.com/arrokh/tailge/internal/target"
)

// RouteIdentity is the provider-independent identity retained for exact
// observation, hashing, verification, and rollback. Provider-specific command
// selectors remain private to this package.
type RouteIdentity struct {
	ID          string
	ProviderKey string
	Kind        exposuredata.RouteKind
	Service     string
	Path        string
	Target      nettarget.Target
	Mode        exposuredata.ExposureMode
	URL         string
	Backend     string
}

func IdentityOf(route exposuredata.ExposureRoute) RouteIdentity {
	return RouteIdentity{ID: route.ID, ProviderKey: route.ProviderKey, Kind: route.Kind, Service: route.Service, Path: route.Path, Target: route.Target, Mode: route.Mode, URL: route.URL, Backend: route.Backend}
}

func (identity RouteIdentity) CanonicalKey() string {
	return strings.Join([]string{identity.ID, identity.ProviderKey, string(identity.Kind), identity.Service, identity.Path, identity.Target.Key(), string(identity.Mode), identity.URL, identity.Backend}, "\x00")
}

func RouteFingerprint(route exposuredata.ExposureRoute) string {
	hash := sha256.Sum256([]byte(IdentityOf(route).CanonicalKey()))
	return hex.EncodeToString(hash[:])
}

// ListenerSelector is the exact public listener syntax accepted by Serve and
// Funnel. Service-style provider identities are deliberately not parsed here:
// they do not identify a deterministic listener port.
type ListenerSelector struct {
	Mode      exposuredata.ExposureMode
	Transport string
	Port      int
}

func ParseListenerSelector(value string, expectedMode exposuredata.ExposureMode) (ListenerSelector, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 || parts[0] != string(expectedMode) {
		return ListenerSelector{}, fmt.Errorf("selector mode does not match requested exposure mode")
	}
	service := parts[1]
	if !validProviderService(service) || (!strings.HasPrefix(service, "https=") && !strings.HasPrefix(service, "tcp=")) {
		return ListenerSelector{}, fmt.Errorf("selector is not a deterministic listener selector")
	}
	fields := strings.SplitN(service, "=", 2)
	port, err := strconv.Atoi(fields[1])
	if err != nil || port < 1 || port > 65535 {
		return ListenerSelector{}, fmt.Errorf("selector has an invalid listener port")
	}
	return ListenerSelector{Mode: expectedMode, Transport: fields[0], Port: port}, nil
}

func hasSubcommand(help, command string) bool {
	for _, field := range strings.Fields(help) {
		if field == command {
			return true
		}
	}
	return false
}

func hasValueFlag(help, wanted string) bool {
	wanted = strings.ToLower(wanted)
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		for i, raw := range fields {
			field := strings.Trim(strings.ToLower(raw), "()[],:;")
			if field != wanted && !strings.HasPrefix(field, wanted+"=") {
				continue
			}
			if strings.Contains(field, "=") {
				return true
			}
			if i+1 < len(fields) && strings.EqualFold(strings.Trim(fields[i+1], "()[],:;"), "value") {
				return true
			}
		}
	}
	return false
}

func exactFlagOff(help string) bool {
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		for i, raw := range fields {
			field := strings.Trim(strings.ToLower(raw), "()[],:;")
			if field != "--tcp" && field != "--https" && !strings.HasPrefix(field, "--tcp=") && !strings.HasPrefix(field, "--https=") {
				continue
			}
			for j := i + 1; j < len(fields) && j <= i+2; j++ {
				if strings.ToLower(fields[j]) == "off" {
					return true
				}
			}
		}
	}
	return false
}

func isTargetField(key string) bool {
	switch strings.ToLower(key) {
	case "proxy", "target", "backend", "handler":
		return true
	default:
		return false
	}
}

// localBackendTarget makes wildcard listener addresses dialable while retaining
// every specific address unchanged. The wildcard itself is never a valid dial
// destination, and loopback is necessarily included in its bind scope.
func localBackendTarget(target nettarget.Target) nettarget.Target {
	target = target.Normalized()
	switch target.Address {
	case "0.0.0.0":
		target.Address = "127.0.0.1"
	case "::":
		target.Address = "::1"
	}
	return target
}

// HTTPPathBackendArgument preserves a specific listener address. Wildcard
// listeners use the corresponding loopback address because a wildcard cannot
// be dialed directly; alternate aliases such as localhost remain explicit.
func HTTPPathBackendArgument(target nettarget.Target) string {
	return "http://" + localBackendTarget(target).String()
}

// HTTPSBackendArgument optionally uses localhost for IPv6 listeners when the
// operator explicitly accepts the host-alias resolution tradeoff.
func HTTPSBackendArgument(target nettarget.Target, allowLocalhostForIPv6 bool) string {
	target = target.Normalized()
	if allowLocalhostForIPv6 && (target.Address == "::" || target.Address == "::1") {
		return "http://localhost:" + strconv.Itoa(target.Port)
	}
	return HTTPPathBackendArgument(target)
}

// HTTPPathBackendMatches compares backend authority while accepting the
// unbracketed IPv6 spelling emitted by some Tailscale status versions. It
// still requires local HTTP, the exact parsed address/port, and no mounted
// backend path of its own.
func HTTPPathBackendMatches(observed, expected string) bool {
	observedTarget, observedOK := parseHTTPPathBackend(observed)
	expectedTarget, expectedOK := parseHTTPPathBackend(expected)
	return observedOK && expectedOK && observedTarget.Key() == expectedTarget.Key()
}

func parseHTTPPathBackend(value string) (nettarget.Target, bool) {
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok || !strings.EqualFold(scheme, "http") {
		return nettarget.Target{}, false
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 && rest[slash:] != "/" {
		return nettarget.Target{}, false
	}
	parsed, err := nettarget.ParseTarget(value, "tcp")
	if err != nil {
		return nettarget.Target{}, false
	}
	return parsed.Normalized(), true
}

// rawTCPBackendArgument restores an observed raw-TCP backend without losing its
// transport scheme. Scheme-less status values are accepted only when their
// exact normalized target matches the selected listener (or its wildcard's
// corresponding loopback backend).
func rawTCPBackendArgument(selected nettarget.Target, observed string) (string, error) {
	expected := localBackendTarget(selected)
	if strings.TrimSpace(observed) == "" {
		return "tcp://" + expected.String(), nil
	}
	rawBackend := strings.TrimSpace(observed)
	if scheme, remainder, ok := strings.Cut(rawBackend, "://"); ok {
		if !strings.EqualFold(scheme, "tcp") {
			return "", fmt.Errorf("raw TCP backend uses a non-TCP scheme")
		}
		rawBackend = remainder
	}
	backend, err := nettarget.ParseTarget(observed, "tcp")
	if err != nil {
		return "", fmt.Errorf("raw TCP backend is invalid: %w", err)
	}
	backend = backend.Normalized()
	if backend.Key() != expected.Key() {
		return "", fmt.Errorf("raw TCP backend does not match the exact listener address")
	}
	backendAddress := backend.String()
	if rawAddress, rawPort, err := net.SplitHostPort(rawBackend); err == nil && strings.EqualFold(rawAddress, "localhost") && rawPort == strconv.Itoa(backend.Port) {
		backendAddress = net.JoinHostPort(rawAddress, rawPort)
	}
	return "tcp://" + backendAddress, nil
}

func validateHandlerSelection(service, path, backend string) error {
	if service != "" && !validProviderService(service) {
		return fmt.Errorf("route service selector is invalid")
	}
	if path != "" && (!strings.HasPrefix(path, "/") || strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' || r == '"' }) >= 0) {
		return fmt.Errorf("route handler path is invalid")
	}
	if len(backend) > 4096 || strings.IndexFunc(backend, func(r rune) bool { return unicode.IsSpace(r) || r < 0x20 || r == 0x7f || r == '\\' || r == '"' }) >= 0 {
		return fmt.Errorf("route backend contains unsupported characters")
	}
	return nil
}

func ProviderKeyForTargetWithCapabilities(mode exposuredata.ExposureMode, target nettarget.Target, caps Capabilities) string {
	transport := "tcp"
	port := target.Normalized().Port
	if mode == exposuredata.ExposureFunnel && !caps.FunnelLegacy {
		// Funnel's public listener is independent of the local backend port.
		port = 10000
	}
	return providerKeyForTransportAndPort(mode, transport, port)
}

func providerKeyForTransportAndPort(mode exposuredata.ExposureMode, transport string, port int) string {
	if (mode != exposuredata.ExposureServe && mode != exposuredata.ExposureFunnel) || (transport != "https" && transport != "tcp") || port < 1 || port > 65535 {
		return ""
	}
	return string(mode) + ":" + transport + "=" + strconv.Itoa(port)
}

func sameProviderEndpoint(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftParts := strings.SplitN(left, ":", 2)
	rightParts := strings.SplitN(right, ":", 2)
	if len(leftParts) != 2 || len(rightParts) != 2 {
		return false
	}
	leftSelector, rightSelector := leftParts[1], rightParts[1]
	if leftPort := strings.TrimPrefix(strings.TrimPrefix(leftSelector, "https="), "tcp="); leftPort != leftSelector {
		if rightPort := strings.TrimPrefix(strings.TrimPrefix(rightSelector, "https="), "tcp="); rightPort != rightSelector {
			return leftPort == rightPort
		}
	}
	return leftSelector == rightSelector
}

func validProviderService(value string) bool {
	if strings.HasPrefix(value, "tcp=") || strings.HasPrefix(value, "https=") {
		port, err := strconv.Atoi(strings.SplitN(value, "=", 2)[1])
		return err == nil && port >= 1 && port <= 65535 && strings.Count(value, "=") == 1
	}
	service := strings.TrimPrefix(value, "svc:")
	if service == value && (strings.HasPrefix(value, "-") || strings.Contains(value, "=")) {
		return false
	}
	if service == "" || len(service) > 256 {
		return false
	}
	for index, r := range service {
		if index == 0 && r == '-' {
			return false
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._/-:", r) {
			continue
		}
		return false
	}
	return true
}

func routeSelectorForPort(mode exposuredata.ExposureMode, transport string, port int) string {
	if port < 1 || port > 65535 {
		return ""
	}
	if transport == "" {
		transport = "https"
	}
	if mode == exposuredata.ExposureServe || mode == exposuredata.ExposureFunnel {
		return string(mode) + ":" + transport + "=" + strconv.Itoa(port)
	}
	return ""
}

func portNumber(value string) int {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

func observedHTTPHandlerURL(base, handler string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Hostname() == "" {
		return base
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	port := parsed.Port()
	if port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if handler == "" {
		handler = parsed.Path
	}
	if handler == "" {
		handler = "/"
	}
	return (&url.URL{Scheme: "https", Host: host, Path: handler}).String()
}

func handlerPath(path []string) string {
	for index := len(path) - 1; index >= 0; index-- {
		if strings.HasPrefix(path[index], "/") {
			return path[index]
		}
	}
	return ""
}

func routeID(mode exposuredata.ExposureMode, target nettarget.Target, url, service, handler, backend, path string) string {
	return nettarget.StableID(string(mode), target.Key(), url, service, handler, backend, path)
}

func dedupRoutes(routes []exposuredata.ExposureRoute) []exposuredata.ExposureRoute {
	seen := map[string]bool{}
	result := make([]exposuredata.ExposureRoute, 0, len(routes))
	for _, route := range routes {
		key := string(route.Mode) + ":" + route.ID
		if route.ID == "" {
			key = string(route.Mode) + ":" + route.Target.Key() + ":" + route.URL
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, route)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Target.Port != result[j].Target.Port {
			return result[i].Target.Port < result[j].Target.Port
		}
		return result[i].Target.Key() < result[j].Target.Key()
	})
	return result
}
