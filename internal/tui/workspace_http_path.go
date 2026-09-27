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
	textinput "github.com/charmbracelet/bubbles/textinput"
)

func (m *workspaceModel) discardHTTPPathAction() {
	m.httpPathAction = false
	m.httpPath = ""
	m.httpPathLocalhostBackend = false
	m.pathInput.Blur()
}

func (m *workspaceModel) httpPathBaseReason(item exposure.ReconciledItem) string {
	if m.configErr != nil {
		return "HTTP path unavailable: configuration is invalid"
	}
	if m.processBusy {
		return "HTTP path unavailable: process termination is in progress"
	}
	if m.refreshState.isPending() {
		return "HTTP path unavailable: refresh is in progress"
	}
	if m.viewErr != nil || !m.view.Listeners.Authoritative || m.view.Listeners.Stale || !m.view.Exposures.Authoritative || m.view.Exposures.Stale {
		return "HTTP path unavailable: listener or provider state is stale or unknown"
	}
	if m.readyErr != nil || !m.hasReadiness {
		return "HTTP path unavailable: Tailscale readiness is stale or unknown"
	}
	if item.Listener == nil {
		return "HTTP path unavailable: select a current local listener"
	}
	switch item.State {
	case exposuredata.ExposureAmbiguous:
		return "HTTP path unavailable: existing route identity is ambiguous"
	case exposuredata.ExposureUnknown, exposuredata.ExposureUnavailable, exposuredata.ExposureUnsupported:
		return "HTTP path unavailable: listener or route identity is not authoritative"
	}
	if item.OperationState == exposuredata.ExposureApplying || item.State == exposuredata.ExposureApplying {
		return "HTTP path unavailable: an operation is Applying for this listener"
	}
	if _, busy := m.activeOps[item.Listener.Target.Normalized().Key()]; busy {
		return "HTTP path unavailable: an operation is Applying for this listener"
	}
	return ""
}

func (m *workspaceModel) httpPathShortcutAvailable(item exposure.ReconciledItem) bool {
	if m.httpPathBaseReason(item) != "" {
		return false
	}
	for _, mode := range []exposuredata.ExposureMode{exposuredata.ExposureServe, exposuredata.ExposureFunnel} {
		status, _, _ := workspace.HTTPPathStatus(m.readiness, mode)
		if status == readiness.ReadinessReady {
			return true
		}
	}
	return false
}

func (m *workspaceModel) httpPathStatusLabel() string {
	item, ok := m.selectedItem()
	if !ok || !m.httpPathShortcutAvailable(item) {
		return "p HTTP path[off]"
	}
	return "p HTTP path[ok]"
}

func supportsLocalhostBackendAlias(target targetmodel.Target) bool {
	target = target.Normalized()
	return target.Address == "::1" || target.Address == "::"
}

func httpPathBackend(target targetmodel.Target, localhostBackendAlias bool) string {
	if localhostBackendAlias {
		return tailscale.HTTPSBackendArgument(target, true)
	}
	return tailscale.HTTPPathBackendArgument(target)
}

func (m *workspaceModel) openHTTPPathInput() {
	items := m.actionItems()
	if len(items) != 1 {
		m.setBanner("Named HTTP paths apply to one exact listener at a time", true)
		return
	}
	item := items[0]
	if reason := m.httpPathBaseReason(item); reason != "" {
		m.setBanner(reason, true)
		return
	}
	listenerTarget := item.Listener.Target.Normalized()
	m.actionSession.open(item.ID, listenerTarget, exposuredata.ExposureServe)
	m.actionSession.capturePreview(
		routeFingerprint(m.view.Exposures.Routes, &listenerTarget),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, listenerTarget),
		selectionFingerprint([]exposure.ReconciledItem{item}),
	)
	m.httpPath, m.httpPathMode, m.httpPathAction, m.httpPathLocalhostBackend = "", exposuredata.ExposureServe, true, false
	m.pathInput.SetValue("")
	m.pathInput.Focus()
	m.modal = modalHTTPPath
	m.transient = "Preview: add named HTTP path"
}

func (m *workspaceModel) submitHTTPPath() {
	item, ok := m.actionAnchorItem()
	if !ok || item.Listener == nil || item.ID != m.actionSession.itemID {
		m.modal = modalNone
		m.discardHTTPPathAction()
		m.setBanner("Selection changed — refresh required", true)
		return
	}
	if reason := m.httpPathBaseReason(item); reason != "" {
		m.modal = modalNone
		m.discardHTTPPathAction()
		m.setBanner(reason, true)
		return
	}
	status, message, remediation := workspace.HTTPPathStatus(m.readiness, m.httpPathMode)
	if status != readiness.ReadinessReady {
		m.setBanner(httpPathReadinessReason(m.httpPathMode, status, message, remediation), true)
		return
	}
	if m.actionSession.previewChanged(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint([]exposure.ReconciledItem{item}),
	) {
		m.modal = modalNone
		m.discardHTTPPathAction()
		m.setBanner("Selection changed — refresh required", true)
		return
	}
	path := strings.TrimSpace(m.pathInput.Value())
	var err error
	if path == "" {
		processName := item.Listener.Process
		if processName == "" {
			processName = item.Listener.Name
		}
		path, err = exposuredata.GeneratedHTTPPath(processName, item.Listener.Target.Port)
	} else {
		path, err = exposuredata.NormalizeHTTPPath(path)
	}
	if err != nil {
		m.setBanner(err.Error(), true)
		return
	}
	if reason := httpPathRouteConflict(m.view.Exposures.Routes, path, m.httpPathMode, item.Listener.Target, m.httpPathLocalhostBackend); reason != "" {
		m.setBanner(reason, true)
		return
	}
	m.httpPath = path
	m.actionSession.mode = m.httpPathMode
	m.actionSession.confirm = false
	m.pathInput.Blur()
	m.modal = modalConfirm
}

func httpPathRouteConflict(routes []exposuredata.ExposureRoute, path string, mode exposuredata.ExposureMode, target targetmodel.Target, localhostBackendAlias bool) string {
	backend := httpPathBackend(target, localhostBackendAlias)
	backendTarget, targetErr := targetmodel.ParseTarget(backend, "tcp")
	for _, route := range routes {
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
		if route.ProviderKey == "" || err != nil {
			return "Cannot add a path: an existing route has no exact provider endpoint identity"
		}
		if selector.Port != 443 {
			continue
		}
		if route.Mode != mode {
			return "Serve and Funnel cannot be mixed on the shared HTTPS hostname and port"
		}
		if route.Kind != exposuredata.RouteKindHTTPPath && route.Kind != exposuredata.RouteKindHTTPSRoot {
			return "The shared HTTPS endpoint already has a non-HTTP handler"
		}
		if route.Path == path {
			if targetErr == nil && route.Mode == mode && route.ID != "" && route.State == exposuredata.ExposureActive && route.Target.Normalized().Key() == backendTarget.Normalized().Key() && tailscale.HTTPPathBackendMatches(route.Backend, backend) {
				continue
			}
			return "The HTTPS path " + path + " is already configured for a different route identity; disable the exact handler before reusing it"
		}
	}
	return ""
}

func httpPathReadinessReason(mode exposuredata.ExposureMode, status readiness.ReadinessStatus, message, remediation string) string {
	reason := fmt.Sprintf("%s HTTP path routes — %s", mode, string(status))
	if message != "" {
		reason += ": " + message
	}
	if remediation != "" {
		reason += ". Next: " + remediation
	}
	return reason
}

func pathInputView(input textinput.Model) string {
	return sanitizeTUIText(input.View())
}
