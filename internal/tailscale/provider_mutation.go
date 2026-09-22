package tailscale

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

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
