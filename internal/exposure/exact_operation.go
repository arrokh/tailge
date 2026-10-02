package exposure

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

type exactOperationDependencies struct {
	discoverer interface {
		List(context.Context) (discovery.ListenerSnapshot, error)
	}
	provider           Provider
	mutationLockPath   string
	readinessOptions   tailscale.ReadinessOptions
	now                func() time.Time
	decorateRoutes     func(*exposuredata.ExposureSnapshot)
	markManaged        func(exposuredata.ExposureRoute)
	revokeManaged      func(exposuredata.ExposureRoute)
	recordReceipt      func(string, string, targetmodel.Target, exposuredata.ExposureMode, error)
	recordVerification func(string, targetmodel.Target, exposuredata.ExposureMode, error)
}

func (d exactOperationDependencies) currentTime() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d exactOperationDependencies) decorateExposure(snapshot *exposuredata.ExposureSnapshot) {
	if d.decorateRoutes != nil {
		d.decorateRoutes(snapshot)
	}
}

func (d exactOperationDependencies) markManagedRoute(route exposuredata.ExposureRoute) {
	if d.markManaged != nil {
		d.markManaged(route)
	}
}

func (d exactOperationDependencies) revokeManagedRoute(route exposuredata.ExposureRoute) {
	if d.revokeManaged != nil {
		d.revokeManaged(route)
	}
}

func (d exactOperationDependencies) recordReceiptEvent(operationID, phase string, target targetmodel.Target, mode exposuredata.ExposureMode, err error) {
	if d.recordReceipt != nil {
		d.recordReceipt(operationID, phase, target, mode, err)
	}
}

func (d exactOperationDependencies) recordVerificationEvent(operationID string, target targetmodel.Target, mode exposuredata.ExposureMode, err error) {
	if d.recordVerification != nil {
		d.recordVerification(operationID, target, mode, err)
	}
}

type exactExposureOperation struct {
	dependencies          exactOperationDependencies
	target                targetmodel.Target
	mode                  exposuredata.ExposureMode
	selectedProviderKey   string
	selectedRouteID       string
	selectedRouteMode     exposuredata.ExposureMode
	httpPath              string
	httpPathIntent        bool
	httpsPort             int
	httpsRootIntent       bool
	localhostBackendAlias bool
	replaceRawTCP         bool
	expectedRawTCPRouteID string
	confirmFunnel         bool
	confirmExternal       bool
	timeout               time.Duration
	approval              *MutationApproval
	operationID           string
	operationStarted      time.Time
}

func isHTTPHandlerRoute(route exposuredata.ExposureRoute) bool {
	if route.Kind == exposuredata.RouteKindHTTPPath ||
		route.Kind == exposuredata.RouteKindHTTPSRoot ||
		route.Path != "" ||
		strings.HasPrefix(strings.ToLower(route.URL), "https://") {
		return true
	}
	_, selector, ok := strings.Cut(route.ProviderKey, ":")
	return ok && strings.HasPrefix(selector, "https=")
}

func routeRemovalSelectorIdentity(route exposuredata.ExposureRoute) string {
	return strings.Join([]string{string(route.Mode), route.ProviderKey, route.Service, route.Path}, "\x00")
}

func (op *exactExposureOperation) validate() error {
	if err := op.target.Validate(); err != nil {
		return fault.WrapError(fault.ErrInvalidInput, "exposure", err.Error(), false, "invalid", "Select a valid target and retry.", err)
	}
	op.target = op.target.Normalized()
	if op.mode != exposuredata.ExposureServe && op.mode != exposuredata.ExposureFunnel && op.mode != exposuredata.ExposureDisabled {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "unsupported exposure mode", false, "invalid", "Choose disabled, serve, or funnel.")
	}
	if op.mode == exposuredata.ExposureFunnel && !op.confirmFunnel {
		return fault.NewError(fault.ErrUnsafe, "exposure", "Funnel requires explicit public-internet confirmation", false, "not_confirmed", "Confirm the exact target with `--confirm-public` or the TUI confirmation screen.")
	}
	if op.httpPathIntent && op.httpsRootIntent {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "an HTTPS route cannot be both a named path and a root handler", false, "invalid", "Choose a named service path or an explicit HTTPS root route.")
	}
	if op.httpPathIntent {
		if op.httpPath == "" {
			return fault.NewError(fault.ErrInvalidInput, "exposure", "named HTTP path requires a non-root service path", false, "invalid", "Choose one named path; the root endpoint has no implicit handler.")
		}
		if op.mode != exposuredata.ExposureServe && op.mode != exposuredata.ExposureFunnel {
			return fault.NewError(fault.ErrInvalidInput, "exposure", "named HTTP paths require Serve or Funnel mode", false, "invalid", "Choose private Serve or explicitly confirmed public Funnel.")
		}
		if op.httpsPort < 1 || op.httpsPort > 65535 {
			return fault.NewError(fault.ErrInvalidInput, "exposure", "HTTPS listener port is invalid", false, "invalid", "Use a valid HTTPS port from 1 through 65535.")
		}
		path, err := exposuredata.NormalizeHTTPPath(op.httpPath)
		if err != nil || path != op.httpPath {
			return fault.NewError(fault.ErrInvalidInput, "exposure", "named HTTP path is not a canonical service slug", false, "invalid", "Use one lowercase slug such as `api` or leave it blank to generate a stable name.")
		}
	}
	if op.replaceRawTCP && !op.httpsRootIntent {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "raw-TCP replacement requires explicit HTTPS-root intent", false, "invalid", "Select a private HTTPS root before replacing a raw-TCP route.")
	}
	if op.expectedRawTCPRouteID != "" && !op.replaceRawTCP {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "an expected raw-TCP route identity requires replacement intent", false, "invalid", "Select an exact HTTPS-root conversion before binding a route identity.")
	}
	if op.localhostBackendAlias && !op.httpPathIntent && !op.httpsRootIntent {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "localhost backend alias requires an explicit HTTPS handler", false, "invalid", "Use the exact listener backend or explicitly select a named HTTPS path or root route.")
	}
	if op.localhostBackendAlias && op.target.Address != "::1" && op.target.Address != "::" {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "localhost backend alias is only supported for IPv6 listeners", false, "invalid", "Omit `--localhost-backend` to preserve the selected address exactly.")
	}
	if op.httpsRootIntent && (op.mode != exposuredata.ExposureServe || op.httpPath != "/" || op.httpsPort < 1 || op.httpsPort > 65535) {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "explicit HTTPS root route requires private Serve mode, path `/`, and a valid HTTPS port", false, "invalid", "Use a valid HTTPS port and Serve; custom root routes are currently tailnet-private.")
	}
	if op.dependencies.provider == nil || op.dependencies.discoverer == nil {
		return fault.NewError(fault.ErrDependency, "exposure", "exposure providers are unavailable", true, "unavailable", "Run the readiness check and retry.")
	}
	return nil
}

func (op exactExposureOperation) run(ctx context.Context) (receipt exposuredata.OperationReceipt, applyErr error) {
	c := op.dependencies
	target := op.target
	mode := op.mode
	selectedProviderKey := op.selectedProviderKey
	selectedRouteMode := op.selectedRouteMode
	confirmExternal := op.confirmExternal
	timeout := op.timeout
	approval := op.approval
	operationID := op.operationID
	operationStarted := op.operationStarted
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	if timeout > maxOperationTimeout {
		timeout = maxOperationTimeout
	}
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	unlock, err := AcquireMutationLock(operationCtx, c.mutationLockPath)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	defer unlock()
	listeners, err := c.discoverer.List(operationCtx)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	exposures, err := c.provider.List(operationCtx)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	c.decorateExposure(&exposures)
	if !exposures.Authoritative || exposures.Error != nil {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "current exposure state is not authoritative", true, "unknown", "Refresh Tailscale state before changing exposure.")
	}
	if approval != nil {
		if approval.Target.Normalized().Key() != target.Key() {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "exposure target changed after confirmation", true, "changed", "Refresh and confirm the current route before retrying.")
		}
		if !approval.AllowOtherRouteChanges && (approval.AllRoutesHash == "" || tailscale.RoutesHash(exposures.Routes) != approval.AllRoutesHash) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "exposure state changed after confirmation", true, "changed", "Refresh and confirm the current route before retrying.")
		}
		if approval.AllowOtherRouteChanges && (approval.RouteIDsHash == "" || approval.TargetRoutesHash == "") {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "batch approval is missing an exact route fingerprint", true, "changed", "Refresh and confirm the current route before retrying.")
		}
		if approval.TargetRoutesHash != "" && RouteIdentityHash(exposures.Routes, target) != approval.TargetRoutesHash {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route identity changed after confirmation", true, "changed", "Refresh and confirm the current route before retrying.")
		}
		if approval.RouteIDsHash != "" && RouteIDsHash(exposures.Routes, target) != approval.RouteIDsHash {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route changed after confirmation", true, "changed", "Refresh and confirm the current route before retrying.")
		}
	}
	if mode != exposuredata.ExposureDisabled && (!listeners.Authoritative || listeners.Error != nil) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "current listener state is not authoritative", true, "unknown", "Refresh local listeners before enabling exposure.")
	}
	candidates := matchingListeners(listeners.Listeners, target)
	if approval != nil && approval.ListenerID != "" {
		if len(candidates) != 1 || !listenerMatchesApproval(candidates[0], *approval) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the confirmed listener identity changed", true, "changed", "Refresh and confirm the current listener before retrying.")
		}
	}
	if len(candidates) != 1 && mode != exposuredata.ExposureDisabled {
		if len(candidates) == 0 {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrInvalidInput, "exposure", "selected listener is no longer present", true, "changed", "Refresh and select a current listener.")
		}
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "target matches multiple listeners", false, "ambiguous", "Select an address-specific listener before applying exposure.")
	}
	if op.httpPathIntent || op.httpsRootIntent {
		path := op.httpPath
		if op.httpsRootIntent {
			path = "/"
		}
		return c.applyHTTPPathRoute(operationCtx, target, path, op.httpsPort, op.httpsRootIntent, op.localhostBackendAlias, op.replaceRawTCP, op.expectedRawTCPRouteID, confirmExternal, mode, candidates[0], exposures.Routes, operationID, operationStarted)
	}
	if mode != exposuredata.ExposureDisabled {
		for _, route := range exposures.Routes {
			if Matches(route.Target, target) && isHTTPHandlerRoute(route) {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "named HTTPS path routes require explicit HTTP-path intent", false, "conflict", "Use `exposure http` to manage this handler; raw-TCP Serve/Funnel actions do not replace HTTP paths.")
			}
		}
	}
	allRouteIDs := RouteIDs(exposures.Routes, target)
	routeIDs := allRouteIDs
	if mode == exposuredata.ExposureDisabled && (selectedProviderKey != "" || op.selectedRouteID != "" || selectedRouteMode != exposuredata.ExposureDisabled) {
		selected := make([]string, 0, 1)
		for _, route := range exposures.Routes {
			if !Matches(route.Target, target) {
				continue
			}
			if selectedProviderKey != "" && route.ProviderKey != selectedProviderKey {
				continue
			}
			if op.selectedRouteID != "" && route.ID != op.selectedRouteID {
				continue
			}
			if selectedRouteMode != exposuredata.ExposureDisabled && route.Mode != selectedRouteMode {
				continue
			}
			selected = append(selected, route.ID)
		}
		if len(selected) != 1 {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "the selected exact route is no longer unique", true, "changed", "Refresh and select one current route before disabling.")
		}
		routeIDs = selected
	}
	precondition := tailscale.ExposurePrecondition{RouteIDs: allRouteIDs, RouteIDsHash: RouteIDsHash(exposures.Routes, target), AllRoutesHash: tailscale.RoutesHash(exposures.Routes)}
	if mode == exposuredata.ExposureDisabled {
		if len(routeIDs) == 0 {
			return exposuredata.OperationReceipt{ID: targetmodel.StableID("noop-disable", target.Key()), StartedAt: c.currentTime(), FinishedAt: c.currentTime(), Verified: true}, nil
		}
		if len(routeIDs) != 1 {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "multiple routes match the target", false, "ambiguous", "Select one exact route before disabling.")
		}
		route := findRoute(exposures.Routes, routeIDs[0])
		if op.selectedRouteID != "" {
			for _, candidate := range exposures.Routes {
				if candidate.ID == route.ID {
					continue
				}
				if sameProviderHandlerSlot(route, candidate) {
					return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "ambiguous route identities share one exact removal selector", false, "ambiguous", "Refresh and resolve the duplicate provider route before removing it.")
				}
			}
		}
		if isHTTPHandlerRoute(route) && op.selectedRouteID == "" {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "HTTP handler removal requires explicit route identity", false, "intent_required", "Use `exposure http disable` with the exact path or root selection; a raw exposure disable does not imply HTTP intent.")
		}
		if isHTTPHandlerRoute(route) && route.Path == "" {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the observed HTTPS handler has no exact named path identity", false, "unsafe", "Tailge will not remove a potentially shared root handler without an exact path selector.")
		}
		if route.Ownership != exposuredata.OwnershipManaged && !confirmExternal {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route has unknown/external ownership", false, "external", "Review the route and pass `--confirm-external` to remove it.")
		}
		if route.ProviderKey == "" {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the provider did not expose an exact removal selector", false, "unsafe", "Review this route with Tailscale; tailge will not use a broad reset.")
		}
		if err := c.requireModeReady(operationCtx, route.Mode); err != nil {
			return exposuredata.OperationReceipt{}, err
		}
		receipt, removeErr := c.provider.Remove(operationCtx, tailscale.RouteSelector{ID: route.ProviderKey, Target: &target, Mode: route.Mode, Service: route.Service, Path: route.Path, Backend: route.Backend, AllRoutesHash: precondition.AllRoutesHash}, precondition.RouteIDsHash)
		c.recordReceiptEvent(operationID, "remove", target, route.Mode, removeErr)
		if removeErr != nil {
			return receipt, removeErr
		}
		verified, _, verifyErr := c.verifyAbsent(operationCtx, route)
		c.recordVerificationEvent(operationID, target, route.Mode, verifyErr)
		receipt.Verified = verifyErr == nil && verified
		if verifyErr != nil {
			receipt.Error = ptr(fault.AsAppError(verifyErr).Safe())
			return receipt, verifyErr
		}
		if !receipt.Verified {
			e := fault.NewError(fault.ErrVerification, "exposure", "route removal could not be verified", true, "unknown", "Refresh and inspect the route before retrying.")
			receipt.Error = ptr(e.Safe())
			return receipt, e
		}
		c.revokeManagedRoute(route)
		return receipt, nil
	}
	if err := c.requireModeReady(operationCtx, mode); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	caps, capErr := c.provider.Capabilities(operationCtx)
	if capErr != nil {
		return exposuredata.OperationReceipt{}, capErr
	}
	if (mode == exposuredata.ExposureServe && (!caps.Serve || !caps.ExactServe)) || (mode == exposuredata.ExposureFunnel && (!caps.Funnel || !caps.ExactFunnel)) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", string(mode)+" does not have a verified exact-route capability", false, "read_only", "Run `tailge doctor --tailscale --probe ...`; tailge will not use a broad reset.")
	}
	// Capability inspection and the initial provider read can take long enough
	// for the selected application listener to disappear. Revalidate it before
	// any replacement removal, then again immediately before Set below.
	if err := c.requireCurrentListener(operationCtx, target); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	var previous *exposuredata.ExposureRoute
	replaceExisting := false
	replacementService, replacementPath, replacementBackend := "", "", ""
	desiredProviderKey := tailscale.ProviderKeyForTargetWithCapabilities(mode, target, caps)
	legacyFunnel := mode == exposuredata.ExposureFunnel && caps.FunnelLegacy
	if legacyFunnel {
		// Legacy Funnel syntax does not select a public listener port before
		// creation; verification must use the exact selector reported by status.
		desiredProviderKey = ""
	}
	if len(routeIDs) == 1 {
		route := findRoute(exposures.Routes, routeIDs[0])
		previous = &route
		if route.Ownership != exposuredata.OwnershipManaged && !confirmExternal {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route has unknown/external ownership", false, "external", "Review the route and pass `--confirm-external` to replace it.")
		}
		if route.ProviderKey == "" {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the provider did not expose an exact replacement selector", false, "unsafe", "Review this route with Tailscale; tailge will not overwrite it without exact identity.")
		}
		if route.Mode != mode && !exactModeSupported(caps, route.Mode) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", "the existing route cannot be safely rolled back with this provider", false, "read_only", "Leave the existing route unchanged and review the installed Tailscale capabilities.")
		}
		replaceExisting = route.Mode != mode || (desiredProviderKey != "" && route.ProviderKey != desiredProviderKey)
		if route.Service != "" && !caps.Service {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", "the existing route uses a service selector unsupported by this provider", false, "read_only", "Use a Tailscale version exposing service-scoped route selectors.")
		}
		if !replaceExisting {
			receipt := exposuredata.OperationReceipt{ID: operationID, StartedAt: operationStarted, FinishedAt: c.currentTime(), Verified: true}
			if route.Ownership == exposuredata.OwnershipManaged {
				c.markManagedRoute(route)
			}
			return receipt, nil
		}
		if !exactRollbackSelector(route, caps) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", "the existing route cannot be restored exactly if replacement fails", false, "read_only", "Leave the existing route unchanged or use a provider route with an exact listener selector.")
		}
		if replaceExisting {
			replacementService, replacementPath, replacementBackend = route.Service, route.Path, route.Backend
			removed, removeErr := c.provider.Remove(operationCtx, tailscale.RouteSelector{ID: route.ProviderKey, Target: &target, Mode: route.Mode, Service: route.Service, Path: route.Path, Backend: route.Backend, AllRoutesHash: precondition.AllRoutesHash}, precondition.RouteIDsHash)
			c.recordReceiptEvent(operationID, "remove-before-replace", target, route.Mode, removeErr)
			if removeErr != nil {
				return removed, removeErr
			}
			verified, afterRemove, verifyErr := c.verifyAbsent(operationCtx, route)
			c.recordVerificationEvent(operationID, target, route.Mode, verifyErr)
			if verifyErr != nil || !verified {
				if verifyErr == nil {
					verifyErr = fault.NewError(fault.ErrVerification, "exposure", "existing route removal could not be verified before replacement", true, "unknown", "Refresh and inspect Tailscale before retrying.")
				}
				return removed, verifyErr
			}
			c.revokeManagedRoute(route)
			precondition = tailscale.ExposurePrecondition{RouteIDs: []string{}, RouteIDsHash: RouteIDsHash(nil, target), AllRoutesHash: tailscale.RoutesHash(afterRemove.Routes)}
		}
	}
	if err := c.requireCurrentListener(operationCtx, target); err != nil {
		if replaceExisting {
			rollbackErr := c.rollbackReplacement(operationCtx, target, *previous)
			c.recordReceiptEvent(operationID, "rollback", previous.Target, previous.Mode, rollbackErr)
			if rollbackErr != nil {
				return receipt, fault.WrapError(fault.ErrOperation, "exposure", "listener disappeared and rollback failed", false, "partial_failure", "Inspect the current route state manually; the previous route could not be restored.", rollbackErr)
			}
		}
		return receipt, err
	}
	receipt, setErr := c.provider.Set(operationCtx, tailscale.ExposureChange{Target: target, Mode: mode, Service: replacementService, Path: replacementPath, Backend: replacementBackend, HTTPPath: previous != nil && previous.Kind == exposuredata.RouteKindHTTPPath, Preconditions: precondition})
	c.recordReceiptEvent(operationID, "set", target, mode, setErr)
	if setErr != nil {
		if replaceExisting {
			rollbackErr := c.rollbackReplacement(operationCtx, target, *previous)
			c.recordReceiptEvent(operationID, "rollback", previous.Target, previous.Mode, rollbackErr)
			if rollbackErr != nil {
				return receipt, fault.WrapError(fault.ErrOperation, "exposure", "replacement failed and rollback failed", false, "partial_failure", "Inspect the current route state manually; neither the requested nor previous mode is verified.", rollbackErr)
			}
		}
		return receipt, setErr
	}
	verifiedRoute, verifyErr := c.verifyMode(operationCtx, target, mode, desiredProviderKey, replacementService, replacementPath, replacementBackend)
	c.recordVerificationEvent(operationID, target, mode, verifyErr)
	if verifyErr != nil {
		if replaceExisting {
			rollbackErr := c.rollbackReplacement(operationCtx, target, *previous)
			c.recordReceiptEvent(operationID, "rollback", previous.Target, previous.Mode, rollbackErr)
			if rollbackErr != nil {
				return receipt, fault.WrapError(fault.ErrOperation, "exposure", "replacement verification failed and rollback failed", false, "partial_failure", "Inspect the current route state manually; the previous route could not be restored.", rollbackErr)
			}
		}
		receipt.Error = ptr(fault.AsAppError(verifyErr).Safe())
		return receipt, verifyErr
	}
	if verifiedRoute.ID == "" {
		e := fault.NewError(fault.ErrVerification, "exposure", "exposure command succeeded but route could not be verified", true, "unknown", "Refresh and inspect Tailscale before retrying.")
		receipt.Error = ptr(e.Safe())
		return receipt, e
	}
	receipt.Verified = true
	c.markManagedRoute(verifiedRoute)
	return receipt, nil
}

func (c exactOperationDependencies) rollbackReplacement(_ context.Context, requested targetmodel.Target, previous exposuredata.ExposureRoute) error {
	// Rollback is cleanup after a mutating command. It must not inherit an
	// expired or cancelled operation context and leave the old route absent.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := c.provider.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return fault.NewError(fault.ErrUnknown, "exposure", "rollback state is not authoritative", true, "unknown", "Inspect the provider manually; rollback was not attempted.")
	}
	matches := make([]exposuredata.ExposureRoute, 0, 2)
	for _, route := range snapshot.Routes {
		if Matches(route.Target, requested) {
			matches = append(matches, route)
		}
	}
	if len(matches) != 0 {
		if len(matches) == 1 && previous.ProviderKey != "" && routeFingerprint(matches[0]) == routeFingerprint(previous) {
			if previous.Ownership == exposuredata.OwnershipManaged {
				c.markManagedRoute(matches[0])
			}
			return nil
		}
		return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found an unexpected current route", false, "changed", "Inspect the current exact route; tailge will not overwrite it during rollback.")
	}
	precondition := tailscale.ExposurePrecondition{
		RouteIDsHash:  RouteIDsHash(nil, previous.Target),
		AllRoutesHash: tailscale.RoutesHash(snapshot.Routes),
	}
	caps, err := c.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	expectedProviderKey := previous.ProviderKey
	if expectedProviderKey == "" {
		expectedProviderKey = tailscale.ProviderKeyForTargetWithCapabilities(previous.Mode, previous.Target, caps)
		if previous.Mode == exposuredata.ExposureFunnel && caps.FunnelLegacy {
			expectedProviderKey = ""
		}
	}
	if _, err := c.provider.Set(ctx, tailscale.ExposureChange{Target: previous.Target, Mode: previous.Mode, ProviderKey: previous.ProviderKey, Service: previous.Service, Path: previous.Path, Backend: previous.Backend, HTTPPath: previous.Kind == exposuredata.RouteKindHTTPPath, Preconditions: precondition}); err != nil {
		return err
	}
	verified, err := c.verifyMode(ctx, previous.Target, previous.Mode, expectedProviderKey, previous.Service, previous.Path, previous.Backend)
	if err != nil {
		return err
	}
	if previous.Ownership == exposuredata.OwnershipManaged {
		c.markManagedRoute(verified)
	}
	return nil
}

func operationErrorCode(err error) fault.ErrorCode {
	if err == nil {
		return ""
	}
	return fault.AsAppError(err).Code
}

func (c exactOperationDependencies) requireCurrentListener(ctx context.Context, target targetmodel.Target) error {
	listeners, err := c.discoverer.List(ctx)
	if err != nil {
		return err
	}
	if !listeners.Authoritative || listeners.Error != nil {
		return fault.NewError(fault.ErrUnknown, "exposure", "current listener state is not authoritative", true, "unknown", "Refresh local listeners before changing exposure.")
	}
	matches := matchingListeners(listeners.Listeners, target)
	if len(matches) == 0 {
		return fault.NewError(fault.ErrInvalidInput, "exposure", "selected listener is no longer present", true, "changed", "Refresh and select a current listener.")
	}
	if len(matches) != 1 {
		return fault.NewError(fault.ErrAmbiguous, "exposure", "target matches multiple listeners", false, "ambiguous", "Select an address-specific listener before applying exposure.")
	}
	return nil
}

func (c exactOperationDependencies) requireModeReady(ctx context.Context, mode exposuredata.ExposureMode) error {
	readinessProvider, ok := c.provider.(ReadinessProvider)
	if !ok {
		return fault.NewError(fault.ErrUnknown, "exposure", string(mode)+" readiness was not reported", true, "unknown", "Run `tailge doctor --tailscale` before changing exposure.")
	}
	readiness, err := readinessProvider.Readiness(ctx, c.readinessOptions)
	if err != nil {
		return err
	}
	for _, modeReadiness := range readiness.Modes {
		if modeReadiness.Mode != mode {
			continue
		}
		if modeReadiness.Status == readinessmodel.ReadinessReady {
			return nil
		}
		return fault.NewError(fault.ErrDependency, "exposure", string(mode)+" is not ready", true, string(modeReadiness.Status), "Run `tailge doctor --tailscale` and complete the compatibility probe.")
	}
	return fault.NewError(fault.ErrUnknown, "exposure", string(mode)+" readiness was not reported", true, "unknown", "Run `tailge doctor --tailscale` before changing exposure.")
}

func (c exactOperationDependencies) verifyMode(ctx context.Context, target targetmodel.Target, mode exposuredata.ExposureMode, expectedProviderKey, expectedService, expectedPath, expectedBackend string) (exposuredata.ExposureRoute, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, err := c.provider.List(ctx)
		if err == nil && snapshot.Authoritative && snapshot.Error == nil {
			matches := make([]exposuredata.ExposureRoute, 0, 1)
			for _, route := range snapshot.Routes {
				if route.Mode == mode && Matches(route.Target, target) && route.ProviderKey != "" && (expectedProviderKey == "" || route.ProviderKey == expectedProviderKey) && route.Service == expectedService && route.Path == expectedPath && (expectedBackend == "" || route.Backend == expectedBackend) {
					matches = append(matches, route)
				}
			}
			if len(matches) == 1 {
				return matches[0], nil
			}
			if len(matches) > 1 {
				return exposuredata.ExposureRoute{}, fault.NewError(fault.ErrAmbiguous, "exposure", "multiple routes match the requested target after apply", false, "ambiguous", "Inspect Tailscale and choose one exact route.")
			}
		}
		select {
		case <-ctx.Done():
			return exposuredata.ExposureRoute{}, verificationContextError("route state could not be verified", ctx.Err())
		case <-deadline.C:
			return exposuredata.ExposureRoute{}, fault.NewError(fault.ErrVerification, "exposure", "route state could not be verified", true, "unknown", "Refresh and inspect Tailscale before retrying.")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (c exactOperationDependencies) verifyAbsent(ctx context.Context, selected exposuredata.ExposureRoute) (bool, exposuredata.ExposureSnapshot, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, err := c.provider.List(ctx)
		if err == nil && snapshot.Authoritative && snapshot.Error == nil {
			present := false
			for _, route := range snapshot.Routes {
				// A route ID includes observed URL and visibility mode, both of
				// which can change while the provider handler remains configured.
				// Match the exact provider selector scope across Serve/Funnel instead
				// of claiming removal from a changed route ID alone.
				if route.ID == selected.ID || sameProviderHandlerSlot(selected, route) {
					present = true
					break
				}
			}
			if !present {
				return true, snapshot, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, exposuredata.ExposureSnapshot{}, verificationContextError("route removal could not be verified", ctx.Err())
		case <-deadline.C:
			return false, exposuredata.ExposureSnapshot{}, fault.NewError(fault.ErrVerification, "exposure", "route removal could not be verified", true, "unknown", "Refresh and inspect Tailscale before retrying.")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func sameProviderHandlerSlot(left, right exposuredata.ExposureRoute) bool {
	leftSelector, leftErr := tailscale.ParseListenerSelector(left.ProviderKey, left.Mode)
	rightSelector, rightErr := tailscale.ParseListenerSelector(right.ProviderKey, right.Mode)
	return leftErr == nil && rightErr == nil && leftSelector.Transport == rightSelector.Transport && leftSelector.Port == rightSelector.Port &&
		left.Service == right.Service && left.Path == right.Path
}

func exactModeSupported(caps tailscale.Capabilities, mode exposuredata.ExposureMode) bool {
	switch mode {
	case exposuredata.ExposureServe:
		return caps.Serve && caps.ExactServe
	case exposuredata.ExposureFunnel:
		return caps.Funnel && caps.ExactFunnel
	default:
		return false
	}
}

func exactRollbackSelector(route exposuredata.ExposureRoute, caps tailscale.Capabilities) bool {
	if route.ProviderKey == "" || route.Backend == "" || (route.Mode == exposuredata.ExposureFunnel && caps.FunnelLegacy) || (route.Service != "" && !caps.Service) {
		return false
	}
	if route.Path != "" && !strings.HasPrefix(route.Path, "/") {
		return false
	}
	parts := strings.SplitN(route.ProviderKey, ":", 2)
	if len(parts) != 2 || parts[0] != string(route.Mode) {
		return false
	}
	service := parts[1]
	if !strings.HasPrefix(service, "https=") && !strings.HasPrefix(service, "tcp=") {
		return false
	}
	port, err := strconv.Atoi(strings.TrimPrefix(strings.TrimPrefix(service, "https="), "tcp="))
	return err == nil && port >= 1 && port <= 65535
}

func verificationContextError(message string, cause error) *fault.AppError {
	code, state := fault.ErrVerification, "unknown"
	if errors.Is(cause, context.DeadlineExceeded) {
		code, state = fault.ErrTimeout, "unknown"
	} else if errors.Is(cause, context.Canceled) {
		code, state = fault.ErrCancelled, "cancelled"
	}
	return fault.WrapError(code, "exposure", message, true, state, "Refresh before retrying; the final provider state is not assumed.", cause)
}

func listenerMatchesApproval(listener discovery.Listener, approval MutationApproval) bool {
	return listener.ID == approval.ListenerID && listener.PID == approval.ListenerPID && listener.Process == approval.ListenerProcess && listener.ProcessStart == approval.ListenerStart && listener.CommandLine == approval.ListenerCommandLine && listener.Target.Normalized().Key() == approval.ListenerTarget.Normalized().Key()
}

func matchingListeners(listeners []discovery.Listener, target targetmodel.Target) []discovery.Listener {
	result := []discovery.Listener{}
	for _, listener := range listeners {
		if listener.Target.Key() == target.Key() || Matches(target, listener.Target) {
			result = append(result, listener)
		}
	}
	return result
}

func (c exactOperationDependencies) applyHTTPPathRoute(ctx context.Context, target targetmodel.Target, path string, httpsPort int, root, localhostBackendAlias, replaceRawTCP bool, expectedRawTCPRouteID string, confirmExternal bool, mode exposuredata.ExposureMode, listener discovery.Listener, routes []exposuredata.ExposureRoute, operationID string, operationStarted time.Time) (exposuredata.OperationReceipt, error) {
	if listener.Target.Normalized().Key() != target.Normalized().Key() {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "HTTPS route requires the exact discovered listener address", false, "changed", "Refresh and select the exact local listener before configuring HTTPS.")
	}
	if httpsPort < 1 || httpsPort > 65535 || (root && mode != exposuredata.ExposureServe) || (!root && httpsPort != 443) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrInvalidInput, "exposure", "HTTPS route port or mode is invalid", false, "invalid", "Named paths use port 443; explicit root routes currently require private Serve and a valid port.")
	}
	providerKey := string(mode) + ":https=" + strconv.Itoa(httpsPort)
	backend := tailscale.HTTPPathBackendArgument(target)
	if localhostBackendAlias {
		backend = tailscale.HTTPSBackendArgument(target, true)
	}
	backendTarget, targetErr := targetmodel.ParseTarget(backend, "tcp")
	if targetErr != nil {
		return exposuredata.OperationReceipt{}, fault.WrapError(fault.ErrInvalidInput, "exposure", "HTTPS backend identity is invalid", false, "invalid", "Select a valid local HTTP listener address.", targetErr)
	}
	if err := c.requireModeReady(ctx, mode); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	caps, err := c.provider.Capabilities(ctx)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	pathKind := exposuredata.RouteKindHTTPPath
	if root {
		pathKind = exposuredata.RouteKindHTTPSRoot
	}
	if (mode == exposuredata.ExposureServe && !caps.ServePath) || (mode == exposuredata.ExposureFunnel && !caps.FunnelPath) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", string(mode)+" exact HTTPS handler operations are unsupported by the installed Tailscale client", false, "read_only", "Use a Tailscale version with exact HTTPS listener and --set-path operations; Tailge will not reset or replace shared handlers.")
	}
	if (mode == exposuredata.ExposureServe && (!caps.Serve || !caps.ExactServe || !caps.ServeHTTPS)) || (mode == exposuredata.ExposureFunnel && (!caps.Funnel || !caps.ExactFunnel || !caps.FunnelHTTPS || caps.FunnelLegacy)) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", string(mode)+" HTTPS routes are unsupported by the installed Tailscale client", false, "read_only", "Use a Tailscale version with exact HTTPS listener operations; existing handlers will not be reset.")
	}
	seenEndpointSelectors := make(map[string]struct{})
	var rawTCPReplacement *exposuredata.ExposureRoute
	endpointRouteCount := 0
	for _, route := range routes {
		selector, parseErr := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if route.ProviderKey == "" || parseErr != nil {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "an existing route has no exact provider endpoint identity", false, "read_only", "Review provider status manually; Tailge will not mutate an unidentified HTTPS endpoint.")
		}
		if selector.Port != httpsPort {
			continue
		}
		endpointRouteCount++
		selectorIdentity := routeRemovalSelectorIdentity(route)
		if _, duplicate := seenEndpointSelectors[selectorIdentity]; duplicate {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "ambiguous HTTPS handler identity: multiple provider routes share one exact selector", false, "ambiguous", "Refresh and resolve the duplicate route identity before adding another handler.")
		}
		seenEndpointSelectors[selectorIdentity] = struct{}{}
		if route.ID == "" || route.State != exposuredata.ExposureActive {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "an existing endpoint handler has incomplete route identity", false, "read_only", "Refresh provider status; Tailge will not mutate an endpoint with incomplete handler state.")
		}
		if route.Mode != mode {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "Serve and Funnel cannot be mixed on the shared HTTPS endpoint", false, "scope_conflict", "Every handler on this hostname and port shares one access scope.")
		}
		if selector.Transport == "tcp" || route.Kind == exposuredata.RouteKindRawTCP {
			if !root || mode != exposuredata.ExposureServe || !replaceRawTCP {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the HTTPS endpoint already has a non-HTTP handler (raw TCP); explicit private HTTPS-root conversion is required", false, "conflict", "Use an explicit private HTTPS-root conversion with `--replace-raw-tcp`; Tailge will not infer HTTP or replace raw TCP implicitly.")
			}
			if selector.Transport != "tcp" || route.Kind != exposuredata.RouteKindRawTCP || route.Service != "" || route.Path != "" {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "the conflicting TCP route has incomplete exact identity", false, "read_only", "Refresh provider status; Tailge will not replace a route it cannot restore exactly.")
			}
			if route.Target.Normalized().Key() != target.Normalized().Key() || !tailscale.RawTCPBackendMatchesTarget(target, route.Backend) {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the conflicting raw-TCP route does not match the exact selected listener (target or backend identity)", false, "conflict", "Select the exact listener served by the existing TCP route or choose another HTTPS port.")
			}
			if expectedRawTCPRouteID != "" && route.ID != expectedRawTCPRouteID {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected raw-TCP route changed after preview", true, "changed", "Refresh and review the exact route before confirming conversion.")
			}
			if rawTCPReplacement != nil {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "multiple raw-TCP routes occupy the requested HTTPS port", false, "ambiguous", "Resolve the exact route identity before converting this endpoint.")
			}
			if route.Ownership != exposuredata.OwnershipManaged && !confirmExternal {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the conflicting raw-TCP route has unknown/external ownership", false, "external", "Review the exact route and repeat with `--confirm-external` to replace it.")
			}
			if !caps.ServeTCP || !exactRollbackSelector(route, caps) {
				return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", "the conflicting raw-TCP route cannot be restored exactly with this provider", false, "read_only", "Leave it unchanged or use a Tailscale version with an exact Serve TCP selector.")
			}
			replacement := route
			rawTCPReplacement = &replacement
			continue
		}
		if selector.Transport != "https" || (route.Kind != exposuredata.RouteKindHTTPPath && route.Kind != exposuredata.RouteKindHTTPSRoot) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the HTTPS endpoint already has a non-HTTP handler", false, "conflict", "Remove or move the exact existing endpoint handler before adding an HTTP route.")
		}
		if (route.Kind == exposuredata.RouteKindHTTPPath && route.Path == "") || (route.Kind == exposuredata.RouteKindHTTPSRoot && route.Path != "/") {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "an existing HTTPS handler has incomplete path identity", false, "read_only", "Refresh provider status; Tailge will not mutate a handler whose exact path is unknown.")
		}
		if !httpBackendMatchesTarget(route) {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnknown, "exposure", "an existing HTTPS handler has incomplete backend identity", false, "read_only", "Refresh provider status; Tailge will not mutate an endpoint with an unidentified handler backend.")
		}
		if route.Path == path {
			if route.Kind == pathKind && route.Mode == mode && route.ID != "" && route.State == exposuredata.ExposureActive && route.Target.Normalized().Key() == backendTarget.Normalized().Key() && tailscale.HTTPPathBackendMatches(route.Backend, backend) {
				return exposuredata.OperationReceipt{ID: route.ID, StartedAt: operationStarted, FinishedAt: c.currentTime(), Verified: true}, nil
			}
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the requested HTTPS path is already configured for a different route identity", false, "conflict", "Choose another path or disable the exact existing handler first.")
		}
	}
	if expectedRawTCPRouteID != "" && rawTCPReplacement == nil {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected raw-TCP route is no longer present at this HTTPS port", true, "changed", "Refresh and review the exact route before confirming conversion.")
	}
	if rawTCPReplacement != nil {
		if endpointRouteCount != 1 {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "the raw-TCP handler shares its port with other observed endpoint routes", false, "ambiguous", "Resolve every handler on this exact port before converting it.")
		}
		return c.replaceRawTCPWithHTTPSRoot(ctx, target, *rawTCPReplacement, path, httpsPort, localhostBackendAlias, backend, operationID, operationStarted)
	}
	if err := c.requireCurrentListener(ctx, target); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	precondition := tailscale.ExposurePrecondition{RouteIDs: RouteIDs(routes, target), RouteIDsHash: RouteIDsHash(routes, target), AllRoutesHash: tailscale.RoutesHash(routes)}
	if precondition.AllRoutesHash == "" || precondition.RouteIDsHash == "" {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "HTTPS route preflight is incomplete", false, "unsafe", "Refresh all provider route state before adding an HTTPS handler.")
	}
	change := tailscale.ExposureChange{Target: target, Mode: mode, ProviderKey: providerKey, Path: path, HTTPSPort: httpsPort, HTTPPath: !root, HTTPSRoot: root, Backend: backend, Preconditions: precondition}
	receipt, setErr := c.provider.Set(ctx, change)
	phase := "set-http-path"
	if root {
		phase = "set-https-root"
	}
	c.recordReceiptEvent(operationID, phase, target, mode, setErr)
	if setErr != nil {
		return receipt, setErr
	}
	verifiedRoute, verifyErr := c.verifyHTTPSRoute(ctx, target, mode, providerKey, path, httpsPort, root, localhostBackendAlias, backend)
	c.recordVerificationEvent(operationID, target, mode, verifyErr)
	if verifyErr != nil {
		receipt.Error = ptr(fault.AsAppError(verifyErr).Safe())
		return receipt, verifyErr
	}
	if verifiedRoute.ID == "" {
		return receipt, fault.NewError(fault.ErrVerification, "exposure", "HTTPS route was not observed after configuration", true, "unknown", "Refresh and inspect Tailscale before retrying.")
	}
	receipt.Verified = true
	c.markManagedRoute(verifiedRoute)
	return receipt, nil
}

func (c exactOperationDependencies) replaceRawTCPWithHTTPSRoot(ctx context.Context, target targetmodel.Target, previous exposuredata.ExposureRoute, path string, httpsPort int, localhostBackendAlias bool, backend, operationID string, operationStarted time.Time) (exposuredata.OperationReceipt, error) {
	if path != "/" || previous.Mode != exposuredata.ExposureServe || previous.Target.Normalized().Key() != target.Normalized().Key() {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "raw-TCP conversion does not match one exact private HTTPS root", false, "changed", "Refresh and select the exact Serve TCP route and local listener before retrying.")
	}
	if err := c.requireCurrentListener(ctx, target); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	var precondition tailscale.ExposurePrecondition
	// Refresh all routes before removal so the provider receives a full-set
	// precondition and the selected identity is revalidated at the mutation seam.
	before, err := c.provider.List(ctx)
	if err != nil || !before.Authoritative || before.Error != nil {
		if err == nil {
			err = fault.NewError(fault.ErrUnknown, "exposure", "provider route state is not authoritative before conversion", true, "unknown", "Refresh Tailscale state; the raw-TCP route was not changed.")
		}
		return exposuredata.OperationReceipt{}, err
	}
	currentMatches := 0
	for _, route := range before.Routes {
		if routeFingerprint(route) == routeFingerprint(previous) {
			currentMatches++
		}
	}
	if currentMatches != 1 {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the exact raw-TCP route changed before conversion", true, "changed", "Refresh and review the current exact route before confirming conversion.")
	}
	precondition = tailscale.ExposurePrecondition{
		RouteIDs:      RouteIDs(before.Routes, target),
		RouteIDsHash:  RouteIDsHash(before.Routes, target),
		AllRoutesHash: tailscale.RoutesHash(before.Routes),
	}
	removed, removeErr := c.provider.Remove(ctx, tailscale.RouteSelector{ID: previous.ProviderKey, Target: &target, Mode: previous.Mode, Service: previous.Service, Path: previous.Path, Backend: previous.Backend, AllRoutesHash: precondition.AllRoutesHash}, precondition.RouteIDsHash)
	c.recordReceiptEvent(operationID, "remove-before-https-root", target, previous.Mode, removeErr)
	if removeErr != nil {
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "raw-TCP removal failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, removeErr
	}
	absent, afterRemove, verifyRemoveErr := c.verifyAbsent(ctx, previous)
	if verifyRemoveErr != nil || !absent {
		if verifyRemoveErr == nil {
			verifyRemoveErr = fault.NewError(fault.ErrVerification, "exposure", "raw-TCP removal could not be verified before HTTPS-root conversion", true, "unknown", "Refresh and inspect the exact route before retrying.")
		}
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "raw-TCP removal verification failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, verifyRemoveErr
	}
	c.revokeManagedRoute(previous)
	endpointRoutes, endpointErr := exactRoutesAtPort(afterRemove.Routes, httpsPort)
	if endpointErr != nil || len(endpointRoutes) != 0 {
		if endpointErr == nil {
			endpointErr = fault.NewError(fault.ErrUnsafe, "exposure", "another handler appeared on the HTTPS port after raw-TCP removal", true, "changed", "Tailge will not overwrite the changed endpoint; inspect it and restore the raw route manually if needed.")
		}
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS endpoint changed and exact raw-TCP restoration could not be verified", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, endpointErr
	}
	if err := c.requireCurrentListener(ctx, target); err != nil {
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "listener changed during HTTPS-root conversion and exact raw-TCP restoration failed", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, err
	}
	precondition = tailscale.ExposurePrecondition{
		RouteIDs:      RouteIDs(afterRemove.Routes, target),
		RouteIDsHash:  RouteIDsHash(afterRemove.Routes, target),
		AllRoutesHash: tailscale.RoutesHash(afterRemove.Routes),
	}
	if precondition.RouteIDsHash == "" || precondition.AllRoutesHash == "" {
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root preflight failed and exact raw-TCP restoration failed", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, fault.NewError(fault.ErrUnsafe, "exposure", "HTTPS-root preflight after raw-TCP removal is incomplete", true, "unknown", "Refresh provider state before retrying; the exact raw-TCP route was restored.")
	}
	providerKey := "serve:https=" + strconv.Itoa(httpsPort)
	setReceipt, setErr := c.provider.Set(ctx, tailscale.ExposureChange{Target: target, Mode: exposuredata.ExposureServe, ProviderKey: providerKey, Path: path, HTTPSPort: httpsPort, HTTPSRoot: true, Backend: backend, Preconditions: precondition})
	c.recordReceiptEvent(operationID, "set-https-root", target, exposuredata.ExposureServe, setErr)
	if setErr != nil {
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, true)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return setReceipt, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root apply failed and exact raw-TCP restoration could not be verified", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return setReceipt, setErr
	}
	verifiedRoute, verifyErr := c.verifyHTTPSRoute(ctx, target, exposuredata.ExposureServe, providerKey, path, httpsPort, true, localhostBackendAlias, backend)
	if verifyErr == nil && verifiedRoute.ID != "" {
		verifyErr = c.verifySingleRouteAtPort(ctx, httpsPort, verifiedRoute)
	}
	c.recordVerificationEvent(operationID, target, exposuredata.ExposureServe, verifyErr)
	if verifyErr != nil || verifiedRoute.ID == "" {
		if verifyErr == nil {
			verifyErr = fault.NewError(fault.ErrVerification, "exposure", "HTTPS root was not observed after raw-TCP conversion", true, "unknown", "Refresh and inspect Tailscale before retrying.")
		}
		rollbackErr := c.restoreRawTCPAfterHTTPSRootFailure(target, previous, httpsPort, backend, operationID, true)
		c.recordReceiptEvent(operationID, "rollback-raw-tcp", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return setReceipt, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root verification failed and exact raw-TCP restoration could not be verified", false, "unverified", "Inspect the exact Serve route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return setReceipt, verifyErr
	}
	setReceipt.Verified = true
	c.markManagedRoute(verifiedRoute)
	return setReceipt, nil
}

func (c exactOperationDependencies) restoreRawTCPAfterHTTPSRootFailure(requested targetmodel.Target, previous exposuredata.ExposureRoute, httpsPort int, backend, operationID string, removePartialRoot bool) error {
	requested = requested.Normalized()
	if previous.Mode != exposuredata.ExposureServe || previous.Kind != exposuredata.RouteKindRawTCP || previous.Target.Normalized().Key() != requested.Key() || !tailscale.RawTCPBackendMatchesTarget(requested, previous.Backend) {
		return fault.NewError(fault.ErrUnsafe, "exposure", "captured raw-TCP route no longer matches the selected private listener", false, "changed", "Inspect the exact Serve route manually; Tailge will not restore a different target.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := c.provider.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return fault.NewError(fault.ErrUnknown, "exposure", "rollback route state is not authoritative", true, "unknown", "Inspect Tailscale manually; exact raw-TCP restoration was not attempted.")
	}
	endpointRoutes, err := exactRoutesAtPort(snapshot.Routes, httpsPort)
	if err != nil {
		return err
	}
	if len(endpointRoutes) == 1 && routeFingerprint(endpointRoutes[0]) == routeFingerprint(previous) {
		if previous.Ownership == exposuredata.OwnershipManaged {
			c.markManagedRoute(endpointRoutes[0])
		}
		return nil
	}
	if len(endpointRoutes) > 1 {
		return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found multiple handlers on the exact HTTPS port", false, "changed", "Inspect the endpoint manually; Tailge will not overwrite or reset shared handlers.")
	}
	if len(endpointRoutes) == 1 {
		if !removePartialRoot {
			return fault.NewError(fault.ErrUnsafe, "exposure", "an unexpected handler appeared before HTTPS-root apply", false, "changed", "Tailge will not remove a route it did not attempt to create; inspect the endpoint and restore raw TCP manually if needed.")
		}
		partial := endpointRoutes[0]
		backendTarget, targetErr := targetmodel.ParseTarget(backend, "tcp")
		if targetErr != nil || partial.ID == "" || partial.ProviderKey != "serve:https="+strconv.Itoa(httpsPort) || partial.Mode != exposuredata.ExposureServe || partial.Kind != exposuredata.RouteKindHTTPSRoot || partial.Path != "/" || partial.Service != "" || partial.State != exposuredata.ExposureActive || partial.Target.Normalized().Key() != backendTarget.Normalized().Key() || !tailscale.HTTPPathBackendMatches(partial.Backend, backend) {
			return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found an unexpected route on the HTTPS port", false, "changed", "Inspect the exact route manually; Tailge will not remove an unrecognized handler.")
		}
		precondition := tailscale.ExposurePrecondition{RouteIDs: RouteIDs(snapshot.Routes, partial.Target), RouteIDsHash: RouteIDsHash(snapshot.Routes, partial.Target), AllRoutesHash: tailscale.RoutesHash(snapshot.Routes)}
		_, removeErr := c.provider.Remove(ctx, tailscale.RouteSelector{ID: partial.ProviderKey, Target: &partial.Target, Mode: partial.Mode, Service: partial.Service, Path: partial.Path, Backend: partial.Backend, AllRoutesHash: precondition.AllRoutesHash}, precondition.RouteIDsHash)
		c.recordReceiptEvent(operationID, "remove-partial-https-root", partial.Target, partial.Mode, removeErr)
		if removeErr != nil {
			return removeErr
		}
		absent, afterRemove, verifyErr := c.verifyAbsent(ctx, partial)
		if verifyErr != nil {
			return verifyErr
		}
		if !absent {
			return fault.NewError(fault.ErrVerification, "exposure", "partial HTTPS root removal could not be verified", true, "unknown", "Inspect Tailscale manually; exact raw-TCP restoration was not attempted.")
		}
		snapshot = afterRemove
		endpointRoutes, err = exactRoutesAtPort(snapshot.Routes, httpsPort)
		if err != nil {
			return err
		}
		if len(endpointRoutes) != 0 {
			return fault.NewError(fault.ErrUnsafe, "exposure", "the HTTPS port changed during rollback", false, "changed", "Inspect the exact endpoint manually; Tailge will not overwrite a concurrent route.")
		}
	}
	caps, err := c.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !caps.Serve || !caps.ExactServe || !caps.ServeTCP || !exactRollbackSelector(previous, caps) {
		return fault.NewError(fault.ErrUnsupported, "exposure", "the captured raw-TCP route cannot be restored with current exact Serve capabilities", false, "read_only", "Inspect Tailscale manually; Tailge will not use a broad reset.")
	}
	precondition := tailscale.ExposurePrecondition{RouteIDs: RouteIDs(snapshot.Routes, previous.Target), RouteIDsHash: RouteIDsHash(snapshot.Routes, previous.Target), AllRoutesHash: tailscale.RoutesHash(snapshot.Routes)}
	_, setErr := c.provider.Set(ctx, tailscale.ExposureChange{Target: previous.Target, Mode: previous.Mode, ProviderKey: previous.ProviderKey, Service: previous.Service, Path: previous.Path, Backend: previous.Backend, Preconditions: precondition})
	c.recordReceiptEvent(operationID, "restore-raw-tcp", previous.Target, previous.Mode, setErr)
	verified, verifyErr := c.verifyMode(ctx, previous.Target, previous.Mode, previous.ProviderKey, previous.Service, previous.Path, previous.Backend)
	if verifyErr != nil {
		if setErr != nil {
			return fault.WrapError(fault.ErrVerification, "exposure", "raw-TCP restoration failed", false, "unverified", "Inspect the exact route manually; restoration was not verified.", setErr)
		}
		return verifyErr
	}
	if err := c.verifySingleRouteAtPort(ctx, httpsPort, verified); err != nil {
		return err
	}
	if previous.Ownership == exposuredata.OwnershipManaged {
		c.markManagedRoute(verified)
	}
	return nil
}

func (c exactOperationDependencies) verifySingleRouteAtPort(ctx context.Context, port int, expected exposuredata.ExposureRoute) error {
	snapshot, err := c.provider.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return fault.NewError(fault.ErrUnknown, "exposure", "endpoint state is not authoritative after HTTPS-root mutation", true, "unknown", "Inspect Tailscale manually; Tailge cannot verify that the exact route set is restored.")
	}
	routes, err := exactRoutesAtPort(snapshot.Routes, port)
	if err != nil {
		return err
	}
	if len(routes) != 1 || routes[0].State != exposuredata.ExposureActive || !sameProviderRouteIdentity(routes[0], expected) {
		return fault.NewError(fault.ErrVerification, "exposure", "the exact endpoint route set could not be verified", true, "unknown", "Inspect Tailscale manually; Tailge will not remove or overwrite a concurrent route.")
	}
	return nil
}

func sameProviderRouteIdentity(left, right exposuredata.ExposureRoute) bool {
	return left.ProviderKey == right.ProviderKey && left.Kind == right.Kind && left.Service == right.Service && left.Path == right.Path &&
		left.Target.Normalized().Key() == right.Target.Normalized().Key() && left.Mode == right.Mode && left.URL == right.URL && left.Backend == right.Backend
}

func exactRoutesAtPort(routes []exposuredata.ExposureRoute, port int) ([]exposuredata.ExposureRoute, error) {
	matches := make([]exposuredata.ExposureRoute, 0)
	for _, route := range routes {
		if route.ProviderKey == "" {
			return nil, fault.NewError(fault.ErrUnknown, "exposure", "a route has no exact provider selector during conversion", true, "unknown", "Inspect Tailscale manually; Tailge will not assume the endpoint is free.")
		}
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if err != nil {
			return nil, fault.NewError(fault.ErrUnknown, "exposure", "a route has an unrecognized provider selector during conversion", true, "unknown", "Inspect Tailscale manually; Tailge will not assume the endpoint is free.")
		}
		if selector.Port == port {
			matches = append(matches, route)
		}
	}
	return matches, nil
}

func (c exactOperationDependencies) verifyHTTPSRoute(ctx context.Context, target targetmodel.Target, mode exposuredata.ExposureMode, providerKey, path string, httpsPort int, root, localhostBackendAlias bool, backend string) (exposuredata.ExposureRoute, error) {
	backendTarget, targetErr := targetmodel.ParseTarget(backend, "tcp")
	if targetErr != nil {
		return exposuredata.ExposureRoute{}, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS backend identity is invalid", false, "unknown", "Refresh and inspect the exact local backend before retrying.", targetErr)
	}
	kind := exposuredata.RouteKindHTTPPath
	if root {
		kind = exposuredata.RouteKindHTTPSRoot
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, err := c.provider.List(ctx)
		if err == nil && snapshot.Authoritative && snapshot.Error == nil {
			matches := make([]exposuredata.ExposureRoute, 0, 1)
			for _, route := range snapshot.Routes {
				if route.Kind != kind || route.Mode != mode || route.ProviderKey != providerKey || route.Path != path || !tailscale.HTTPPathBackendMatches(route.Backend, backend) || route.Target.Normalized().Key() != backendTarget.Normalized().Key() {
					continue
				}
				observed, parseErr := url.Parse(route.URL)
				if parseErr != nil || !strings.EqualFold(observed.Scheme, "https") || observed.Hostname() == "" || observed.Path != path {
					continue
				}
				observedPort := observed.Port()
				if (httpsPort == 443 && observedPort != "") || (httpsPort != 443 && observedPort != strconv.Itoa(httpsPort)) {
					continue
				}
				matches = append(matches, route)
			}
			if len(matches) == 1 {
				return matches[0], nil
			}
			if len(matches) > 1 {
				return exposuredata.ExposureRoute{}, fault.NewError(fault.ErrAmbiguous, "exposure", "multiple provider handlers match the requested HTTPS route", false, "ambiguous", "Inspect Tailscale and choose one exact handler.")
			}
		}
		select {
		case <-ctx.Done():
			return exposuredata.ExposureRoute{}, verificationContextError("HTTPS route could not be verified", ctx.Err())
		case <-deadline.C:
			remediation := "Refresh and inspect Tailscale before retrying."
			if !localhostBackendAlias && (target.Normalized().Address == "::1" || target.Normalized().Address == "::") {
				remediation = "If Tailscale cannot proxy the numeric IPv6 backend, first disable this exact handler, then explicitly recreate with `--localhost-backend`; this uses hostname resolution and weakens the exact-address guarantee."
			}
			return exposuredata.ExposureRoute{}, fault.NewError(fault.ErrVerification, "exposure", "HTTPS route could not be verified", true, "unknown", remediation)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func findRoute(routes []exposuredata.ExposureRoute, id string) exposuredata.ExposureRoute {
	for _, route := range routes {
		if route.ID == id {
			return route
		}
	}
	return exposuredata.ExposureRoute{}
}
