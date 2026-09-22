package target

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

type NetworkScope string

const (
	ScopeLoopback NetworkScope = "loopback"
	ScopeLocal    NetworkScope = "local-network"
	ScopeWildcard NetworkScope = "wildcard"
	ScopeUnknown  NetworkScope = "unknown"
)

type Target struct {
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

func (t Target) Validate() error {
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if strings.IndexFunc(t.Protocol, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 || strings.ToLower(strings.TrimSpace(t.Protocol)) != "tcp" {
		return fmt.Errorf("protocol is unsupported; only tcp is supported")
	}
	if strings.IndexFunc(t.Address, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' || r == '"' }) >= 0 {
		return fmt.Errorf("address contains unsupported control or quoting characters")
	}
	address := strings.TrimSpace(t.Address)
	if address == "" {
		return fmt.Errorf("address is required")
	}
	if len(address) > 255 || strings.IndexFunc(address, func(r rune) bool { return r <= ' ' || r == '\\' || r == '"' }) >= 0 {
		return fmt.Errorf("address contains unsupported whitespace or characters")
	}
	return nil
}

func (t Target) Normalized() Target {
	address := NormalizeAddress(t.Address)
	protocol := strings.ToLower(strings.TrimSpace(t.Protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	return Target{Address: address, Port: t.Port, Protocol: protocol}
}

func (t Target) Key() string {
	n := t.Normalized()
	return n.Protocol + ":" + n.Address + ":" + strconv.Itoa(n.Port)
}

// TargetsMatch applies the directional correlation policy used when an
// observed route is compared with a local listener. Exact addresses match;
// loopback families correlate; a loopback route may correlate with a wildcard
// listener; wildcard routes correlate only with wildcard listeners. Callers
// retain the listener's scope warning and must still reject ambiguity.
func TargetsMatch(route, listener Target) bool {
	route, listener = route.Normalized(), listener.Normalized()
	if route.Protocol != listener.Protocol || route.Port != listener.Port {
		return false
	}
	if route.Address == listener.Address {
		return true
	}
	routeScope, listenerScope := ScopeForAddress(route.Address), ScopeForAddress(listener.Address)
	if routeScope == ScopeLoopback && listenerScope == ScopeLoopback {
		return true
	}
	if routeScope == ScopeWildcard && listenerScope == ScopeWildcard {
		return true
	}
	return routeScope == ScopeLoopback && listenerScope == ScopeWildcard
}

func (t Target) String() string {
	n := t.Normalized()
	return net.JoinHostPort(n.Address, strconv.Itoa(n.Port))
}

func ParseTarget(value string, protocol string) (Target, error) {
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' || r == '"' }) >= 0 {
		return Target{}, fmt.Errorf("target contains unsupported control or quoting characters")
	}
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") {
		u, err := neturlParse(value)
		if err != nil {
			return Target{}, fmt.Errorf("invalid target: %w", err)
		}
		value = u.Host
	}
	bracketedHost := strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]")
	if bracketedHost {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	// Tailscale status currently emits IPv6 proxy URLs such as
	// "http://::1:4322" without URL brackets. Interpret the final colon as
	// the port separator only when the preceding text is a valid IPv6 address;
	// ordinary malformed targets remain rejected below.
	if strings.Count(value, ":") > 1 && !bracketedHost && !strings.HasPrefix(value, "[") {
		if separator := strings.LastIndexByte(value, ':'); separator > 0 {
			host, port := value[:separator], value[separator+1:]
			if net.ParseIP(host) != nil {
				value = "[" + host + "]:" + port
			}
		}
	}
	address, portText, err := net.SplitHostPort(value)
	if err != nil {
		// A bare port is intentionally accepted as localhost for CLI convenience.
		if p, parseErr := strconv.Atoi(value); parseErr == nil {
			address, portText = "127.0.0.1", strconv.Itoa(p)
		} else {
			return Target{}, fmt.Errorf("target must be address:port or port")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return Target{}, fmt.Errorf("invalid target port")
	}
	t := Target{Address: address, Port: port, Protocol: protocol}
	if t.Protocol == "" {
		t.Protocol = "tcp"
	}
	if err := t.Validate(); err != nil {
		return Target{}, err
	}
	return t.Normalized(), nil
}

// neturlParse is kept small to make target parsing explicit and testable.
func neturlParse(value string) (*urlValue, error) {
	parts := strings.SplitN(value, "://", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid URL")
	}
	if scheme := strings.ToLower(parts[0]); scheme != "http" && scheme != "https" && scheme != "tcp" {
		return nil, fmt.Errorf("unsupported URL scheme")
	}
	rest := parts[1]
	if strings.ContainsAny(rest, "?#") {
		return nil, fmt.Errorf("URL query and fragment are not accepted")
	}
	host := rest
	if slash := strings.IndexByte(host, '/'); slash >= 0 {
		host = host[:slash]
	}
	if host == "" || strings.ContainsAny(host, "@\\") {
		return nil, fmt.Errorf("URL host or userinfo is invalid")
	}
	return &urlValue{Host: host}, nil
}

type urlValue struct{ Host string }

func NormalizeAddress(address string) string {
	address = strings.TrimSpace(address)
	address = strings.TrimPrefix(address, "[")
	address = strings.TrimSuffix(address, "]")
	if zone := strings.LastIndexByte(address, '%'); zone > 0 && strings.Contains(address, ":") {
		address = address[:zone]
	}
	lower := strings.ToLower(address)
	switch lower {
	case "", "*":
		return "0.0.0.0"
	case "localhost":
		return "127.0.0.1"
	case "0.0.0.0":
		return "0.0.0.0"
	case "::", "0:0:0:0:0:0:0:0":
		return "::"
	}
	if ip := net.ParseIP(address); ip != nil {
		return ip.String()
	}
	return lower
}

func ScopeForAddress(address string) NetworkScope {
	a := NormalizeAddress(address)
	if a == "0.0.0.0" || a == "::" {
		return ScopeWildcard
	}
	ip := net.ParseIP(a)
	if ip == nil {
		return ScopeUnknown
	}
	if ip.IsLoopback() {
		return ScopeLoopback
	}
	return ScopeLocal
}

func StableID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
