package exposure

import (
	"context"
	"errors"
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
	dependencies        exactOperationDependencies
	target              targetmodel.Target
	mode                exposuredata.ExposureMode
	selectedProviderKey string
	selectedRouteMode   exposuredata.ExposureMode
	confirmFunnel       bool
	confirmExternal     bool
	timeout             time.Duration
	approval            *MutationApproval
	operationID         string
	operationStarted    time.Time
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
	allRouteIDs := RouteIDs(exposures.Routes, target)
	routeIDs := allRouteIDs
	if mode == exposuredata.ExposureDisabled && (selectedProviderKey != "" || selectedRouteMode != exposuredata.ExposureDisabled) {
		selected := make([]string, 0, 1)
		for _, route := range exposures.Routes {
			if !Matches(route.Target, target) {
				continue
			}
			if selectedProviderKey != "" && route.ProviderKey != selectedProviderKey {
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
		verified, _, verifyErr := c.verifyAbsent(operationCtx, target, route.ID)
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
			verified, afterRemove, verifyErr := c.verifyAbsent(operationCtx, target, route.ID)
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
	receipt, setErr := c.provider.Set(operationCtx, tailscale.ExposureChange{Target: target, Mode: mode, Service: replacementService, Path: replacementPath, Backend: replacementBackend, Preconditions: precondition})
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
	if _, err := c.provider.Set(ctx, tailscale.ExposureChange{Target: previous.Target, Mode: previous.Mode, ProviderKey: previous.ProviderKey, Service: previous.Service, Path: previous.Path, Backend: previous.Backend, Preconditions: precondition}); err != nil {
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

func (c exactOperationDependencies) verifyAbsent(ctx context.Context, target targetmodel.Target, routeID string) (bool, exposuredata.ExposureSnapshot, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		snapshot, err := c.provider.List(ctx)
		if err == nil && snapshot.Authoritative && snapshot.Error == nil {
			present := false
			for _, route := range snapshot.Routes {
				// The target may legitimately retain another exposure route (for
				// example Serve after removing Funnel). Verification must therefore
				// follow the exact route identity, not the broad target match.
				if route.ID == routeID {
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
func findRoute(routes []exposuredata.ExposureRoute, id string) exposuredata.ExposureRoute {
	for _, route := range routes {
		if route.ID == id {
			return route
		}
	}
	return exposuredata.ExposureRoute{}
}
