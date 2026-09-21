package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type ErrorCode string

const (
	ErrInvalidInput ErrorCode = "invalid_input"
	ErrDependency   ErrorCode = "dependency_unavailable"
	ErrPermission   ErrorCode = "permission_denied"
	ErrTimeout      ErrorCode = "timeout"
	ErrCancelled    ErrorCode = "cancelled"
	ErrOperation    ErrorCode = "operation_failed"
	ErrVerification ErrorCode = "verification_failed"
	ErrConfig       ErrorCode = "config_failure"
	ErrInterrupted  ErrorCode = "interrupted"
	ErrUnknown      ErrorCode = "unknown"
	ErrUnsafe       ErrorCode = "unsafe_operation"
	ErrAmbiguous    ErrorCode = "ambiguous_target"
	ErrUnsupported  ErrorCode = "unsupported"
)

func (c ErrorCode) ExitCode() int {
	switch c {
	case ErrInvalidInput, ErrAmbiguous, ErrUnsupported:
		return 2
	case ErrDependency:
		return 3
	case ErrPermission:
		return 4
	case ErrTimeout, ErrCancelled:
		return 5
	case ErrOperation, ErrUnsafe:
		return 6
	case ErrVerification, ErrUnknown:
		return 7
	case ErrConfig:
		return 8
	case ErrInterrupted:
		return 130
	default:
		return 1
	}
}

type AppError struct {
	Code        ErrorCode `json:"code"`
	Source      string    `json:"source"`
	Message     string    `json:"message"`
	Retryable   bool      `json:"retryable"`
	State       string    `json:"state"`
	Remediation string    `json:"remediation"`
	Exit        int       `json:"-"`
	Cause       error     `json:"-"`
}

func (e *AppError) Error() string {
	if e == nil {
		return ""
	}
	if e.Source == "" {
		return e.Message
	}
	return e.Source + ": " + e.Message
}

func (e *AppError) Unwrap() error { return e.Cause }

func NewError(code ErrorCode, source, message string, retryable bool, state, remediation string) *AppError {
	return &AppError{Code: code, Source: source, Message: message, Retryable: retryable, State: state, Remediation: remediation, Exit: code.ExitCode()}
}

func WrapError(code ErrorCode, source, message string, retryable bool, state, remediation string, cause error) *AppError {
	e := NewError(code, source, message, retryable, state, remediation)
	e.Cause = cause
	return e
}

func AsAppError(err error) *AppError {
	if err == nil {
		return nil
	}
	var appErr *AppError
	if errors.As(err, &appErr) && appErr != nil {
		return appErr
	}
	return NewError(ErrUnknown, "tailge", err.Error(), false, "unknown", "Run the command again after reviewing the diagnostics.")
}

type SafeError struct {
	Code        ErrorCode `json:"code"`
	Source      string    `json:"source"`
	Message     string    `json:"message"`
	Retryable   bool      `json:"retryable"`
	State       string    `json:"state"`
	Remediation string    `json:"remediation"`
	ExitCode    int       `json:"exit_code,omitempty"`
}

func (e *AppError) Safe() SafeError {
	if e == nil {
		return SafeError{}
	}
	return SafeError{Code: e.Code, Source: e.Source, Message: e.Message, Retryable: e.Retryable, State: e.State, Remediation: e.Remediation, ExitCode: e.Exit}
}

type NetworkScope string

const (
	ScopeLoopback NetworkScope = "loopback"
	ScopeLocal    NetworkScope = "local-network"
	ScopeWildcard NetworkScope = "wildcard"
	ScopeUnknown  NetworkScope = "unknown"
)

type MetadataQuality string

const (
	MetadataComplete MetadataQuality = "complete"
	MetadataPartial  MetadataQuality = "partial"
	MetadataUnknown  MetadataQuality = "unknown"
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

type Listener struct {
	ID           string          `json:"id"`
	Target       Target          `json:"target"`
	Name         string          `json:"name"`
	PID          int             `json:"pid,omitempty"`
	Process      string          `json:"process,omitempty"`
	CommandLine  string          `json:"command_line,omitempty"`
	ProcessStart string          `json:"process_start,omitempty"`
	Scope        NetworkScope    `json:"scope"`
	Metadata     MetadataQuality `json:"metadata"`
	FirstSeen    time.Time       `json:"first_seen"`
	LastSeen     time.Time       `json:"last_seen"`
}

type ExposureMode string

const (
	ExposureDisabled ExposureMode = "disabled"
	ExposureServe    ExposureMode = "serve"
	ExposureFunnel   ExposureMode = "funnel"
)

func (m ExposureMode) Valid() bool {
	return m == ExposureDisabled || m == ExposureServe || m == ExposureFunnel
}

type Ownership string

const (
	OwnershipManaged  Ownership = "managed"
	OwnershipExternal Ownership = "external"
	OwnershipUnknown  Ownership = "unknown"
)

type ExposureState string

const (
	ExposureActive      ExposureState = "active"
	ExposureInactive    ExposureState = "inactive_configured"
	ExposureUnsupported ExposureState = "unsupported"
	ExposureAmbiguous   ExposureState = "ambiguous"
	ExposureUnknown     ExposureState = "unknown"
	ExposureUnavailable ExposureState = "unavailable"
	ExposureApplying    ExposureState = "applying"
	ExposureSucceeded   ExposureState = "succeeded"
	ExposureFailed      ExposureState = "failed"
	ExposureCancelled   ExposureState = "cancelled"
	ExposureUnverified  ExposureState = "unverified"
)

type ExposureRoute struct {
	ID             string        `json:"id"`
	ProviderKey    string        `json:"provider_key,omitempty"`
	Service        string        `json:"service,omitempty"`
	Path           string        `json:"path,omitempty"`
	Target         Target        `json:"target"`
	Mode           ExposureMode  `json:"mode"`
	URL            string        `json:"url,omitempty"`
	Ownership      Ownership     `json:"ownership"`
	State          ExposureState `json:"state"`
	LastSeen       time.Time     `json:"last_seen"`
	LastVerifiedAt time.Time     `json:"last_verified_at,omitempty"`
	LastError      *SafeError    `json:"last_error,omitempty"`
	Recommendation string        `json:"recommendation,omitempty"`
	Source         string        `json:"source,omitempty"`
	// Backend is retained internally so an exact rollback can reuse the
	// provider's original backend URL and not silently change its scheme/path.
	Backend string `json:"-"`
}

type ListenerSnapshot struct {
	At            time.Time  `json:"at"`
	Authoritative bool       `json:"authoritative"`
	Stale         bool       `json:"stale,omitempty"`
	Listeners     []Listener `json:"listeners"`
	Warnings      []string   `json:"warnings,omitempty"`
	Error         *SafeError `json:"error,omitempty"`
	Source        string     `json:"source"`
}

type ExposureSnapshot struct {
	At            time.Time       `json:"at"`
	Authoritative bool            `json:"authoritative"`
	Stale         bool            `json:"stale,omitempty"`
	Routes        []ExposureRoute `json:"routes"`
	Warnings      []string        `json:"warnings,omitempty"`
	Error         *SafeError      `json:"error,omitempty"`
	Source        string          `json:"source"`
}

type ReadinessStatus string

const (
	ReadinessReady    ReadinessStatus = "ready"
	ReadinessReadOnly ReadinessStatus = "read_only"
	ReadinessNotReady ReadinessStatus = "not_ready"
	ReadinessUnknown  ReadinessStatus = "unknown"
)

type ReadinessCheck struct {
	Name        string          `json:"name"`
	Status      ReadinessStatus `json:"status"`
	Message     string          `json:"message"`
	Remediation string          `json:"remediation,omitempty"`
	CheckedAt   time.Time       `json:"checked_at"`
}

type ModeReadiness struct {
	Mode   ExposureMode     `json:"mode"`
	Status ReadinessStatus  `json:"status"`
	Checks []ReadinessCheck `json:"checks"`
	Probe  bool             `json:"probe_verified"`
	Remote string           `json:"remote_reachability"`
}

type Readiness struct {
	At        time.Time        `json:"at"`
	Binary    string           `json:"binary,omitempty"`
	Version   string           `json:"version,omitempty"`
	Daemon    ReadinessStatus  `json:"daemon"`
	Identity  ReadinessStatus  `json:"identity"`
	Connected ReadinessStatus  `json:"connected"`
	Status    ReadinessStatus  `json:"status"`
	Checks    []ReadinessCheck `json:"checks"`
	Modes     []ModeReadiness  `json:"modes"`
	Warnings  []string         `json:"warnings,omitempty"`
}

type OperationEvent struct {
	OperationID string        `json:"operation_id"`
	Phase       string        `json:"phase"`
	At          time.Time     `json:"at"`
	Duration    string        `json:"duration,omitempty"`
	Target      Target        `json:"target"`
	Mode        ExposureMode  `json:"mode"`
	Capability  string        `json:"capability,omitempty"`
	State       ExposureState `json:"state"`
	ErrorCode   ErrorCode     `json:"error_code,omitempty"`
}

type OperationReceipt struct {
	ID         string     `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at"`
	Command    string     `json:"command,omitempty"`
	ExitCode   int        `json:"exit_code"`
	Stdout     string     `json:"stdout,omitempty"`
	Stderr     string     `json:"stderr,omitempty"`
	Verified   bool       `json:"verified"`
	Error      *SafeError `json:"error,omitempty"`
}

func InactiveRecommendation(t Target) string {
	return fmt.Sprintf("No listener is currently accepting connections on %s. Start the service and refresh, or disable this stale exposure after reviewing its public/private scope.", t.String())
}
