package exposure

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

// replaceExactRouteWithHTTPSRoot atomically replaces one captured raw-TCP or
// HTTPS-root route with an HTTPS root. It intentionally rejects path handlers
// and shared endpoints: changing access scope must not silently affect sibling
// services.
func (c exactOperationDependencies) replaceExactRouteWithHTTPSRoot(
	ctx context.Context,
	target targetmodel.Target,
	expectedRouteID string,
	mode exposuredata.ExposureMode,
	path string,
	httpsPort int,
	localhostBackendAlias bool,
	backend string,
	confirmExternal bool,
	routes []exposuredata.ExposureRoute,
	operationID string,
	operationStarted time.Time,
) (exposuredata.OperationReceipt, error) {
	if path != "/" || expectedRouteID == "" {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrInvalidInput, "exposure", "exact HTTPS-root replacement requires one captured route", false, "invalid", "Refresh and select one exact route before confirming replacement.")
	}
	target = target.Normalized()
	var previous *exposuredata.ExposureRoute
	for i := range routes {
		if routes[i].ID != expectedRouteID {
			continue
		}
		if previous != nil {
			return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "the selected route identity is duplicated", false, "ambiguous", "Refresh and resolve the duplicate route before replacing it.")
		}
		previous = &routes[i]
	}
	if previous == nil {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route changed after preview", true, "changed", "Refresh and review the exact route before confirming replacement.")
	}
	if err := validateHTTPSRootReplacementSource(*previous, target, true); err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	if previous.Mode == mode && previous.Kind == exposuredata.RouteKindHTTPSRoot &&
		previous.ProviderKey == string(mode)+":https="+strconv.Itoa(httpsPort) &&
		httpsRootMatchesCapturedBackend(*previous, target, backend) {
		return exposuredata.OperationReceipt{ID: previous.ID, StartedAt: operationStarted, FinishedAt: c.currentTime(), Verified: true}, nil
	}
	if previous.Ownership != exposuredata.OwnershipManaged && !confirmExternal {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the selected route has unknown/external ownership", false, "external", "Review the exact route and confirm its replacement.")
	}
	previousSelector, err := tailscale.ParseListenerSelector(previous.ProviderKey, previous.Mode)
	if err != nil {
		return exposuredata.OperationReceipt{}, fault.WrapError(fault.ErrUnsafe, "exposure", "the selected route has no exact endpoint identity", false, "unsafe", "Refresh provider state; Tailge will not guess a rollback endpoint.", err)
	}
	caps, err := c.provider.Capabilities(ctx)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	if !exactModeSupported(caps, previous.Mode) || !exactRollbackSelector(*previous, caps) ||
		(previousSelector.Transport == "tcp" && ((previous.Mode == exposuredata.ExposureServe && !caps.ServeTCP) || (previous.Mode == exposuredata.ExposureFunnel && !caps.FunnelTCP))) ||
		(previousSelector.Transport == "https" && ((previous.Mode == exposuredata.ExposureServe && (!caps.ServeHTTPS || !caps.ServePath)) || (previous.Mode == exposuredata.ExposureFunnel && (!caps.FunnelHTTPS || !caps.FunnelPath)))) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsupported, "exposure", "the captured route cannot be restored exactly with current provider capabilities", false, "read_only", "Leave it unchanged or use a Tailscale version with exact route rollback support.")
	}
	oldEndpoint, err := exactRoutesAtPort(routes, previousSelector.Port)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	if len(oldEndpoint) != 1 || oldEndpoint[0].ID != previous.ID {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrAmbiguous, "exposure", "the selected route shares its endpoint with other handlers", false, "ambiguous", "Resolve every handler on the exact endpoint before changing its access scope.")
	}
	newEndpoint, err := exactRoutesAtPort(routes, httpsPort)
	if err != nil {
		return exposuredata.OperationReceipt{}, err
	}
	if len(newEndpoint) != 0 && !(previousSelector.Port == httpsPort && len(newEndpoint) == 1 && newEndpoint[0].ID == previous.ID) {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the requested HTTPS endpoint is already occupied", false, "conflict", "Choose an unoccupied exact HTTPS endpoint; Tailge will not overwrite another handler.")
	}
	if err := c.requireCurrentListener(ctx, target); err != nil {
		return exposuredata.OperationReceipt{}, err
	}

	before, err := c.provider.List(ctx)
	if err != nil || !before.Authoritative || before.Error != nil {
		if err == nil {
			err = fault.NewError(fault.ErrUnknown, "exposure", "provider route state is not authoritative before HTTPS replacement", true, "unknown", "Refresh Tailscale state; the captured route was not changed.")
		}
		return exposuredata.OperationReceipt{}, err
	}
	currentMatches := 0
	for _, route := range before.Routes {
		if routeFingerprint(route) == routeFingerprint(*previous) {
			currentMatches++
		}
	}
	if currentMatches != 1 {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrUnsafe, "exposure", "the exact route changed before HTTPS replacement", true, "changed", "Refresh and review the current exact route before confirming replacement.")
	}
	beforeOldEndpoint, err := exactRoutesAtPort(before.Routes, previousSelector.Port)
	if err != nil || len(beforeOldEndpoint) != 1 || routeFingerprint(beforeOldEndpoint[0]) != routeFingerprint(*previous) {
		if err == nil {
			err = fault.NewError(fault.ErrUnsafe, "exposure", "the captured endpoint changed before HTTPS replacement", true, "changed", "Refresh and review every exact handler before confirming replacement.")
		}
		return exposuredata.OperationReceipt{}, err
	}
	beforeNewEndpoint, err := exactRoutesAtPort(before.Routes, httpsPort)
	if err != nil || (len(beforeNewEndpoint) != 0 && !(previousSelector.Port == httpsPort && len(beforeNewEndpoint) == 1 && routeFingerprint(beforeNewEndpoint[0]) == routeFingerprint(*previous))) {
		if err == nil {
			err = fault.NewError(fault.ErrUnsafe, "exposure", "the requested HTTPS endpoint changed before replacement", true, "changed", "Refresh and review the exact endpoint before confirming replacement.")
		}
		return exposuredata.OperationReceipt{}, err
	}
	precondition := tailscale.ExposurePrecondition{RouteIDs: RouteIDs(before.Routes, target), RouteIDsHash: RouteIDsHash(before.Routes, target), AllRoutesHash: tailscale.RoutesHash(before.Routes)}
	removed, removeErr := c.provider.Remove(ctx, tailscale.RouteSelector{
		ID: previous.ProviderKey, Target: &target, Mode: previous.Mode, Service: previous.Service,
		Path: previous.Path, Backend: previous.Backend, AllRoutesHash: precondition.AllRoutesHash,
	}, precondition.RouteIDsHash)
	c.recordReceiptEvent(operationID, "remove-before-https-root", target, previous.Mode, removeErr)
	if removeErr != nil {
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "route removal failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, removeErr
	}
	absent, afterRemove, verifyRemoveErr := c.verifyAbsent(ctx, *previous)
	if verifyRemoveErr != nil || !absent {
		if verifyRemoveErr == nil {
			verifyRemoveErr = fault.NewError(fault.ErrVerification, "exposure", "exact route removal could not be verified before HTTPS replacement", true, "unknown", "Refresh and inspect the exact route before retrying.")
		}
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "route removal verification failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, verifyRemoveErr
	}
	c.revokeManagedRoute(*previous)
	newEndpoint, endpointErr := exactRoutesAtPort(afterRemove.Routes, httpsPort)
	if endpointErr != nil || len(newEndpoint) != 0 {
		if endpointErr == nil {
			endpointErr = fault.NewError(fault.ErrUnsafe, "exposure", "another handler appeared on the HTTPS endpoint after route removal", true, "changed", "Tailge will not overwrite changed endpoint state; inspect it and the prior route manually.")
		}
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "the HTTPS endpoint changed and exact route restoration failed", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, endpointErr
	}
	if err := c.requireCurrentListener(ctx, target); err != nil {
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "listener changed during HTTPS replacement and exact route restoration failed", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, err
	}
	precondition = tailscale.ExposurePrecondition{RouteIDs: RouteIDs(afterRemove.Routes, target), RouteIDsHash: RouteIDsHash(afterRemove.Routes, target), AllRoutesHash: tailscale.RoutesHash(afterRemove.Routes)}
	if precondition.RouteIDsHash == "" || precondition.AllRoutesHash == "" {
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, false)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return removed, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root preflight failed and exact route restoration failed", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return removed, fault.NewError(fault.ErrUnsafe, "exposure", "HTTPS-root preflight after route removal is incomplete", true, "unknown", "Refresh provider state before retrying; the exact prior route was restored.")
	}
	providerKey := string(mode) + ":https=" + strconv.Itoa(httpsPort)
	setReceipt, setErr := c.provider.Set(ctx, tailscale.ExposureChange{
		Target: target, Mode: mode, ProviderKey: providerKey, Path: path, HTTPSPort: httpsPort,
		HTTPSRoot: true, Backend: backend, Preconditions: precondition,
	})
	c.recordReceiptEvent(operationID, "set-https-root", target, mode, setErr)
	if setErr != nil {
		setMayHaveApplied := strings.TrimSpace(setReceipt.Command) != ""
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, setMayHaveApplied)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return setReceipt, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root apply failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return setReceipt, setErr
	}
	verifiedRoute, verifyErr := c.verifyHTTPSRoute(ctx, target, mode, providerKey, path, httpsPort, true, localhostBackendAlias, backend)
	if verifyErr == nil && verifiedRoute.ID != "" {
		verifyErr = c.verifySingleRouteAtPort(ctx, httpsPort, verifiedRoute)
	}
	c.recordVerificationEvent(operationID, target, mode, verifyErr)
	if verifyErr != nil || verifiedRoute.ID == "" {
		if verifyErr == nil {
			verifyErr = fault.NewError(fault.ErrVerification, "exposure", "HTTPS root was not observed after exact route replacement", true, "unknown", "Refresh and inspect Tailscale before retrying.")
		}
		rollbackErr := c.restoreRouteAfterHTTPSRootFailure(target, *previous, mode, httpsPort, backend, operationID, true)
		c.recordReceiptEvent(operationID, "rollback-previous-route", target, previous.Mode, rollbackErr)
		if rollbackErr != nil {
			return setReceipt, fault.WrapError(fault.ErrVerification, "exposure", "HTTPS-root verification failed and exact route restoration could not be verified", false, "unverified", "Inspect the exact prior route manually; Tailge did not apply a broad reset.", rollbackErr)
		}
		return setReceipt, verifyErr
	}
	setReceipt.Verified = true
	c.markManagedRoute(verifiedRoute)
	return setReceipt, nil
}

func validateHTTPSRootReplacementSource(route exposuredata.ExposureRoute, target targetmodel.Target, localhostBackendAlias bool) error {
	selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
	if err != nil || route.ID == "" || route.State != exposuredata.ExposureActive || route.Service != "" {
		return fault.NewError(fault.ErrUnsafe, "exposure", "the selected route has incomplete exact identity", false, "unsafe", "Refresh and select one active route that matches the exact local listener.")
	}
	switch route.Kind {
	case exposuredata.RouteKindRawTCP:
		if route.Target.Normalized().Key() != target.Normalized().Key() || selector.Transport != "tcp" || route.Path != "" || !tailscale.RawTCPBackendMatchesTarget(target, route.Backend) {
			return fault.NewError(fault.ErrUnsafe, "exposure", "the raw-TCP route does not match the exact local listener backend", false, "conflict", "Refresh and select the exact raw-TCP route for this listener.")
		}
	case exposuredata.RouteKindHTTPSRoot:
		if selector.Transport != "https" || route.Path != "/" || !httpsRootSourceBackendMatches(route, target, localhostBackendAlias) {
			return fault.NewError(fault.ErrUnsafe, "exposure", "the existing HTTPS root does not match the exact local listener backend", false, "conflict", "Refresh and select the exact HTTPS root for this listener.")
		}
	default:
		return fault.NewError(fault.ErrUnsafe, "exposure", "only exact raw-TCP routes and HTTPS roots can be replaced by S/F", false, "intent_required", "Manage named HTTPS paths through their exact route action.")
	}
	return nil
}

func httpsRootSourceBackendMatches(route exposuredata.ExposureRoute, target targetmodel.Target, localhostBackendAlias bool) bool {
	target = target.Normalized()
	backends := []string{tailscale.HTTPPathBackendArgument(target)}
	if localhostBackendAlias && (target.Address == "::1" || target.Address == "::") {
		backends = append(backends, tailscale.HTTPSBackendArgument(target, true))
	}
	for _, backend := range backends {
		parsed, err := targetmodel.ParseTarget(backend, "tcp")
		if err == nil && parsed.Normalized().Port == target.Port && route.Target.Normalized().Key() == parsed.Normalized().Key() && tailscale.HTTPPathBackendMatches(route.Backend, backend) {
			return true
		}
	}
	return false
}

func httpsRootMatchesCapturedBackend(route exposuredata.ExposureRoute, target targetmodel.Target, backend string) bool {
	parsed, err := targetmodel.ParseTarget(backend, "tcp")
	return err == nil && route.Target.Normalized().Key() == parsed.Normalized().Key() && parsed.Normalized().Port == target.Normalized().Port && tailscale.HTTPPathBackendMatches(route.Backend, backend)
}

func (c exactOperationDependencies) restoreRouteAfterHTTPSRootFailure(target targetmodel.Target, previous exposuredata.ExposureRoute, attemptedMode exposuredata.ExposureMode, attemptedPort int, attemptedBackend, operationID string, removePartialRoot bool) error {
	target = target.Normalized()
	if err := validateHTTPSRootReplacementSource(previous, target, true); err != nil {
		return err
	}
	previousSelector, err := tailscale.ParseListenerSelector(previous.ProviderKey, previous.Mode)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := c.provider.List(ctx)
	if err != nil {
		return err
	}
	if !snapshot.Authoritative || snapshot.Error != nil {
		return fault.NewError(fault.ErrUnknown, "exposure", "rollback route state is not authoritative", true, "unknown", "Inspect Tailscale manually; exact route restoration was not attempted.")
	}
	oldEndpoint, err := exactRoutesAtPort(snapshot.Routes, previousSelector.Port)
	if err != nil {
		return err
	}
	for _, route := range oldEndpoint {
		if routeFingerprint(route) == routeFingerprint(previous) && route.State == exposuredata.ExposureActive {
			if len(oldEndpoint) != 1 {
				return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found additional handlers at the captured endpoint", false, "changed", "Inspect the exact endpoint manually; Tailge will not overwrite or reset shared handlers.")
			}
			if previous.Ownership == exposuredata.OwnershipManaged {
				c.markManagedRoute(route)
			}
			return nil
		}
	}
	if len(oldEndpoint) != 0 {
		return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found a changed handler at the captured endpoint", false, "changed", "Inspect the exact endpoint manually; Tailge will not overwrite a concurrent route.")
	}

	attemptedKey := string(attemptedMode) + ":https=" + strconv.Itoa(attemptedPort)
	attemptedEndpoint, err := exactRoutesAtPort(snapshot.Routes, attemptedPort)
	if err != nil {
		return err
	}
	if len(attemptedEndpoint) != 0 {
		if !removePartialRoot || len(attemptedEndpoint) != 1 || !matchesHTTPSRoot(attemptedEndpoint[0], target, attemptedMode, attemptedKey, attemptedPort, attemptedBackend) {
			if previousSelector.Port == attemptedPort {
				return fault.NewError(fault.ErrUnsafe, "exposure", "rollback found an unexpected route at the captured endpoint", false, "changed", "Inspect the exact route manually; Tailge will not remove an unrecognized or concurrent handler.")
			}
		} else {
			partial := attemptedEndpoint[0]
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
				return fault.NewError(fault.ErrVerification, "exposure", "partial HTTPS root removal could not be verified", true, "unknown", "Inspect Tailscale manually; exact route restoration was not attempted.")
			}
			snapshot = afterRemove
		}
	}
	oldEndpoint, err = exactRoutesAtPort(snapshot.Routes, previousSelector.Port)
	if err != nil {
		return err
	}
	if len(oldEndpoint) != 0 {
		return fault.NewError(fault.ErrUnsafe, "exposure", "the captured endpoint changed during rollback", false, "changed", "Inspect the exact endpoint manually; Tailge will not overwrite a concurrent route.")
	}
	caps, err := c.provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !exactModeSupported(caps, previous.Mode) || !exactRollbackSelector(previous, caps) {
		return fault.NewError(fault.ErrUnsupported, "exposure", "the captured route cannot be restored with current exact provider capabilities", false, "read_only", "Inspect Tailscale manually; Tailge will not use a broad reset.")
	}
	if selector, parseErr := tailscale.ParseListenerSelector(previous.ProviderKey, previous.Mode); parseErr != nil || (selector.Transport == "tcp" && ((previous.Mode == exposuredata.ExposureServe && !caps.ServeTCP) || (previous.Mode == exposuredata.ExposureFunnel && !caps.FunnelTCP))) || (selector.Transport == "https" && ((previous.Mode == exposuredata.ExposureServe && (!caps.ServeHTTPS || !caps.ServePath)) || (previous.Mode == exposuredata.ExposureFunnel && (!caps.FunnelHTTPS || !caps.FunnelPath)))) {
		return fault.NewError(fault.ErrUnsupported, "exposure", "the captured route transport cannot be restored exactly", false, "read_only", "Inspect Tailscale manually; Tailge will not guess a rollback command.")
	}
	precondition := tailscale.ExposurePrecondition{RouteIDs: RouteIDs(snapshot.Routes, previous.Target), RouteIDsHash: RouteIDsHash(snapshot.Routes, previous.Target), AllRoutesHash: tailscale.RoutesHash(snapshot.Routes)}
	if precondition.RouteIDsHash == "" || precondition.AllRoutesHash == "" {
		return fault.NewError(fault.ErrUnsafe, "exposure", "rollback preflight is incomplete", true, "unknown", "Refresh Tailscale manually; exact route restoration was not attempted.")
	}
	change := tailscale.ExposureChange{Target: previous.Target, Mode: previous.Mode, ProviderKey: previous.ProviderKey, Service: previous.Service, Path: previous.Path, Backend: previous.Backend, Preconditions: precondition}
	if previous.Kind == exposuredata.RouteKindHTTPSRoot {
		change.HTTPSRoot = true
		change.HTTPSPort = previousSelector.Port
	}
	_, setErr := c.provider.Set(ctx, change)
	c.recordReceiptEvent(operationID, "restore-exact-route", previous.Target, previous.Mode, setErr)
	if setErr != nil {
		return setErr
	}
	var restored exposuredata.ExposureRoute
	if previous.Kind == exposuredata.RouteKindHTTPSRoot {
		restored, err = c.verifyHTTPSRoute(ctx, previous.Target, previous.Mode, previous.ProviderKey, previous.Path, previousSelector.Port, true, false, previous.Backend)
	} else {
		restored, err = c.verifyMode(ctx, previous.Target, previous.Mode, previous.ProviderKey, previous.Service, previous.Path, previous.Backend)
	}
	if err != nil {
		return err
	}
	if restored.State != exposuredata.ExposureActive || !sameProviderRouteIdentity(restored, previous) {
		return fault.NewError(fault.ErrVerification, "exposure", "the captured route identity was not restored", false, "unverified", "Inspect the exact route manually; Tailge could not verify restoration.")
	}
	if err := c.verifySingleRouteAtPort(ctx, previousSelector.Port, restored); err != nil {
		return err
	}
	if previous.Ownership == exposuredata.OwnershipManaged {
		c.markManagedRoute(restored)
	}
	return nil
}

func matchesHTTPSRoot(route exposuredata.ExposureRoute, target targetmodel.Target, mode exposuredata.ExposureMode, providerKey string, port int, backend string) bool {
	return route.ID != "" && route.State == exposuredata.ExposureActive && route.Mode == mode && route.ProviderKey == providerKey && route.Kind == exposuredata.RouteKindHTTPSRoot && route.Path == "/" && route.Service == "" && route.Target.Normalized().Port == target.Normalized().Port && httpsRootMatchesCapturedBackend(route, target, backend) && strings.HasSuffix(providerKey, ":https="+strconv.Itoa(port))
}
