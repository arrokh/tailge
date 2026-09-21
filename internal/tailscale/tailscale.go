package tailscale

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/runner"
)

type Exposer interface {
	Capabilities(context.Context) (Capabilities, error)
	List(context.Context) (model.ExposureSnapshot, error)
	Set(context.Context, ExposureChange) (model.OperationReceipt, error)
	Remove(context.Context, RouteSelector, string) (model.OperationReceipt, error)
}

type ExposurePrecondition struct {
	RouteIDsHash  string
	AllRoutesHash string
	RouteIDs      []string
}

type ExposureChange struct {
	Target        model.Target
	Mode          model.ExposureMode
	ProviderKey   string
	Service       string
	Path          string
	Backend       string
	Preconditions ExposurePrecondition
}

type RouteSelector struct {
	ID            string
	Target        *model.Target
	Mode          model.ExposureMode
	Service       string
	Path          string
	Backend       string
	AllRoutesHash string
}

type Capabilities struct {
	Serve        bool
	Funnel       bool
	ExactServe   bool
	ExactFunnel  bool
	ServeHTTPS   bool
	ServeTCP     bool
	FunnelHTTPS  bool
	FunnelTCP    bool
	FunnelLegacy bool
	Service      bool
	Version      string
}

type Adapter struct {
	Runner runner.Runner
	Binary string
	Now    func() time.Time
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func New(r runner.Runner) *Adapter {
	return &Adapter{Runner: r, Binary: findBinary(), Now: time.Now}
}

func findBinary() string {
	path, err := exec.LookPath("tailscale")
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return ""
	}
	return path
}

func (a *Adapter) binaryPath() string {
	if a.Binary != "" {
		return a.Binary
	}
	return findBinary()
}

func (a *Adapter) Version(ctx context.Context) (string, error) {
	if a.binaryPath() == "" {
		return "", model.NewError(model.ErrDependency, "tailscale", "tailscale executable was not found", true, "unavailable", "Install Tailscale and retry.")
	}
	result, err := a.run(ctx, "version")
	if err != nil {
		return "", a.commandError("version", result, err)
	}
	if result.Truncated {
		return "", model.NewError(model.ErrUnknown, "tailscale", "version output was truncated", true, "partial", "Retry with a healthy Tailscale installation.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return "", model.NewError(model.ErrUnknown, "tailscale", "version emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	line := strings.TrimSpace(sanitizeText(strings.SplitN(result.Stdout, "\n", 2)[0]))
	if line == "" {
		return "", model.NewError(model.ErrUnknown, "tailscale", "tailscale version output was empty", true, "unknown", "Retry after checking the Tailscale installation.")
	}
	return line, nil
}

func (a *Adapter) Status(ctx context.Context) (status Status, err error) {
	result, runErr := a.run(ctx, "status", "--json")
	if runErr != nil {
		return Status{}, a.commandError("status", result, runErr)
	}
	if result.Truncated {
		return Status{}, model.NewError(model.ErrUnknown, "tailscale", "status output was truncated", true, "partial", "Retry with a healthy Tailscale installation.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return Status{}, model.NewError(model.ErrUnknown, "tailscale", "status emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	if err := json.Unmarshal([]byte(result.Stdout), &status); err != nil {
		return Status{}, model.WrapError(model.ErrUnknown, "tailscale", "cannot parse status JSON", true, "unknown", "Upgrade or repair Tailscale, then retry.", err)
	}
	if err := validateStatusIPs(status); err != nil {
		return Status{}, model.WrapError(model.ErrUnknown, "tailscale", "status JSON contains an invalid node address", true, "unknown", "Retry after checking the Tailscale status output.", err)
	}
	return status, nil
}

type Status struct {
	BackendState string   `json:"BackendState"`
	AuthURL      string   `json:"AuthURL"`
	HaveNodeKey  bool     `json:"HaveNodeKey"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	Self         struct {
		HostName     string   `json:"HostName"`
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
		Capabilities []string `json:"Capabilities"`
	} `json:"Self"`
}

func validateStatusIPs(status Status) error {
	for _, address := range append(append([]string{}, status.TailscaleIPs...), status.Self.TailscaleIPs...) {
		if net.ParseIP(strings.TrimSpace(address)) == nil {
			return fmt.Errorf("invalid node address %q", sanitizeText(address))
		}
	}
	return nil
}

func hasValidNodeAddress(status Status) bool {
	for _, address := range append(append([]string{}, status.TailscaleIPs...), status.Self.TailscaleIPs...) {
		if net.ParseIP(strings.TrimSpace(address)) != nil {
			return true
		}
	}
	return false
}

func (a *Adapter) Capabilities(ctx context.Context) (Capabilities, error) {
	version, err := a.Version(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	serve, err := a.help(ctx, "serve")
	if err != nil {
		return Capabilities{}, err
	}
	funnel, err := a.help(ctx, "funnel")
	if err != nil {
		return Capabilities{}, err
	}
	serveLower, funnelLower := strings.ToLower(serve), strings.ToLower(funnel)
	serveClear := hasSubcommand(serveLower, "clear")
	serviceFlag := hasValueFlag(serveLower, "--service") || strings.Contains(serveLower, "--service=")
	serveHTTPS := strings.Contains(serveLower, "--https")
	serveTCP := strings.Contains(serveLower, "--tcp")
	serveExactOff := exactFlagOff(serveLower)
	funnelReset := hasSubcommand(funnelLower, "reset")
	funnelLegacy := strings.Contains(funnelLower, "{on|off}") || strings.Contains(funnelLower, "{on,off}")
	funnelHTTPS := strings.Contains(funnelLower, "--https")
	funnelTCP := strings.Contains(funnelLower, "--tcp")
	// The v2 Funnel CLI accepts `tailscale funnel --https=<port> off`
	// (and the equivalent --tcp form), but its --help output omits the
	// positional `off` syntax. Recognize the typed value form; Set and Remove
	// perform bounded read-after-write verification before reporting success.
	// Legacy {on|off} syntax remains supported separately.
	funnelV2ExactOff := hasValueFlag(funnelLower, "--https") || hasValueFlag(funnelLower, "--tcp")
	funnelExactOff := exactFlagOff(funnelLower) || funnelV2ExactOff
	return Capabilities{
		Serve:        hasSubcommand(serveLower, "status"),
		Funnel:       hasSubcommand(funnelLower, "status") && (funnelReset || funnelLegacy || funnelExactOff),
		ExactServe:   (serveClear || serveExactOff) && (serveHTTPS || serveTCP),
		ExactFunnel:  funnelLegacy || (funnelExactOff && (funnelHTTPS || funnelTCP)),
		ServeHTTPS:   serveHTTPS,
		ServeTCP:     serveTCP,
		FunnelHTTPS:  funnelHTTPS,
		FunnelTCP:    funnelTCP,
		FunnelLegacy: funnelLegacy,
		Service:      serviceFlag,
		Version:      version,
	}, nil
}

func (a *Adapter) checkRemovalPrecondition(ctx context.Context, target model.Target, selector RouteSelector, expectedHash string) error {
	snapshot, err := a.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return model.NewError(model.ErrUnknown, "tailscale", "current exposure state is not authoritative", true, "unknown", "Refresh before changing exposure.")
	}
	ids := []string{}
	found := false
	for _, route := range snapshot.Routes {
		if targetMatches(route.Target, target) {
			ids = append(ids, route.ID)
			if route.ProviderKey == selector.ID && (selector.Mode == "" || selector.Mode == model.ExposureDisabled || route.Mode == selector.Mode) && route.Service == selector.Service && route.Path == selector.Path && route.Backend == selector.Backend {
				found = true
			}
		}
	}
	if actual := hashIDs(ids); actual != expectedHash {
		return model.NewError(model.ErrUnsafe, "tailscale", "exposure changed since preflight", true, "changed", "Refresh, review the new route set, and retry.")
	}
	if selector.AllRoutesHash != "" && RoutesHash(snapshot.Routes) != selector.AllRoutesHash {
		return model.NewError(model.ErrUnsafe, "tailscale", "exposure configuration changed since preflight", true, "changed", "Refresh, review all routes, and retry.")
	}
	if !found {
		return model.NewError(model.ErrUnsafe, "tailscale", "exact route selector is no longer present for the target", true, "changed", "Refresh and select the current route before retrying.")
	}
	return nil
}

func (a *Adapter) checkPreconditionSnapshot(ctx context.Context, target model.Target, precondition ExposurePrecondition) (model.ExposureSnapshot, error) {
	if precondition.RouteIDsHash == "" || precondition.AllRoutesHash == "" {
		return model.ExposureSnapshot{}, model.NewError(model.ErrUnsafe, "tailscale", "precondition requires route and global fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
	}
	snapshot, err := a.List(ctx)
	if err != nil {
		return model.ExposureSnapshot{}, err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return model.ExposureSnapshot{}, model.NewError(model.ErrUnknown, "tailscale", "current exposure state is not authoritative", true, "unknown", "Refresh before changing exposure.")
	}
	if precondition.RouteIDsHash != "" {
		ids := []string{}
		for _, route := range snapshot.Routes {
			if targetMatches(route.Target, target) {
				ids = append(ids, route.ID)
			}
		}
		if actual := hashIDs(ids); actual != precondition.RouteIDsHash {
			return model.ExposureSnapshot{}, model.NewError(model.ErrUnsafe, "tailscale", "exposure changed since preflight", true, "changed", "Refresh, review the new route set, and retry.")
		}
	}
	if precondition.AllRoutesHash != "" && RoutesHash(snapshot.Routes) != precondition.AllRoutesHash {
		return model.ExposureSnapshot{}, model.NewError(model.ErrUnsafe, "tailscale", "exposure configuration changed since preflight", true, "changed", "Refresh, review all routes, and retry.")
	}
	return snapshot, nil
}

func targetMatches(a, b model.Target) bool {
	a, b = a.Normalized(), b.Normalized()
	if a.Protocol != b.Protocol || a.Port != b.Port {
		return false
	}
	if a.Address == b.Address {
		return true
	}
	if model.ScopeForAddress(a.Address) == model.ScopeLoopback && model.ScopeForAddress(b.Address) == model.ScopeLoopback {
		return true
	}
	return (model.ScopeForAddress(a.Address) == model.ScopeLoopback && (b.Address == "0.0.0.0" || b.Address == "::")) ||
		((a.Address == "0.0.0.0" || a.Address == "::") && (b.Address == "0.0.0.0" || b.Address == "::"))
}

func RoutesHash(routes []model.ExposureRoute) string {
	identities := make([]string, 0, len(routes))
	for _, route := range routes {
		identities = append(identities, IdentityOf(route).CanonicalKey())
	}
	return hashIDs(identities)
}

func hashIDs(ids []string) string {
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(h[:])
}

func (a *Adapter) help(ctx context.Context, command string) (string, error) {
	result, err := a.run(ctx, command, "--help")
	if err != nil {
		return "", a.commandError(command+" --help", result, err)
	}
	if result.Truncated {
		return "", model.NewError(model.ErrUnknown, "tailscale", command+" help output was truncated", true, "partial", "Retry before changing exposure capabilities.")
	}
	// Some Tailscale CLI versions print normal help to stderr even with a zero
	// exit status. Capability parsing therefore considers both streams, while
	// still applying the bounded capture and conservative token checks above.
	return result.Stdout + "\n" + result.Stderr, nil
}

func (a *Adapter) List(ctx context.Context) (model.ExposureSnapshot, error) {
	now := a.now()
	snapshot := model.ExposureSnapshot{At: now, Source: "tailscale", Authoritative: false, Routes: []model.ExposureRoute{}}
	serve, serveErr := a.listMode(ctx, model.ExposureServe, now)
	funnel, funnelErr := a.listMode(ctx, model.ExposureFunnel, now)
	if serveErr != nil || funnelErr != nil {
		if serveErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, serveErr.Error())
		}
		if funnelErr != nil {
			snapshot.Warnings = append(snapshot.Warnings, funnelErr.Error())
		}
		providerErr := aggregateReadError(serveErr, funnelErr)
		snapshot.Error = ptr(providerErr.Safe())
		snapshot.Routes = append(snapshot.Routes, serve.Routes...)
		snapshot.Routes = append(snapshot.Routes, funnel.Routes...)
		return snapshot, providerErr
	}
	snapshot.Routes = append(snapshot.Routes, serve.Routes...)
	snapshot.Routes = append(snapshot.Routes, funnel.Routes...)
	snapshot.Routes = dedupRoutes(snapshot.Routes)
	snapshot.Authoritative = true
	return snapshot, nil
}

func aggregateReadError(first, second error) *model.AppError {
	chosen := first
	if chosen == nil {
		chosen = second
	}
	if chosen == nil {
		return model.NewError(model.ErrUnknown, "tailscale", "exposure state is incomplete", true, "unknown", "Retry before changing any exposure.")
	}
	app := model.AsAppError(chosen)
	code := app.Code
	for _, candidate := range []error{first, second} {
		if candidate == nil {
			continue
		}
		next := model.AsAppError(candidate)
		switch next.Code {
		case model.ErrPermission:
			code = model.ErrPermission
		case model.ErrTimeout, model.ErrCancelled:
			if code != model.ErrPermission {
				code = next.Code
			}
		case model.ErrDependency:
			if code != model.ErrPermission && code != model.ErrTimeout && code != model.ErrCancelled {
				code = model.ErrDependency
			}
		case model.ErrUnknown:
			if code == model.ErrOperation {
				code = model.ErrUnknown
			}
		}
	}
	return model.WrapError(code, "tailscale", "exposure state is incomplete", true, "unknown", "Retry before changing any exposure.", chosen)
}

func (a *Adapter) listMode(ctx context.Context, mode model.ExposureMode, now time.Time) (model.ExposureSnapshot, error) {
	result, err := a.run(ctx, string(mode), "status", "--json")
	if err != nil {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, a.commandError(string(mode)+" status", result, err)
	}
	if result.Truncated {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status output was truncated", true, "partial", "Retry before changing exposure.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	routes, err := parseStatus(mode, []byte(result.Stdout), now)
	if err != nil {
		return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode)}, err
	}
	return model.ExposureSnapshot{At: now, Source: "tailscale " + string(mode), Authoritative: true, Routes: routes}, nil
}

func (a *Adapter) Set(ctx context.Context, change ExposureChange) (model.OperationReceipt, error) {
	started := a.now()
	receipt := model.OperationReceipt{ID: model.StableID("set", change.Target.Key(), string(change.Mode), started.UTC().Format(time.RFC3339Nano)), StartedAt: started}
	if err := change.Target.Validate(); err != nil {
		appErr := model.WrapError(model.ErrInvalidInput, "tailscale", err.Error(), false, "invalid", "Select a valid TCP target and retry.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Mode != model.ExposureServe && change.Mode != model.ExposureFunnel {
		appErr := model.NewError(model.ErrInvalidInput, "tailscale", "set requires Serve or Funnel mode", false, "invalid", "Use disable to remove exposure.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Preconditions.RouteIDsHash == "" || change.Preconditions.AllRoutesHash == "" {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "exposure set requires route and global preflight fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	caps, capErr := a.Capabilities(ctx)
	if capErr != nil {
		receipt.Error = ptr(model.AsAppError(capErr).Safe())
		receipt.FinishedAt = a.now()
		return receipt, capErr
	}
	if (change.Mode == model.ExposureServe && (!caps.Serve || !caps.ExactServe)) || (change.Mode == model.ExposureFunnel && (!caps.Funnel || !caps.ExactFunnel)) {
		appErr := model.NewError(model.ErrUnsupported, "tailscale", string(change.Mode)+" does not have a verified exact-route capability", false, "read_only", "Run `tailge doctor --tailscale --probe ...`; tailge will not use a broad reset.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Service != "" && !caps.Service {
		appErr := model.NewError(model.ErrUnsupported, "tailscale", "service-scoped routes are not supported by the installed CLI", false, "read_only", "Use a Tailscale version exposing the --service selector.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if err := validateHandlerSelection(change.Service, change.Path, change.Backend); err != nil {
		appErr := model.WrapError(model.ErrUnsafe, "tailscale", err.Error(), false, "unsafe", "Refresh the route and retry; tailge will not guess provider configuration.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// Local discovery is TCP-only, so never silently configure an HTTP reverse
	// proxy for a database, SSH server, or other raw TCP service. Funnel uses
	// its documented public TCP port; the backend port remains change.Target.Port.
	transport := "tcp"
	listenPort := change.Target.Port
	legacyFunnel := change.Mode == model.ExposureFunnel && caps.FunnelLegacy
	if change.Mode == model.ExposureFunnel && !legacyFunnel {
		listenPort = 10000
	}
	if change.ProviderKey != "" {
		if legacyFunnel {
			appErr := model.NewError(model.ErrUnsupported, "tailscale", "legacy Funnel routes cannot be restored with an exact listener selector", false, "read_only", "Leave the existing route unchanged or use a Tailscale version with exact listener flags.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		selector, parseErr := ParseListenerSelector(change.ProviderKey, change.Mode)
		if parseErr != nil {
			appErr := model.WrapError(model.ErrUnsafe, "tailscale", "exact restore selector is invalid", false, "unsafe", "Refresh the route; tailge will not guess a rollback selector.", parseErr)
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		transport, listenPort = selector.Transport, selector.Port
	}
	if !legacyFunnel && ((change.Mode == model.ExposureServe && transport == "tcp" && !caps.ServeTCP) || (change.Mode == model.ExposureFunnel && transport == "tcp" && !caps.FunnelTCP) || (change.Mode == model.ExposureServe && transport == "https" && !caps.ServeHTTPS) || (change.Mode == model.ExposureFunnel && transport == "https" && !caps.FunnelHTTPS)) {
		appErr := model.NewError(model.ErrUnsupported, "tailscale", string(change.Mode)+" has no deterministic listener-port syntax", false, "read_only", "Use a Tailscale version exposing --https or --tcp listener flags.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	snapshot, err := a.checkPreconditionSnapshot(ctx, change.Target, change.Preconditions)
	if err != nil {
		receipt.Error = ptr(model.AsAppError(err).Safe())
		receipt.FinishedAt = a.now()
		return receipt, err
	}
	expectedKey := string(change.Mode) + ":" + transport + "=" + strconv.Itoa(listenPort)
	if legacyFunnel {
		expectedKey = ""
	}
	if snapshot.Authoritative {
		for _, route := range snapshot.Routes {
			if expectedKey != "" && sameProviderEndpoint(route.ProviderKey, expectedKey) && !targetMatches(route.Target, change.Target) {
				appErr := model.NewError(model.ErrUnsafe, "tailscale", "the requested provider endpoint is already owned by another target", false, "external", "Review the existing exact route and remove or replace it explicitly.")
				receipt.Error = ptr(appErr.Safe())
				receipt.FinishedAt = a.now()
				return receipt, appErr
			}
			if targetMatches(route.Target, change.Target) && (route.Mode != change.Mode || route.ProviderKey != expectedKey || route.Service != change.Service || route.Path != change.Path) {
				appErr := model.NewError(model.ErrUnsafe, "tailscale", "an existing route for this target must be removed exactly before setting a different route", false, "changed", "Use the controller's exact replacement flow; tailge will not stack or overwrite routes.")
				receipt.Error = ptr(appErr.Safe())
				receipt.FinishedAt = a.now()
				return receipt, appErr
			}
		}
	}
	targetArg := change.Backend
	if targetArg == "" {
		targetArg = targetArgumentForTransport(change.Target, transport)
	}
	args := []string{"--bg", "--yes"}
	if change.Service != "" {
		args = append(args, "--service="+change.Service)
	}
	if change.Path != "" {
		args = append(args, "--set-path="+change.Path)
	}
	args = append(args, "--"+transport+"="+strconv.Itoa(listenPort), targetArg)
	if legacyFunnel {
		args = []string{"--bg", "--yes", targetArg, "on"}
	}
	commandArgs := append([]string{string(change.Mode)}, args...)
	result, err := a.run(ctx, commandArgs...)
	receipt.Command, receipt.ExitCode, receipt.Stdout, receipt.Stderr = result.Command, result.ExitCode, redact(result.Stdout), redact(result.Stderr)
	receipt.FinishedAt = a.now()
	if err != nil {
		appErr := a.commandError(string(change.Mode), result, err)
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	if result.Truncated {
		appErr := model.NewError(model.ErrUnknown, "tailscale", "exposure command output was truncated", true, "unknown", "Refresh and verify the route before retrying.")
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	return receipt, nil
}

func (a *Adapter) Remove(ctx context.Context, selector RouteSelector, expectedHash string) (model.OperationReceipt, error) {
	started := a.now()
	receipt := model.OperationReceipt{ID: model.StableID("remove", selector.ID, expectedHash, started.UTC().Format(time.RFC3339Nano)), StartedAt: started}
	if selector.ID == "" {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "cannot remove an exposure without an exact route identity", false, "unsafe", "Refresh and select one uniquely identified route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if expectedHash == "" || selector.AllRoutesHash == "" {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "route removal requires route and global preflight fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if selector.Target == nil {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "route removal requires the exact target for precondition validation", false, "unsafe", "Refresh and select the complete route target before retrying.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// IDs generated from unaddressable status paths are intentionally not
	// accepted. The caller must provide a deterministic provider listener selector.
	parts := strings.SplitN(selector.ID, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "provider does not expose an exact removal selector for this route", false, "unsafe", "Use the Tailscale client to review this route; tailge will not use a broad reset.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	mode, service := parts[0], parts[1]
	if selector.Mode != "" && selector.Mode != model.ExposureDisabled && mode != string(selector.Mode) {
		appErr := model.NewError(model.ErrUnsafe, "tailscale", "route selector mode does not match route identity", false, "changed", "Refresh the route and retry with its exact mode.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if _, parseErr := ParseListenerSelector(selector.ID, model.ExposureMode(mode)); parseErr != nil {
		appErr := model.WrapError(model.ErrUnsafe, "tailscale", "provider route identity is not an exact listener selector", false, "unsafe", "Refresh the route; tailge will not use a broad reset.", parseErr)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if mode == string(model.ExposureFunnel) && (selector.Service != "" || selector.Path != "") {
		appErr := model.NewError(model.ErrUnsupported, "tailscale", "Funnel route identity contains unsupported service or path scope", false, "read_only", "Review the Funnel route manually; tailge will not guess its selector.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	var args []string
	switch mode {
	case string(model.ExposureServe):
		caps, capErr := a.Capabilities(ctx)
		if capErr != nil {
			receipt.Error = ptr(model.AsAppError(capErr).Safe())
			receipt.FinishedAt = a.now()
			return receipt, capErr
		}
		if !caps.ExactServe || (selector.Service != "" && !caps.Service) {
			appErr := model.NewError(model.ErrUnsupported, "tailscale", "Serve exact removal is not available through the installed CLI", false, "read_only", "Tailge will not use serve reset; use a version with exact route removal and service selectors.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		args = []string{"serve"}
		if selector.Service != "" {
			args = append(args, "--service="+selector.Service)
		}
		if selector.Path != "" {
			args = append(args, "--set-path="+selector.Path)
		}
		args = append(args, "--bg", "--"+service, "off")
	case string(model.ExposureFunnel):
		caps, capErr := a.Capabilities(ctx)
		if capErr != nil {
			receipt.Error = ptr(model.AsAppError(capErr).Safe())
			receipt.FinishedAt = a.now()
			return receipt, capErr
		}
		if !caps.ExactFunnel {
			appErr := model.NewError(model.ErrUnsupported, "tailscale", "Funnel exact removal is not available through the installed CLI", false, "read_only", "Tailge will not use funnel reset; use a version with exact listener removal.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		if caps.FunnelLegacy {
			service = strings.TrimPrefix(service, "tcp=")
			args = []string{"funnel", service, "off"}
		} else if strings.Contains(service, "=") {
			args = []string{"funnel", "--" + service, "off"}
		} else {
			appErr := model.NewError(model.ErrUnsafe, "tailscale", "Funnel route is missing its exact listener flag", false, "unsafe", "Refresh the route; tailge will not use funnel reset.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
	default:
		appErr := model.NewError(model.ErrUnsupported, "tailscale", "unknown exposure route mode", false, "unsupported", "Refresh and select a supported route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// Capability inspection can take time and may itself read provider state.
	// Revalidate after it so the exact selector/fingerprint is the last check
	// before the mutating command is sent.
	if err := validateHandlerSelection(selector.Service, selector.Path, selector.Backend); err != nil {
		appErr := model.WrapError(model.ErrUnsafe, "tailscale", err.Error(), false, "unsafe", "Refresh the route and retry; tailge will not guess provider configuration.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if err := a.checkRemovalPrecondition(ctx, *selector.Target, selector, expectedHash); err != nil {
		receipt.Error = ptr(model.AsAppError(err).Safe())
		receipt.FinishedAt = a.now()
		return receipt, err
	}
	result, err := a.run(ctx, args...)
	receipt.Command, receipt.ExitCode, receipt.Stdout, receipt.Stderr = result.Command, result.ExitCode, redact(result.Stdout), redact(result.Stderr)
	receipt.FinishedAt = a.now()
	if err != nil {
		appErr := a.commandError(mode+" remove", result, err)
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	if result.Truncated {
		appErr := model.NewError(model.ErrUnknown, "tailscale", "exposure removal output was truncated", true, "unknown", "Refresh and verify the route before retrying.")
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	return receipt, nil
}

// ReadinessOptions carries optional persisted Serve compatibility evidence.
// Funnel readiness is capability-driven when exact listener flags are exposed;
// each Funnel mutation still performs bounded read-after-write verification.
type ReadinessOptions struct {
	ServeProbeVersion  string
	FunnelProbeVersion string
}

func (a *Adapter) Readiness(ctx context.Context, options ReadinessOptions) (model.Readiness, error) {
	now := a.now()
	readiness := model.Readiness{At: now, Status: model.ReadinessUnknown, Checks: []model.ReadinessCheck{}, Modes: []model.ModeReadiness{}}
	if a.binaryPath() == "" {
		check := model.ReadinessCheck{Name: "binary", Status: model.ReadinessNotReady, Message: "tailscale executable was not found", Remediation: "Install Tailscale and retry.", CheckedAt: now}
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = model.ReadinessNotReady
		return readiness, model.NewError(model.ErrDependency, "tailscale", check.Message, true, "unavailable", check.Remediation)
	}
	readiness.Binary = a.binaryPath()
	version, versionErr := a.Version(ctx)
	if versionErr != nil {
		check := checkFromError("version", versionErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, versionErr
	}
	readiness.Version = version
	status, statusErr := a.Status(ctx)
	if statusErr != nil {
		check := checkFromError("status", statusErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, statusErr
	}
	backendState := sanitizeText(status.BackendState)
	daemonCheck := statusValue(backendState == "Running", "Tailscale control service is running", "Tailscale backend is "+backendState, now)
	identityCheck := statusValue(status.HaveNodeKey && (status.Self.HostName != "" || status.Self.DNSName != ""), "node identity is available", "node identity is missing", now)
	connectedCheck := statusValue(status.BackendState == "Running" && hasValidNodeAddress(status) && status.Self.Online, "node is connected", "node is not currently connected", now)
	readiness.Daemon, readiness.Identity, readiness.Connected = daemonCheck.Status, identityCheck.Status, connectedCheck.Status
	readiness.Checks = append(readiness.Checks, daemonCheck, identityCheck, connectedCheck)
	caps, capErr := a.Capabilities(ctx)
	if capErr != nil {
		check := checkFromError("capabilities", capErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, capErr
	}
	baseStatus := baseReadiness(readiness)
	for _, mode := range []model.ExposureMode{model.ExposureServe, model.ExposureFunnel} {
		modeReady := model.ModeReadiness{Mode: mode, Status: model.ReadinessUnknown, Checks: []model.ReadinessCheck{}, Remote: "not checked"}
		supported, exact, probeVersion := false, false, ""
		if mode == model.ExposureServe {
			supported, exact, probeVersion = caps.Serve, caps.ExactServe, options.ServeProbeVersion
		} else {
			supported, exact, probeVersion = caps.Funnel, caps.ExactFunnel, options.FunnelProbeVersion
		}
		if baseStatus != model.ReadinessReady {
			modeReady.Status = baseStatus
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: "node readiness", Status: baseStatus, Message: "Tailscale node is not ready for exposure changes", Remediation: "Fix the daemon, identity, and connection checks, then retry.", CheckedAt: now})
		} else if !supported {
			modeReady.Status = model.ReadinessNotReady
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " capability", Status: model.ReadinessNotReady, Message: string(mode) + " is not supported by the installed Tailscale CLI", Remediation: "Use a supported Tailscale version and policy.", CheckedAt: now})
		} else if !exact {
			modeReady.Status = model.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " exact route operations", Status: model.ReadinessReadOnly, Message: "exact route replacement/removal is not available", Remediation: "Tailge will not use a broad reset.", CheckedAt: now})
		} else if mode == model.ExposureFunnel {
			// Funnel uses the exact listener flags discovered from the installed
			// CLI. The mutation path performs its own bounded set/read-after-write
			// verification, so a manual disposable public probe is not required to
			// unlock the action. Public exposure still requires explicit confirmation.
			modeReady.Status = model.ReadinessReady
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: "funnel exact route capability", Status: model.ReadinessReady, Message: "exact Funnel listener operations are available; each action is verified after mutation", CheckedAt: now})
		} else if probeVersion == "" || probeVersion != version {
			modeReady.Status = model.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: model.ReadinessReadOnly, Message: "this adapter version has not passed an explicit compatibility probe", Remediation: "Run `tailge doctor --tailscale --probe ...` with a disposable listener.", CheckedAt: now})
		} else {
			modeReady.Status = model.ReadinessReady
			modeReady.Probe = true
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: model.ReadinessReady, Message: "set, verify, and cleanup evidence matches this adapter version", CheckedAt: now})
		}
		readiness.Modes = append(readiness.Modes, modeReady)
	}
	readiness.Status = aggregateReadiness(readiness.Modes, readiness.Checks)
	// Mode-specific readiness is returned in the report. Funnel can be ready
	// from exact CLI capabilities without a manual public probe; its public
	// confirmation and per-operation verification gates remain mandatory.
	return readiness, nil
}

func modeFailures(status model.ReadinessStatus, message, remediation string, now time.Time) []model.ModeReadiness {
	modes := make([]model.ModeReadiness, 0, 2)
	for _, mode := range []model.ExposureMode{model.ExposureServe, model.ExposureFunnel} {
		modes = append(modes, model.ModeReadiness{Mode: mode, Status: status, Remote: "not checked", Checks: []model.ReadinessCheck{{Name: "node readiness", Status: status, Message: message, Remediation: remediation, CheckedAt: now}}})
	}
	return modes
}

func baseReadiness(readiness model.Readiness) model.ReadinessStatus {
	statuses := []model.ReadinessStatus{readiness.Daemon, readiness.Identity, readiness.Connected}
	for _, status := range statuses {
		if status == model.ReadinessUnknown {
			return model.ReadinessUnknown
		}
	}
	for _, status := range statuses {
		if status != model.ReadinessReady {
			return model.ReadinessNotReady
		}
	}
	return model.ReadinessReady
}

func parseStatus(mode model.ExposureMode, data []byte, now time.Time) ([]model.ExposureRoute, error) {
	var value any
	if strings.TrimSpace(string(data)) == "" {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status output was empty", true, "unknown", "Retry the Tailscale status command.")
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", "cannot parse "+string(mode)+" status JSON", true, "unknown", "Upgrade or repair Tailscale, then retry.", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON has an unsupported root shape", true, "unknown", "Retry after checking the Tailscale version.")
	}
	if len(root) > 0 && !recognizedStatusShape(value) {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON has no recognized route fields", true, "unknown", "Retry after checking the Tailscale version.")
	}
	if err := validateStatusTargets(value, nil); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an invalid target", true, "unknown", "Retry after checking the Tailscale version.", err)
	}
	if err := validateCompleteHandlers(value, nil); err != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an unsupported or incomplete handler", true, "unknown", "Review the Tailscale configuration manually; tailge will not mutate incomplete state.", err)
	}
	permissions, permissionErr := funnelPermissions(value)
	if permissionErr != nil {
		return nil, model.WrapError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains an invalid AllowFunnel field", true, "unknown", "Retry after checking the Tailscale status output.", permissionErr)
	}
	var routes []model.ExposureRoute
	walkStatus(value, nil, "", "", mode, now, &routes)
	routes = dedupRoutes(routes)
	if len(root) > 0 && len(routes) == 0 && !onlyFunnelPermissionStatus(value) {
		return nil, model.NewError(model.ErrUnknown, "tailscale", string(mode)+" status JSON contains recognized fields but no complete routes", true, "unknown", "Retry after checking the Tailscale status output.")
	}
	routes = filterFunnelRoutes(routes, permissions)
	return routes, nil
}

const maxStatusDepth = 64

func validateCompleteHandlers(value any, path []string) error {
	if len(path) > maxStatusDepth {
		return fmt.Errorf("status JSON nesting exceeds %d levels", maxStatusDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(path, key)
			if strings.EqualFold(key, "handlers") {
				handlers, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: handlers field must be an object", sanitizeText(strings.Join(childPath, "/")))
				}
				for handlerPath, rawHandler := range handlers {
					handler, ok := rawHandler.(map[string]any)
					if !ok {
						return fmt.Errorf("%s/%s: handler must be an object", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
					complete := false
					targetFields := 0
					for handlerKey, rawTarget := range handler {
						if !isTargetField(handlerKey) {
							continue
						}
						if target, ok := rawTarget.(string); ok && target != "" {
							targetFields++
							complete = true
						}
					}
					if !complete {
						return fmt.Errorf("%s/%s: handler has no supported proxy target", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
					if targetFields > 1 {
						return fmt.Errorf("%s/%s: handler has multiple proxy targets", sanitizeText(strings.Join(childPath, "/")), sanitizeText(handlerPath))
					}
				}
			}
			if err := validateCompleteHandlers(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := validateCompleteHandlers(child, append(path, strconv.Itoa(index))); err != nil {
				return err
			}
		}
	}
	return nil
}

func funnelPermissions(value any) (map[string]bool, error) {
	permissions := map[string]bool{}
	var walk func(any, int) error
	walk = func(current any, depth int) error {
		if depth > maxStatusDepth {
			return nil
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if strings.EqualFold(key, "AllowFunnel") {
					if child == nil {
						continue
					}
					entries, ok := child.(map[string]any)
					if !ok {
						return fmt.Errorf("AllowFunnel must be an object")
					}
					for endpoint, raw := range entries {
						enabled, ok := raw.(bool)
						if !ok {
							return fmt.Errorf("AllowFunnel endpoint %q must be boolean", sanitizeText(endpoint))
						}
						if endpointPort(endpoint) == "" || portNumber(endpointPort(endpoint)) == 0 {
							return fmt.Errorf("AllowFunnel endpoint %q is not host:port", sanitizeText(endpoint))
						}
						permissions[endpoint] = enabled
					}
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value, 0); err != nil {
		return nil, err
	}
	return permissions, nil
}

func filterFunnelRoutes(routes []model.ExposureRoute, permissions map[string]bool) []model.ExposureRoute {
	if len(permissions) == 0 {
		if len(routes) == 0 {
			return routes
		}
		filtered := make([]model.ExposureRoute, 0, len(routes))
		for _, route := range routes {
			if route.Mode == model.ExposureServe {
				filtered = append(filtered, route)
			}
		}
		return filtered
	}
	filtered := make([]model.ExposureRoute, 0, len(routes))
	for _, route := range routes {
		funnel := funnelEnabledForRoute(route, permissions)
		if (route.Mode == model.ExposureFunnel && funnel) || (route.Mode == model.ExposureServe && !funnel) {
			filtered = append(filtered, route)
		}
	}
	return filtered
}

func funnelEnabledForRoute(route model.ExposureRoute, permissions map[string]bool) bool {
	ports := map[string]bool{strconv.Itoa(route.Target.Port): true}
	if _, selector, ok := strings.Cut(route.ProviderKey, ":"); ok {
		if _, providerPort, ok := strings.Cut(selector, "="); ok && providerPort != "" {
			ports[providerPort] = true
		}
	}
	endpoint := ""
	if route.URL != "" {
		if parsed, err := url.Parse(route.URL); err == nil && parsed.Host != "" {
			endpoint = normalizeEndpoint(parsed.Host)
		}
	}
	for rawEndpoint, enabled := range permissions {
		if !enabled {
			continue
		}
		if endpoint != "" {
			if endpoint == normalizeEndpoint(rawEndpoint) {
				return true
			}
			continue
		}
		if ports[endpointPort(rawEndpoint)] {
			return true
		}
	}
	return false
}

func normalizeEndpoint(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(value))
	}
	return strings.ToLower(net.JoinHostPort(strings.TrimSuffix(host, "."), port))
}

func endpointPort(value string) string {
	if _, port, err := net.SplitHostPort(value); err == nil {
		return port
	}
	if index := strings.LastIndexByte(value, ':'); index >= 0 && index+1 < len(value) {
		return value[index+1:]
	}
	return ""
}

func onlyFunnelPermissionStatus(value any) bool {
	root, ok := value.(map[string]any)
	if !ok || len(root) == 0 {
		return false
	}
	for key := range root {
		if !strings.EqualFold(key, "AllowFunnel") {
			return false
		}
	}
	return true
}

func recognizedStatusShape(value any) bool {
	recognized := false
	var walk func(any, int)
	walk = func(current any, depth int) {
		if recognized || depth > maxStatusDepth {
			return
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if lower == "web" || lower == "tcp" || lower == "https" || lower == "handlers" || lower == "proxy" || lower == "target" || lower == "backend" || lower == "handler" || lower == "tcpforward" || lower == "service" || lower == "servicename" || lower == "allowfunnel" || strings.HasPrefix(lower, "svc:") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") || publicPort(key) > 0 {
					recognized = true
					return
				}
				walk(child, depth+1)
			}
		case []any:
			for _, child := range typed {
				walk(child, depth+1)
			}
		}
	}
	walk(value, 0)
	return recognized
}

func validateStatusTargets(value any, path []string) error {
	if len(path) > maxStatusDepth {
		return fmt.Errorf("status JSON nesting exceeds %d levels", maxStatusDepth)
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(path, key)
			lowerKey := strings.ToLower(key)
			if lowerKey == "tcp" || lowerKey == "web" || lowerKey == "handlers" {
				container, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: %s field must be an object", sanitizeText(strings.Join(childPath, "/")), key)
				}
				if lowerKey == "tcp" {
					for port, route := range container {
						switch typedRoute := route.(type) {
						case string:
							if _, err := model.ParseTarget(typedRoute, "tcp"); err != nil {
								return fmt.Errorf("%s/%s: invalid TCP target: %w", sanitizeText(strings.Join(childPath, "/")), sanitizeText(port), err)
							}
						case map[string]any:
							// Tailscale may use an object containing transport metadata
							// (for example HTTPS: true) alongside target fields.
						default:
							return fmt.Errorf("%s/%s: TCP route must be an object or target string", sanitizeText(strings.Join(childPath, "/")), sanitizeText(port))
						}
					}
				}
				if lowerKey == "web" {
					for host, route := range container {
						if _, ok := route.(map[string]any); !ok {
							return fmt.Errorf("%s/%s: Web route must be an object", sanitizeText(strings.Join(childPath, "/")), sanitizeText(host))
						}
					}
				}
			}
			if lowerKey == "service" || lowerKey == "servicename" {
				if _, ok := child.(string); !ok {
					return fmt.Errorf("%s: service field must be a string", sanitizeText(strings.Join(childPath, "/")))
				}
			}
			if isTargetField(key) || strings.EqualFold(key, "tcpforward") {
				targetText, ok := child.(string)
				if !ok {
					return fmt.Errorf("%s: target field must be a string", sanitizeText(strings.Join(childPath, "/")))
				}
				if _, err := model.ParseTarget(targetText, "tcp"); err != nil {
					return fmt.Errorf("%s: %w", sanitizeText(strings.Join(childPath, "/")), err)
				}
			}
			if err := validateStatusTargets(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range typed {
			if err := validateStatusTargets(child, append(path, strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func walkStatus(value any, path []string, urlHint, serviceHint string, mode model.ExposureMode, now time.Time, routes *[]model.ExposureRoute) {
	if len(path) > maxStatusDepth {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		mapServiceHint := serviceHint
		for key, child := range typed {
			keys = append(keys, key)
			if strings.EqualFold(key, "Service") || strings.EqualFold(key, "ServiceName") {
				if mapService, ok := child.(string); ok {
					mapServiceHint = mapService
				}
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			newPath := append(append([]string{}, path...), key)
			nextURL, nextService := urlHint, mapServiceHint
			lowerKey := strings.ToLower(key)
			if strings.HasPrefix(lowerKey, "https://") || strings.HasPrefix(lowerKey, "http://") {
				nextURL = redact(key)
			}
			if !strings.Contains(key, "://") {
				if port := publicPort(key); port > 0 {
					nextURL = "https://" + key
				}
			}
			if strings.HasPrefix(strings.ToLower(key), "svc:") {
				nextService = key
			}
			if strings.EqualFold(key, "Service") || strings.EqualFold(key, "ServiceName") {
				if s, ok := child.(string); ok {
					nextService = s
				}
			}
			if isTargetField(key) {
				if targetString, ok := child.(string); ok {
					if target, err := model.ParseTarget(targetString, "tcp"); err == nil {
						transport := ""
						if containsPath(newPath, "web") {
							transport = "https"
						} else if containsPath(newPath, "tcp") {
							transport = "tcp"
						}
						providerKey := routeSelectorForPort(mode, transport, pathPort(newPath))
						// A service name is not itself a listener selector. Without a
						// public port from the status path, keep ProviderKey empty so
						// removal and rollback fail closed instead of guessing.
						pathValue := handlerPath(newPath)
						id := routeID(mode, target, nextURL, nextService, pathValue, targetString, strings.Join(newPath, "/"))
						*routes = append(*routes, model.ExposureRoute{ID: id, ProviderKey: providerKey, Service: nextService, Path: pathValue, Backend: targetString, Target: target, Mode: mode, URL: redact(nextURL), Ownership: model.OwnershipUnknown, State: model.ExposureActive, LastSeen: now, Source: "tailscale " + string(mode)})
					}
				}
			}
			if strings.EqualFold(key, "TCP") {
				walkTCP(child, newPath, nextURL, nextService, mode, now, routes)
				continue
			}
			walkStatus(child, newPath, nextURL, nextService, mode, now, routes)
		}
	case []any:
		for index, child := range typed {
			walkStatus(child, append(path, strconv.Itoa(index)), urlHint, serviceHint, mode, now, routes)
		}
	}
}

func walkTCP(value any, path []string, urlHint, serviceHint string, mode model.ExposureMode, now time.Time, routes *[]model.ExposureRoute) {
	objects, ok := value.(map[string]any)
	if !ok {
		return
	}
	for portText, child := range objects {
		var targetText, transport string
		switch v := child.(type) {
		case string:
			targetText, transport = v, "tcp"
		case map[string]any:
			for _, wantedKey := range []string{"TCPForward", "Proxy", "Target", "Backend", "Handler", "HTTP", "HTTPS"} {
				for actualKey, rawTarget := range v {
					if !strings.EqualFold(actualKey, wantedKey) {
						continue
					}
					if s, ok := rawTarget.(string); ok {
						targetText = s
						// The enclosing TCP container is authoritative for the
						// public transport. Do not infer HTTPS from a nested
						// target-field name such as Proxy.
						transport = "tcp"
					}
					break
				}
				if targetText != "" {
					break
				}
			}
		}
		if targetText == "" {
			continue
		}
		if target, err := model.ParseTarget(targetText, "tcp"); err == nil {
			if target.Port == 0 {
				if p, e := strconv.Atoi(portText); e == nil {
					target.Port = p
				}
			}
			providerKey := routeSelectorForPort(mode, transport, portNumber(portText))
			// The TCP map key is the exact public listener selector; do not
			// replace it with a service name when it is unavailable.
			id := routeID(mode, target, urlHint, serviceHint, "", targetText, strings.Join(path, "/")+"/"+portText)
			*routes = append(*routes, model.ExposureRoute{ID: id, ProviderKey: providerKey, Service: serviceHint, Backend: targetText, Target: target, Mode: mode, URL: redact(urlHint), Ownership: model.OwnershipUnknown, State: model.ExposureActive, LastSeen: now, Source: "tailscale " + string(mode)})
		}
	}
}

func publicPort(value string) int {
	colon := strings.LastIndexByte(value, ':')
	if colon < 1 || colon == len(value)-1 {
		return 0
	}
	return portNumber(value[colon+1:])
}

func containsPath(path []string, wanted string) bool {
	for _, part := range path {
		if strings.EqualFold(part, wanted) {
			return true
		}
	}
	return false
}

func pathPort(path []string) int {
	for i := len(path) - 1; i >= 0; i-- {
		if port := publicPort(path[i]); port > 0 {
			return port
		}
	}
	return 0
}

func (a *Adapter) run(ctx context.Context, args ...string) (runner.Result, error) {
	if a.Runner == nil {
		return runner.Result{}, model.NewError(model.ErrDependency, "tailscale", "no command runner is configured", true, "unavailable", "Retry the command.")
	}
	name := a.binaryPath()
	if name == "" {
		return runner.Result{ExitCode: -1}, &exec.Error{Name: "tailscale", Err: exec.ErrNotFound}
	}
	return a.Runner.Run(ctx, name, args...)
}

func (a *Adapter) commandError(command string, result runner.Result, cause error) *model.AppError {
	code := model.ErrOperation
	state := "failed"
	retry := true
	message := command + " failed"
	var existing *model.AppError
	if errors.As(cause, &existing) && existing != nil {
		code, state, retry = existing.Code, existing.State, existing.Retryable
	}
	if result.ExitCode == -1 && errors.Is(cause, exec.ErrNotFound) {
		code, state, message = model.ErrDependency, "unavailable", "tailscale executable was not found"
	}
	timedOut := errors.Is(cause, context.DeadlineExceeded)
	cancelled := errors.Is(cause, context.Canceled)
	if timedOut {
		code, state, message = model.ErrTimeout, "unknown", command+" timed out"
	} else if cancelled {
		code, state, message = model.ErrCancelled, "unknown", command+" was cancelled"
	} else if strings.Contains(strings.ToLower(result.Stderr), "permission denied") || strings.Contains(strings.ToLower(result.Stderr), "not permitted") {
		code, state, message = model.ErrPermission, "permission_denied", command+" was denied by Tailscale"
	}
	if result.Stderr != "" {
		message += ": " + redact(strings.TrimSpace(result.Stderr))
	}
	if result.Truncated && !timedOut && !cancelled {
		code, state, message = model.ErrUnknown, "partial", command+" output was truncated"
	}
	return model.WrapError(code, "tailscale", message, retry, state, "Review Tailscale status and retry; final exposure state must be verified.", cause)
}

var urlWithQuery = regexp.MustCompile(`(?i)https?://[^\s]+`)

func redact(value string) string {
	value = sanitizeText(value)
	value = urlWithQuery.ReplaceAllStringFunc(value, redactURLQuery)
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)((?:auth(?:orization)?|token|password|secret|cookie|credential|session(?:[_-]?id)?|jwt|api[_-]?key)(?:=|:|[ \t]+))(?:bearer[ \t]*)?[^\s&]+`),
		regexp.MustCompile(`(?i)(bearer[ \t]*)[^\s&]+`),
	}
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "$1[REDACTED]")
	}
	if len(value) > 4096 {
		value = value[:4096] + "…[truncated]"
	}
	return value
}

func sanitizeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}

func redactURLQuery(raw string) string {
	raw = redactURLUserInfo(raw)
	fragment := ""
	bodyEnd := len(raw)
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		bodyEnd = hash
		fragment = "#[REDACTED]"
	}
	body := raw[:bodyEnd]
	question := strings.IndexByte(body, '?')
	if question < 0 {
		return body + fragment
	}
	return body[:question+1] + redactQueryValues(body[question+1:]) + fragment
}

func redactURLUserInfo(raw string) string {
	scheme := strings.Index(strings.ToLower(raw), "://")
	if scheme < 0 {
		return raw
	}
	start := scheme + 3
	end := len(raw)
	for _, separator := range []byte{'/', '?', '#'} {
		if index := strings.IndexByte(raw[start:], separator); index >= 0 && start+index < end {
			end = start + index
		}
	}
	authority := raw[start:end]
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return raw
	}
	return raw[:start] + "[REDACTED]@" + raw[start+at+1:]
}

func redactQueryValues(query string) string {
	if query == "" {
		return query
	}
	var result strings.Builder
	result.Grow(len(query) + len("[REDACTED]"))
	start := 0
	for index := 0; index <= len(query); index++ {
		if index != len(query) && query[index] != '&' && query[index] != ';' {
			continue
		}
		part := query[start:index]
		if equal := strings.IndexByte(part, '='); equal >= 0 {
			result.WriteString(part[:equal])
			result.WriteString("=[REDACTED]")
		} else if part != "" {
			result.WriteString("[REDACTED]")
		}
		if index < len(query) {
			result.WriteByte(query[index])
		}
		start = index + 1
	}
	return result.String()
}

func checkFromError(name string, err error, now time.Time) model.ReadinessCheck {
	app := model.AsAppError(err)
	return model.ReadinessCheck{Name: name, Status: statusFromError(app), Message: app.Message, Remediation: app.Remediation, CheckedAt: now}
}

func statusFromError(err *model.AppError) model.ReadinessStatus {
	if err == nil {
		return model.ReadinessUnknown
	}
	switch err.Code {
	case model.ErrDependency, model.ErrPermission, model.ErrOperation:
		return model.ReadinessNotReady
	default:
		return model.ReadinessUnknown
	}
}

func statusValue(ok bool, success, failure string, now time.Time) model.ReadinessCheck {
	if ok {
		return model.ReadinessCheck{Name: "status", Status: model.ReadinessReady, Message: success, CheckedAt: now}
	}
	return model.ReadinessCheck{Name: "status", Status: model.ReadinessNotReady, Message: failure, Remediation: "Fix Tailscale setup and retry.", CheckedAt: now}
}

func aggregateReadiness(modes []model.ModeReadiness, checks []model.ReadinessCheck) model.ReadinessStatus {
	if len(modes) == 0 {
		return model.ReadinessUnknown
	}
	allReady := true
	anyNotReady := false
	anyUnknown := false
	for _, c := range checks {
		if c.Status == model.ReadinessNotReady {
			anyNotReady = true
		}
		if c.Status == model.ReadinessUnknown {
			anyUnknown = true
		}
	}
	for _, mode := range modes {
		if mode.Status != model.ReadinessReady {
			allReady = false
		}
		if mode.Status == model.ReadinessNotReady {
			anyNotReady = true
		}
		if mode.Status == model.ReadinessUnknown {
			anyUnknown = true
		}
	}
	if allReady && !anyNotReady && !anyUnknown {
		return model.ReadinessReady
	}
	if anyUnknown {
		return model.ReadinessUnknown
	}
	if anyNotReady {
		return model.ReadinessNotReady
	}
	return model.ReadinessReadOnly
}

func ptr[T any](v T) *T { return &v }
