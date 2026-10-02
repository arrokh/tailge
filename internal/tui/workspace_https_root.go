package tui

import (
	"fmt"
	"strings"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
)

func (m *workspaceModel) discardHTTPSRootAction() {
	m.httpsRootAction = false
	m.httpsRootPort = 0
	m.httpsRootRouteID = ""
	m.httpsRootReplaceRawTCP = false
	m.httpsRootLocalhostBackend = false
}

func (m *workspaceModel) httpsRootBaseReason(item exposure.ReconciledItem) string {
	if m.configErr != nil {
		return "HTTPS root unavailable: configuration is invalid"
	}
	if m.processBusy {
		return "HTTPS root unavailable: process termination is in progress"
	}
	if m.refreshState.isPending() {
		return "HTTPS root unavailable: refresh is in progress"
	}
	if m.viewErr != nil || !m.view.Listeners.Authoritative || m.view.Listeners.Stale || !m.view.Exposures.Authoritative || m.view.Exposures.Stale {
		return "HTTPS root unavailable: listener or provider state is stale or unknown"
	}
	if m.readyErr != nil || !m.hasReadiness {
		return "HTTPS root unavailable: Tailscale readiness is stale or unknown"
	}
	if item.Listener == nil {
		return "HTTPS root unavailable: select a current local listener"
	}
	switch item.State {
	case exposuredata.ExposureAmbiguous:
		return "HTTPS root unavailable: existing route identity is ambiguous"
	case exposuredata.ExposureUnknown, exposuredata.ExposureUnavailable, exposuredata.ExposureUnsupported:
		return "HTTPS root unavailable: listener or route identity is not authoritative"
	}
	if item.OperationState == exposuredata.ExposureApplying || item.State == exposuredata.ExposureApplying {
		return "HTTPS root unavailable: an operation is Applying for this listener"
	}
	if _, busy := m.activeOps[item.Listener.Target.Normalized().Key()]; busy {
		return "HTTPS root unavailable: an operation is Applying for this listener"
	}
	return ""
}

func (m *workspaceModel) httpsRootShortcutAvailable(item exposure.ReconciledItem) bool {
	if len(m.actionItems()) != 1 || m.httpsRootBaseReason(item) != "" {
		return false
	}
	status, _, _ := workspace.HTTPPathStatus(m.readiness, exposuredata.ExposureServe)
	if status != readiness.ReadinessReady {
		return false
	}
	_, alreadyConfigured, reason := httpsRootRouteConflict(m.view.Exposures.Routes, item.Listener.Target, item.Listener.Target.Port, false)
	return !alreadyConfigured && reason == ""
}

func (m *workspaceModel) httpsRootStatusLabel() string {
	item, ok := m.selectedItem()
	if !ok || !m.httpsRootShortcutAvailable(item) {
		return "b HTTPS root[off]"
	}
	return "b HTTPS root[ok]"
}

func httpsRootBackend(target targetmodel.Target, localhostBackendAlias bool) string {
	if localhostBackendAlias {
		return tailscale.HTTPSBackendArgument(target, true)
	}
	return tailscale.HTTPPathBackendArgument(target)
}

func httpsRootRouteConflict(routes []exposuredata.ExposureRoute, target targetmodel.Target, httpsPort int, localhostBackendAlias bool) (*exposuredata.ExposureRoute, bool, string) {
	target = target.Normalized()
	backend := httpsRootBackend(target, localhostBackendAlias)
	if _, targetErr := targetmodel.ParseTarget(backend, "tcp"); targetErr != nil {
		return nil, false, "HTTPS root unavailable: local backend identity is invalid"
	}
	var rawTCP *exposuredata.ExposureRoute
	endpointCount := 0
	for _, route := range routes {
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if route.ProviderKey == "" || err != nil {
			return nil, false, "Cannot configure HTTPS root: an existing route has no exact provider endpoint identity"
		}
		if selector.Port != httpsPort {
			continue
		}
		endpointCount++
		if route.ID == "" || route.State != exposuredata.ExposureActive {
			return nil, false, "Cannot configure HTTPS root: an endpoint handler has incomplete route identity"
		}
		if route.Mode != exposuredata.ExposureServe {
			return nil, false, "Serve and Funnel cannot be mixed on the same HTTPS port"
		}
		if selector.Transport == "tcp" || route.Kind == exposuredata.RouteKindRawTCP {
			if selector.Transport != "tcp" || route.Kind != exposuredata.RouteKindRawTCP || route.Service != "" || route.Path != "" {
				return nil, false, "Cannot convert the raw-TCP endpoint: exact route identity is incomplete"
			}
			if route.Target.Normalized().Key() != target.Key() || !tailscale.RawTCPBackendMatchesTarget(target, route.Backend) {
				return nil, false, "The raw-TCP endpoint does not match the exact local listener backend"
			}
			if rawTCP != nil {
				return nil, false, "Multiple raw-TCP routes occupy this port; resolve the exact identity first"
			}
			routeCopy := route
			rawTCP = &routeCopy
			continue
		}
		if selector.Transport != "https" || (route.Kind != exposuredata.RouteKindHTTPPath && route.Kind != exposuredata.RouteKindHTTPSRoot) {
			return nil, false, "The selected HTTPS port already has a non-HTTP handler"
		}
		if (route.Kind == exposuredata.RouteKindHTTPPath && route.Path == "") || (route.Kind == exposuredata.RouteKindHTTPSRoot && route.Path != "/") {
			return nil, false, "Cannot configure HTTPS root: an existing handler has incomplete path identity"
		}
		if route.Kind == exposuredata.RouteKindHTTPPath {
			path, pathErr := exposuredata.NormalizeHTTPPath(strings.TrimPrefix(route.Path, "/"))
			if pathErr != nil || path != route.Path {
				return nil, false, "Cannot configure HTTPS root: an existing named path has incomplete identity"
			}
		}
		if !httpsHandlerBackendMatchesTarget(route) {
			return nil, false, "Cannot configure HTTPS root: an existing handler has incomplete backend identity"
		}
		if route.Path != "/" {
			continue
		}
		if route.Kind != exposuredata.RouteKindHTTPSRoot {
			return nil, false, "The HTTPS root path is already occupied by a different handler"
		}
		if httpsRootBackendMatches(route, target, backend) || (supportsLocalhostBackendAlias(target) && httpsRootBackendMatches(route, target, tailscale.HTTPSBackendArgument(target, true))) {
			return nil, true, ""
		}
		return nil, false, "The HTTPS root is already configured for a different backend identity"
	}
	if rawTCP != nil && endpointCount != 1 {
		return nil, false, "The raw-TCP route shares its port with other handlers; resolve the exact endpoint first"
	}
	return rawTCP, false, ""
}

func httpsHandlerBackendMatchesTarget(route exposuredata.ExposureRoute) bool {
	target := route.Target.Normalized()
	if tailscale.HTTPPathBackendMatches(route.Backend, tailscale.HTTPPathBackendArgument(target)) {
		return true
	}
	return supportsLocalhostBackendAlias(target) && tailscale.HTTPPathBackendMatches(route.Backend, tailscale.HTTPSBackendArgument(target, true))
}

func httpsRootBackendMatches(route exposuredata.ExposureRoute, target targetmodel.Target, backend string) bool {
	parsedBackend, err := targetmodel.ParseTarget(backend, "tcp")
	return err == nil && parsedBackend.Normalized().Port == target.Port &&
		route.Target.Normalized().Key() == parsedBackend.Normalized().Key() &&
		tailscale.HTTPPathBackendMatches(route.Backend, backend)
}

func (m *workspaceModel) openHTTPSRootPreview() {
	items := m.actionItems()
	if len(items) != 1 {
		m.setBanner("Private HTTPS roots apply to one exact listener at a time", true)
		return
	}
	item := items[0]
	if reason := m.httpsRootBaseReason(item); reason != "" {
		m.setBanner(reason, true)
		return
	}
	status, message, remediation := workspace.HTTPPathStatus(m.readiness, exposuredata.ExposureServe)
	if status != readiness.ReadinessReady {
		m.setBanner(httpsRootReadinessReason(status, message, remediation), true)
		return
	}
	listenerTarget := item.Listener.Target.Normalized()
	rawTCP, already, reason := httpsRootRouteConflict(m.view.Exposures.Routes, listenerTarget, listenerTarget.Port, false)
	if reason != "" {
		m.setBanner(reason, true)
		return
	}
	if already {
		m.transient = fmt.Sprintf("Private HTTPS root is already configured on port %d", listenerTarget.Port)
		return
	}
	m.actionSession.open(item.ID, listenerTarget, exposuredata.ExposureServe)
	m.actionSession.capturePreview(
		routeFingerprint(m.view.Exposures.Routes, &listenerTarget),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, listenerTarget),
		selectionFingerprint([]exposure.ReconciledItem{item}),
	)
	m.actionSession.confirm = false
	m.actionSession.routeID = ""
	m.actionSession.routeKey = ""
	m.httpsRootAction = true
	m.httpsRootPort = listenerTarget.Port
	m.httpsRootReplaceRawTCP = rawTCP != nil
	m.httpsRootRouteID = ""
	if rawTCP != nil {
		m.httpsRootRouteID = rawTCP.ID
		m.actionSession.routeID = rawTCP.ID
		m.actionSession.routeKey = rawTCP.ProviderKey
	}
	m.httpsRootLocalhostBackend = false
	m.modal = modalConfirm
	m.transient = fmt.Sprintf("Preview: private HTTPS root on port %d", listenerTarget.Port)
}

func httpsRootReadinessReason(status readiness.ReadinessStatus, message, remediation string) string {
	reason := "Private HTTPS root routes — " + string(status)
	if message != "" {
		reason += ": " + message
	}
	if remediation != "" {
		reason += ". Next: " + remediation
	}
	return reason
}

func supportsLocalhostBackendAlias(target targetmodel.Target) bool {
	target = target.Normalized()
	return target.Address == "::1" || target.Address == "::"
}
