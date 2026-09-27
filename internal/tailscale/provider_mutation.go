package tailscale

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/target"
)

type ExposurePrecondition struct {
	RouteIDsHash  string
	AllRoutesHash string
	RouteIDs      []string
}

type ExposureChange struct {
	Target        target.Target
	Mode          exposuredata.ExposureMode
	ProviderKey   string
	Service       string
	Path          string
	HTTPPath      bool
	HTTPSRoot     bool
	HTTPSPort     int
	Backend       string
	Preconditions ExposurePrecondition
}

type RouteSelector struct {
	ID            string
	Target        *target.Target
	Mode          exposuredata.ExposureMode
	Service       string
	Path          string
	Backend       string
	AllRoutesHash string
}

func (a *Adapter) checkRemovalPrecondition(ctx context.Context, target target.Target, selector RouteSelector, expectedHash string) error {
	snapshot, err := a.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return fault.NewError(fault.ErrUnknown, "tailscale", "current exposure state is not authoritative", true, "unknown", "Refresh before changing exposure.")
	}
	found := false
	for _, route := range snapshot.Routes {
		if targetMatches(route.Target, target) && route.ProviderKey == selector.ID && (selector.Mode == "" || selector.Mode == exposuredata.ExposureDisabled || route.Mode == selector.Mode) && route.Service == selector.Service && route.Path == selector.Path && route.Backend == selector.Backend {
			found = true
		}
	}
	if actual := RouteIDsHash(snapshot.Routes, target); actual != expectedHash {
		return fault.NewError(fault.ErrUnsafe, "tailscale", "exposure changed since preflight", true, "changed", "Refresh, review the new route set, and retry.")
	}
	if selector.AllRoutesHash != "" && RoutesHash(snapshot.Routes) != selector.AllRoutesHash {
		return fault.NewError(fault.ErrUnsafe, "tailscale", "exposure configuration changed since preflight", true, "changed", "Refresh, review all routes, and retry.")
	}
	if !found {
		return fault.NewError(fault.ErrUnsafe, "tailscale", "exact route selector is no longer present for the target", true, "changed", "Refresh and select the current route before retrying.")
	}
	return nil
}

func (a *Adapter) checkPreconditionSnapshot(ctx context.Context, target target.Target, precondition ExposurePrecondition) (exposuredata.ExposureSnapshot, error) {
	if precondition.RouteIDsHash == "" || precondition.AllRoutesHash == "" {
		return exposuredata.ExposureSnapshot{}, fault.NewError(fault.ErrUnsafe, "tailscale", "precondition requires route and global fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
	}
	snapshot, err := a.List(ctx)
	if err != nil {
		return exposuredata.ExposureSnapshot{}, err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return exposuredata.ExposureSnapshot{}, fault.NewError(fault.ErrUnknown, "tailscale", "current exposure state is not authoritative", true, "unknown", "Refresh before changing exposure.")
	}
	if precondition.RouteIDsHash != "" {
		if actual := RouteIDsHash(snapshot.Routes, target); actual != precondition.RouteIDsHash {
			return exposuredata.ExposureSnapshot{}, fault.NewError(fault.ErrUnsafe, "tailscale", "exposure changed since preflight", true, "changed", "Refresh, review the new route set, and retry.")
		}
	}
	if precondition.AllRoutesHash != "" && RoutesHash(snapshot.Routes) != precondition.AllRoutesHash {
		return exposuredata.ExposureSnapshot{}, fault.NewError(fault.ErrUnsafe, "tailscale", "exposure configuration changed since preflight", true, "changed", "Refresh, review all routes, and retry.")
	}
	return snapshot, nil
}

func (a *Adapter) Set(ctx context.Context, change ExposureChange) (exposuredata.OperationReceipt, error) {
	started := a.now()
	receipt := exposuredata.OperationReceipt{ID: target.StableID("set", change.Target.Key(), string(change.Mode), started.UTC().Format(time.RFC3339Nano)), StartedAt: started}
	if err := change.Target.Validate(); err != nil {
		appErr := fault.WrapError(fault.ErrInvalidInput, "tailscale", err.Error(), false, "invalid", "Select a valid TCP target and retry.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Mode != exposuredata.ExposureServe && change.Mode != exposuredata.ExposureFunnel {
		appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "set requires Serve or Funnel mode", false, "invalid", "Use disable to remove exposure.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Preconditions.RouteIDsHash == "" || change.Preconditions.AllRoutesHash == "" {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "exposure set requires route and global preflight fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	caps, capErr := a.Capabilities(ctx)
	if capErr != nil {
		receipt.Error = ptr(fault.AsAppError(capErr).Safe())
		receipt.FinishedAt = a.now()
		return receipt, capErr
	}
	if (change.Mode == exposuredata.ExposureServe && (!caps.Serve || !caps.ExactServe)) || (change.Mode == exposuredata.ExposureFunnel && (!caps.Funnel || !caps.ExactFunnel)) {
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", string(change.Mode)+" does not have a verified exact-route capability", false, "read_only", "Run `tailge doctor --tailscale --probe ...`; tailge will not use a broad reset.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if change.Service != "" && !caps.Service {
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "service-scoped routes are not supported by the installed CLI", false, "read_only", "Use a Tailscale version exposing the --service selector.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if err := validateHandlerSelection(change.Service, change.Path, change.Backend); err != nil {
		appErr := fault.WrapError(fault.ErrUnsafe, "tailscale", err.Error(), false, "unsafe", "Refresh the route and retry; tailge will not guess provider configuration.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// Local discovery is TCP-only, so only an explicit non-empty path selects
	// Tailscale's HTTPS reverse proxy. All legacy exposure mutations remain raw
	// TCP and retain their existing listener-port behavior.
	pathRoute := change.HTTPPath
	rootHTTPS := change.HTTPSRoot
	if pathRoute && rootHTTPS {
		appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "an HTTPS route cannot be both a named path and an explicit root handler", false, "invalid", "Choose one named path or an explicit HTTPS root route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if pathRoute {
		normalizedPath, pathErr := exposuredata.NormalizeHTTPPath(strings.TrimPrefix(change.Path, "/"))
		if pathErr != nil || normalizedPath != change.Path {
			appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "named HTTP path requires one canonical non-root service slug", false, "invalid", "Choose one lowercase path such as `/api`; the root endpoint has no implicit handler.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
	} else if rootHTTPS {
		if change.Mode != exposuredata.ExposureServe || change.Path != "/" || change.HTTPSPort < 1 || change.HTTPSPort > 65535 {
			appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "explicit HTTPS root route requires Serve mode, path `/`, and a valid HTTPS port", false, "invalid", "Use a private Serve HTTPS port and an explicit root selection.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
	} else if change.Path != "" {
		appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "a named path requires explicit HTTP path intent", false, "invalid", "Set the HTTP path mode explicitly; raw TCP routes never infer HTTP.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	transport := "tcp"
	listenPort := change.Target.Port
	legacyFunnel := change.Mode == exposuredata.ExposureFunnel && caps.FunnelLegacy
	if change.Mode == exposuredata.ExposureFunnel && !legacyFunnel {
		listenPort = 10000
	}
	if pathRoute || rootHTTPS {
		transport = "https"
		listenPort = 443
		if rootHTTPS {
			listenPort = change.HTTPSPort
		}
	}
	if change.ProviderKey != "" {
		if legacyFunnel {
			appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "legacy Funnel routes cannot be restored with an exact listener selector", false, "read_only", "Leave the existing route unchanged or use a Tailscale version with exact listener flags.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		selector, parseErr := ParseListenerSelector(change.ProviderKey, change.Mode)
		if parseErr != nil {
			appErr := fault.WrapError(fault.ErrUnsafe, "tailscale", "exact restore selector is invalid", false, "unsafe", "Refresh the route; tailge will not guess a rollback selector.", parseErr)
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		transport, listenPort = selector.Transport, selector.Port
	}
	if (pathRoute || rootHTTPS) && ((change.Mode == exposuredata.ExposureServe && !caps.ServePath) || (change.Mode == exposuredata.ExposureFunnel && !caps.FunnelPath)) {
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", string(change.Mode)+" exact HTTPS handler routes are not supported by the installed CLI", false, "read_only", "Use a Tailscale version exposing exact --set-path HTTPS handlers; existing routes were not changed.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if (pathRoute || rootHTTPS) && (transport != "https" || listenPort < 1 || listenPort > 65535 || (pathRoute && listenPort != 443) || (rootHTTPS && listenPort != change.HTTPSPort)) {
		appErr := fault.NewError(fault.ErrInvalidInput, "tailscale", "HTTP routes require a supported exact HTTPS listener port", false, "invalid", "Use port 443 for named paths or an explicit HTTPS port for a root route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if !legacyFunnel && ((change.Mode == exposuredata.ExposureServe && transport == "tcp" && !caps.ServeTCP) || (change.Mode == exposuredata.ExposureFunnel && transport == "tcp" && !caps.FunnelTCP) || (change.Mode == exposuredata.ExposureServe && transport == "https" && !caps.ServeHTTPS) || (change.Mode == exposuredata.ExposureFunnel && transport == "https" && !caps.FunnelHTTPS)) {
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", string(change.Mode)+" has no deterministic listener-port syntax", false, "read_only", "Use a Tailscale version exposing --https or --tcp listener flags.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	snapshot, err := a.checkPreconditionSnapshot(ctx, change.Target, change.Preconditions)
	if err != nil {
		receipt.Error = ptr(fault.AsAppError(err).Safe())
		receipt.FinishedAt = a.now()
		return receipt, err
	}
	expectedKey := string(change.Mode) + ":" + transport + "=" + strconv.Itoa(listenPort)
	if legacyFunnel {
		expectedKey = ""
	}
	if snapshot.Authoritative {
		for _, route := range snapshot.Routes {
			if pathRoute || rootHTTPS {
				if route.ProviderKey == "" {
					appErr := fault.NewError(fault.ErrUnknown, "tailscale", "an existing route has no exact endpoint selector", false, "read_only", "Review Tailscale Serve/Funnel status manually; Tailge will not mutate an unidentified shared endpoint.")
					receipt.Error = ptr(appErr.Safe())
					receipt.FinishedAt = a.now()
					return receipt, appErr
				}
				if _, selectorErr := ParseListenerSelector(route.ProviderKey, route.Mode); selectorErr != nil {
					appErr := fault.WrapError(fault.ErrUnknown, "tailscale", "an existing route has an invalid endpoint selector", false, "read_only", "Review Tailscale status manually; Tailge will not mutate an unidentified shared endpoint.", selectorErr)
					receipt.Error = ptr(appErr.Safe())
					receipt.FinishedAt = a.now()
					return receipt, appErr
				}
				if expectedKey != "" && sameProviderEndpoint(route.ProviderKey, expectedKey) {
					if route.Kind != exposuredata.RouteKindHTTPPath && route.Kind != exposuredata.RouteKindHTTPSRoot {
						appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "the HTTPS endpoint already has a non-HTTP handler", false, "conflict", "Remove or move the exact existing endpoint handler before adding an HTTP route.")
						receipt.Error = ptr(appErr.Safe())
						receipt.FinishedAt = a.now()
						return receipt, appErr
					}
					if (route.Kind == exposuredata.RouteKindHTTPPath && route.Path == "") || (route.Kind == exposuredata.RouteKindHTTPSRoot && route.Path != "/") {
						appErr := fault.NewError(fault.ErrUnknown, "tailscale", "an existing HTTPS handler has incomplete path identity", false, "read_only", "Refresh provider status; the handler will not be overwritten without an exact path.")
						receipt.Error = ptr(appErr.Safe())
						receipt.FinishedAt = a.now()
						return receipt, appErr
					}
					if route.Mode != change.Mode {
						appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "Serve and Funnel visibility cannot be mixed on one HTTPS endpoint", false, "scope_conflict", "All paths on this HTTPS hostname and port share one access scope; review the public/private consequence before changing it.")
						receipt.Error = ptr(appErr.Safe())
						receipt.FinishedAt = a.now()
						return receipt, appErr
					}
					if route.Path == change.Path {
						appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "the requested HTTPS path is already configured", false, "conflict", "Choose a different path or disable the exact existing handler first.")
						receipt.Error = ptr(appErr.Safe())
						receipt.FinishedAt = a.now()
						return receipt, appErr
					}
				}
				continue
			}
			if expectedKey != "" && sameProviderEndpoint(route.ProviderKey, expectedKey) && !targetMatches(route.Target, change.Target) {
				appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "the requested provider endpoint is already owned by another target", false, "external", "Review the existing exact route and remove or replace it explicitly.")
				receipt.Error = ptr(appErr.Safe())
				receipt.FinishedAt = a.now()
				return receipt, appErr
			}
			if targetMatches(route.Target, change.Target) && (route.Mode != change.Mode || route.ProviderKey != expectedKey || route.Service != change.Service || route.Path != change.Path) {
				appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "an existing route for this target must be removed exactly before setting a different route", false, "changed", "Use the controller's exact replacement flow; tailge will not stack or overwrite routes.")
				receipt.Error = ptr(appErr.Safe())
				receipt.FinishedAt = a.now()
				return receipt, appErr
			}
		}
	}
	targetArg := change.Backend
	if targetArg == "" {
		if pathRoute || rootHTTPS {
			targetArg = HTTPPathBackendArgument(change.Target)
		} else {
			targetArg = targetArgumentForTransport(change.Target, transport)
		}
	}
	args := []string{"--bg", "--yes"}
	if change.Service != "" {
		args = append(args, "--service="+change.Service)
	}
	if change.HTTPPath || change.HTTPSRoot {
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
		selected := change.Target.Normalized()
		if (pathRoute || rootHTTPS) &&
			(selected.Address == "::1" || selected.Address == "::") &&
			(change.Backend == "" || HTTPPathBackendMatches(change.Backend, HTTPPathBackendArgument(selected))) &&
			strings.Contains(strings.ToLower(result.Stderr), "unknown proxy destination") {
			appErr.Remediation = "Refresh provider status and inspect the exact HTTPS handler. If it exists, disable that exact path or root first; then retry with `--localhost-backend` (or Ctrl+B in the `p` dialog). Hostname resolution weakens the exact IPv6 address guarantee."
		}
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	if result.Truncated {
		appErr := fault.NewError(fault.ErrUnknown, "tailscale", "exposure command output was truncated", true, "unknown", "Refresh and verify the route before retrying.")
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	return receipt, nil
}

func (a *Adapter) Remove(ctx context.Context, selector RouteSelector, expectedHash string) (exposuredata.OperationReceipt, error) {
	started := a.now()
	receipt := exposuredata.OperationReceipt{ID: target.StableID("remove", selector.ID, expectedHash, started.UTC().Format(time.RFC3339Nano)), StartedAt: started}
	if selector.ID == "" {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "cannot remove an exposure without an exact route identity", false, "unsafe", "Refresh and select one uniquely identified route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if expectedHash == "" || selector.AllRoutesHash == "" {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "route removal requires route and global preflight fingerprints", false, "unsafe", "Refresh all exposure state and retry.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if selector.Target == nil {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "route removal requires the exact target for precondition validation", false, "unsafe", "Refresh and select the complete route target before retrying.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// IDs generated from unaddressable status paths are intentionally not
	// accepted. The caller must provide a deterministic provider listener selector.
	parts := strings.SplitN(selector.ID, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "provider does not expose an exact removal selector for this route", false, "unsafe", "Use the Tailscale client to review this route; tailge will not use a broad reset.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	mode, service := parts[0], parts[1]
	if selector.Mode != "" && selector.Mode != exposuredata.ExposureDisabled && mode != string(selector.Mode) {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "route selector mode does not match route identity", false, "changed", "Refresh the route and retry with its exact mode.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	parsedSelector, parseErr := ParseListenerSelector(selector.ID, exposuredata.ExposureMode(mode))
	if parseErr != nil {
		appErr := fault.WrapError(fault.ErrUnsafe, "tailscale", "provider route identity is not an exact listener selector", false, "unsafe", "Refresh the route; tailge will not use a broad reset.", parseErr)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if selector.Path != "" && parsedSelector.Transport != "https" {
		appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "named HTTP path identity requires an exact HTTPS listener selector", false, "unsafe", "Refresh provider status; Tailge will not combine a path handler with a raw-TCP selector.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if mode == string(exposuredata.ExposureFunnel) && selector.Service != "" {
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "Funnel route identity contains unsupported service scope", false, "read_only", "Review the Funnel route manually; tailge will not guess its selector.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	var args []string
	switch mode {
	case string(exposuredata.ExposureServe):
		caps, capErr := a.Capabilities(ctx)
		if capErr != nil {
			receipt.Error = ptr(fault.AsAppError(capErr).Safe())
			receipt.FinishedAt = a.now()
			return receipt, capErr
		}
		if !caps.ExactServe || (selector.Service != "" && !caps.Service) || (selector.Path != "" && !caps.ServePath) {
			appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "Serve exact removal is not available through the installed CLI", false, "read_only", "Tailge will not use serve reset; use a version with exact route removal and service selectors.")
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
	case string(exposuredata.ExposureFunnel):
		caps, capErr := a.Capabilities(ctx)
		if capErr != nil {
			receipt.Error = ptr(fault.AsAppError(capErr).Safe())
			receipt.FinishedAt = a.now()
			return receipt, capErr
		}
		if !caps.ExactFunnel || (selector.Path != "" && (!caps.FunnelHTTPS || !caps.FunnelPath || caps.FunnelLegacy)) {
			appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "Funnel exact removal is not available through the installed CLI", false, "read_only", "Tailge will not use funnel reset; use a version with exact HTTPS path removal.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
		if caps.FunnelLegacy {
			service = strings.TrimPrefix(service, "tcp=")
			args = []string{"funnel", service, "off"}
		} else if strings.Contains(service, "=") {
			args = []string{"funnel"}
			if selector.Path != "" {
				args = append(args, "--set-path="+selector.Path)
			}
			args = append(args, "--"+service, "off")
		} else {
			appErr := fault.NewError(fault.ErrUnsafe, "tailscale", "Funnel route is missing its exact listener flag", false, "unsafe", "Refresh the route; tailge will not use funnel reset.")
			receipt.Error = ptr(appErr.Safe())
			receipt.FinishedAt = a.now()
			return receipt, appErr
		}
	default:
		appErr := fault.NewError(fault.ErrUnsupported, "tailscale", "unknown exposure route mode", false, "unsupported", "Refresh and select a supported route.")
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	// Capability inspection can take time and may itself read provider state.
	// Revalidate after it so the exact selector/fingerprint is the last check
	// before the mutating command is sent.
	if err := validateHandlerSelection(selector.Service, selector.Path, selector.Backend); err != nil {
		appErr := fault.WrapError(fault.ErrUnsafe, "tailscale", err.Error(), false, "unsafe", "Refresh the route and retry; tailge will not guess provider configuration.", err)
		receipt.Error = ptr(appErr.Safe())
		receipt.FinishedAt = a.now()
		return receipt, appErr
	}
	if err := a.checkRemovalPrecondition(ctx, *selector.Target, selector, expectedHash); err != nil {
		receipt.Error = ptr(fault.AsAppError(err).Safe())
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
		appErr := fault.NewError(fault.ErrUnknown, "tailscale", "exposure removal output was truncated", true, "unknown", "Refresh and verify the route before retrying.")
		receipt.Error = ptr(appErr.Safe())
		return receipt, appErr
	}
	return receipt, nil
}
