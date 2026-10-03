package tui

import (
	"fmt"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

func (m *workspaceModel) discardHTTPSRootAction() {
	m.httpsRootAction = false
	m.httpsRootPort = 0
	m.httpsRootRouteID = ""
	m.httpsRootReplaceRoute = false
}

func (m *workspaceModel) httpsRootBaseReason(item exposure.ReconciledItem) string {
	if m.configErr != nil {
		return "HTTPS action unavailable: configuration is invalid"
	}
	if m.processBusy {
		return "HTTPS action unavailable: process termination is in progress"
	}
	if m.refreshState.isPending() {
		return "HTTPS action unavailable: refresh is in progress"
	}
	if m.viewErr != nil || !m.view.Listeners.Authoritative || m.view.Listeners.Stale || !m.view.Exposures.Authoritative || m.view.Exposures.Stale {
		return "HTTPS action unavailable: listener or provider state is stale or unknown"
	}
	if m.readyErr != nil || !m.hasReadiness {
		return "HTTPS action unavailable: Tailscale readiness is stale or unknown"
	}
	if item.Listener == nil {
		return "HTTPS action unavailable: select a current local listener"
	}
	switch item.State {
	case exposuredata.ExposureAmbiguous:
		return "HTTPS action unavailable: existing route identity is ambiguous"
	case exposuredata.ExposureUnknown, exposuredata.ExposureUnavailable, exposuredata.ExposureUnsupported:
		return "HTTPS action unavailable: listener or route identity is not authoritative"
	}
	if item.OperationState == exposuredata.ExposureApplying || item.State == exposuredata.ExposureApplying {
		return "HTTPS action unavailable: an operation is Applying for this listener"
	}
	if _, busy := m.activeOps[item.Listener.Target.Normalized().Key()]; busy {
		return "HTTPS action unavailable: an operation is Applying for this listener"
	}
	return ""
}

func httpsRootBackend(target targetmodel.Target) string {
	return tailscale.HTTPPathBackendArgument(target)
}

func httpsRootBackendMatches(route exposuredata.ExposureRoute, target targetmodel.Target, backend string) bool {
	parsedBackend, err := targetmodel.ParseTarget(backend, "tcp")
	return err == nil && parsedBackend.Normalized().Port == target.Normalized().Port &&
		route.Target.Normalized().Key() == parsedBackend.Normalized().Key() &&
		tailscale.HTTPPathBackendMatches(route.Backend, backend)
}

func httpsRootObservedBackendMatches(route exposuredata.ExposureRoute, target targetmodel.Target) bool {
	if httpsRootBackendMatches(route, target, httpsRootBackend(target)) {
		return true
	}
	target = target.Normalized()
	if target.Address != "::1" && target.Address != "::" {
		return false
	}
	return httpsRootBackendMatches(route, target, tailscale.HTTPSBackendArgument(target, true))
}

func (m *workspaceModel) httpsActionStatusLabel() string {
	status := func(mode exposuredata.ExposureMode) string {
		if m.actionAvailability(mode).Disabled {
			return "off"
		}
		return "ok"
	}
	return "s:" + status(exposuredata.ExposureServe) + " f:" + status(exposuredata.ExposureFunnel)
}

// httpsRootRoutePlan derives the endpoint and exact replacement from observed
// route identity. Serve roots use the local listener port. A current Funnel
// route keeps its HTTPS-capable public port; new Funnel roots default to 443.
func httpsRootRoutePlan(routes []exposuredata.ExposureRoute, item exposure.ReconciledItem, mode exposuredata.ExposureMode) (int, *exposuredata.ExposureRoute, bool, string) {
	if item.Listener == nil {
		return 0, nil, false, "HTTPS action unavailable: select a current local listener"
	}
	if mode != exposuredata.ExposureServe && mode != exposuredata.ExposureFunnel {
		return 0, nil, false, "HTTPS action unavailable: choose Serve or Funnel"
	}
	if len(item.Routes) > 1 {
		return 0, nil, false, "HTTPS action unavailable: multiple exact routes are configured; resolve one exact route first"
	}
	target := item.Listener.Target.Normalized()
	port := target.Port
	if mode == exposuredata.ExposureFunnel {
		port = 443
	}
	var replacement *exposuredata.ExposureRoute
	already := false
	if len(item.Routes) == 1 {
		route := item.Routes[0]
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if err != nil || route.ID == "" || route.State != exposuredata.ExposureActive {
			return 0, nil, false, "HTTPS action unavailable: existing route identity is incomplete"
		}
		if route.Mode == exposuredata.ExposureFunnel && selector.Transport == "https" && !tailscale.FunnelHTTPSPortSupported(selector.Port) {
			return 0, nil, false, "HTTPS action unavailable: existing Funnel HTTPS route cannot be restored on its unsupported port"
		}
		switch route.Kind {
		case exposuredata.RouteKindRawTCP:
			if selector.Transport != "tcp" || route.Service != "" || route.Path != "" || route.Target.Normalized().Key() != target.Key() || !tailscale.RawTCPBackendMatchesTarget(target, route.Backend) {
				return 0, nil, false, "HTTPS action unavailable: raw-TCP route does not match the exact local listener backend"
			}
		case exposuredata.RouteKindHTTPSRoot:
			if selector.Transport != "https" || route.Path != "/" || route.Service != "" || !httpsRootObservedBackendMatches(route, target) {
				return 0, nil, false, "HTTPS action unavailable: existing HTTPS root does not match the exact local listener backend"
			}
		case exposuredata.RouteKindHTTPPath:
			return 0, nil, false, "HTTPS action unavailable: manage the existing named HTTPS path through its exact route action"
		default:
			return 0, nil, false, "HTTPS action unavailable: existing route transport is not exact"
		}
		if mode == exposuredata.ExposureFunnel && route.Mode == exposuredata.ExposureFunnel && tailscale.FunnelHTTPSPortSupported(selector.Port) {
			port = selector.Port
		}
		replacement = &route
		already = route.Mode == mode && route.Kind == exposuredata.RouteKindHTTPSRoot && selector.Port == port && httpsRootBackendMatches(route, target, httpsRootBackend(target))
	}
	if mode == exposuredata.ExposureFunnel && !tailscale.FunnelHTTPSPortSupported(port) {
		return 0, nil, false, "HTTPS action unavailable: Funnel HTTPS supports ports 443, 8443, and 10000"
	}
	endpointRoutes := 0
	previousPort := 0
	if replacement != nil {
		selector, _ := tailscale.ParseListenerSelector(replacement.ProviderKey, replacement.Mode)
		previousPort = selector.Port
	}
	for _, route := range routes {
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if err != nil {
			return 0, nil, false, "HTTPS action unavailable: an observed route lacks exact provider endpoint identity"
		}
		if replacement != nil && route.ID == replacement.ID {
			continue
		}
		if selector.Port == port {
			endpointRoutes++
			if already && (route.Mode != mode || (route.Kind == exposuredata.RouteKindHTTPSRoot && route.Path == "/")) {
				return 0, nil, false, "HTTPS action unavailable: another route conflicts with this root endpoint"
			}
		}
		if !already && replacement != nil && selector.Port == previousPort {
			return 0, nil, false, "HTTPS action unavailable: the exact route shares its endpoint with other handlers"
		}
	}
	if already {
		return port, nil, true, ""
	}
	if endpointRoutes > 0 {
		return 0, nil, false, "HTTPS action unavailable: the requested HTTPS endpoint is already occupied"
	}
	return port, replacement, false, ""
}

func httpsRootReadinessReason(status readiness.ReadinessStatus, message, remediation string) string {
	reason := "HTTPS route readiness — " + string(status)
	if message != "" {
		reason += ": " + message
	}
	if remediation != "" {
		reason += ". Next: " + remediation
	}
	return reason
}

func (m *workspaceModel) openHTTPSRootPreview(mode exposuredata.ExposureMode) {
	items := m.actionItems()
	if len(items) == 0 || (len(items) > 1 && mode == exposuredata.ExposureFunnel) {
		m.setBanner("Funnel HTTPS roots share one public hostname and port; choose one exact listener at a time", true)
		return
	}
	if availability := m.actionAvailability(mode); availability.Disabled {
		m.setBanner(availability.Reason, true)
		return
	}
	item := items[0]
	listenerTarget := item.Listener.Target.Normalized()
	port, replacement, firstAlready, reason := httpsRootRoutePlan(m.view.Exposures.Routes, item, mode)
	if reason != "" {
		m.setBanner(reason, true)
		return
	}
	allAlready := firstAlready
	for _, selected := range items[1:] {
		_, _, already, planErr := httpsRootRoutePlan(m.view.Exposures.Routes, selected, mode)
		if planErr != "" {
			m.setBanner(selected.ID+": "+planErr, true)
			return
		}
		allAlready = allAlready && already
	}
	if allAlready {
		m.modal = modalNone
		m.transient = fmt.Sprintf("Already %s HTTPS for %d selected service(s)", mode, len(items))
		m.discardHTTPSRootAction()
		return
	}
	m.actionSession.open(item.ID, listenerTarget, mode)
	m.actionSession.capturePreview(
		routeFingerprint(m.view.Exposures.Routes, &listenerTarget),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, listenerTarget),
		selectionFingerprint(items),
	)
	m.actionSession.confirm = false
	if replacement == nil {
		m.actionSession.routeID = ""
		m.actionSession.routeKey = ""
	} else {
		m.actionSession.routeID = replacement.ID
		m.actionSession.routeKey = replacement.ProviderKey
	}
	m.httpsRootAction = true
	m.httpsRootPort = port
	m.httpsRootReplaceRoute = replacement != nil
	m.httpsRootRouteID = ""
	if replacement != nil {
		m.httpsRootRouteID = replacement.ID
	}
	m.modal = modalConfirm
	m.transient = fmt.Sprintf("Preview: %s HTTPS on port %d", mode, port)
}
