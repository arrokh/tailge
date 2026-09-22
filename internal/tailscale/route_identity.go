package tailscale

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	Service     string
	Path        string
	Target      nettarget.Target
	Mode        exposuredata.ExposureMode
	URL         string
	Backend     string
}

func IdentityOf(route exposuredata.ExposureRoute) RouteIdentity {
	return RouteIdentity{ID: route.ID, ProviderKey: route.ProviderKey, Service: route.Service, Path: route.Path, Target: route.Target, Mode: route.Mode, URL: route.URL, Backend: route.Backend}
}

func (identity RouteIdentity) CanonicalKey() string {
	return strings.Join([]string{identity.ID, identity.ProviderKey, identity.Service, identity.Path, identity.Target.Key(), string(identity.Mode), identity.URL, identity.Backend}, "\x00")
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

func targetArgument(target nettarget.Target) string {
	target = target.Normalized()
	wildcard := target.Address == "0.0.0.0" || target.Address == "::"
	if wildcard {
		if target.Address == "0.0.0.0" {
			target.Address = "127.0.0.1"
		} else {
			target.Address = "::1"
		}
	}
	if !wildcard && nettarget.ScopeForAddress(target.Address) == nettarget.ScopeLoopback {
		if strings.Contains(target.Address, ":") {
			return "http://localhost:" + strconv.Itoa(target.Port)
		}
		return target.String()
	}
	return "http://" + target.String()
}

func targetArgumentForTransport(target nettarget.Target, transport string) string {
	target = target.Normalized()
	wildcard := target.Address == "0.0.0.0" || target.Address == "::"
	if wildcard {
		if target.Address == "0.0.0.0" {
			target.Address = "127.0.0.1"
		} else {
			target.Address = "::1"
		}
	}
	host := target.String()
	if !wildcard && nettarget.ScopeForAddress(target.Address) == nettarget.ScopeLoopback && strings.Contains(target.Address, ":") {
		host = "localhost:" + strconv.Itoa(target.Port)
	}
	if transport == "tcp" {
		return "tcp://" + host
	}
	return "http://" + host
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
