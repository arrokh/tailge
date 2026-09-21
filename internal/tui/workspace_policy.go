package tui

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

// workspace policy keeps mutation availability, process identity, and
// operation lifecycle decisions separate from Bubble Tea rendering.

func (m *workspaceModel) actionAvailabilityForItems(mode model.ExposureMode) exposureActionAvailability {
	items := m.actionItems()
	if len(items) == 0 {
		return exposureActionAvailability{disabled: true, reason: "No service is selected"}
	}
	for _, item := range items {
		availability := m.actionAvailabilityForItem(item, mode, len(items) > 1)
		if availability.disabled {
			availability.reason = item.ID + ": " + availability.reason
			return availability
		}
	}
	return exposureActionAvailability{}
}

func (m *workspaceModel) actionAvailabilityForItem(item exposure.ReconciledItem, mode model.ExposureMode, batch bool) exposureActionAvailability {
	unavailable := func(reason string) exposureActionAvailability {
		return exposureActionAvailability{disabled: true, reason: reason}
	}
	if m.configErr != nil {
		return unavailable("Action unavailable: configuration is invalid")
	}
	if item.OperationState == model.ExposureApplying || item.State == model.ExposureApplying {
		return unavailable("Action unavailable: an operation is Applying for this target")
	}
	if m.processBusy {
		return unavailable("Action unavailable: process termination is in progress")
	}
	// A target with multiple exact routes is ambiguous for enable/replace. A
	// batch cannot open one route chooser per item, so it is also blocked for
	// disable when batch is true.
	if item.State == model.ExposureUnknown || item.State == model.ExposureUnavailable || item.State == model.ExposureUnsupported || (item.State == model.ExposureAmbiguous && (mode != model.ExposureDisabled || len(item.Routes) <= 1 || batch)) {
		return unavailable("Action unavailable: selected state is not authoritative or exact")
	}
	if mode == model.ExposureDisabled {
		if len(item.Routes) == 0 {
			if item.State == model.ExposureState("disabled") {
				return exposureActionAvailability{}
			}
			return unavailable("Disable unavailable: no exact route is observed")
		}
		if batch && len(item.Routes) != 1 {
			return unavailable("Disable unavailable: batch requires one exact route per target")
		}
		for _, route := range item.Routes {
			if route.ProviderKey == "" {
				return unavailable("Disable unavailable: exact route selector is missing")
			}
		}
		if !m.sameStateForItem(item, mode) {
			if m.refreshState.isPending() {
				return exposureActionAvailability{disabled: true, reason: "Action unavailable: refresh is in progress", wait: true}
			}
			if m.observationsUnavailableForMutation() {
				return unavailable("Action unavailable: listener/exposure state is stale")
			}
		}
		return exposureActionAvailability{}
	}
	if !m.sameStateForItem(item, mode) {
		if m.refreshState.isPending() {
			return exposureActionAvailability{disabled: true, reason: "Action unavailable: refresh is in progress", wait: true}
		}
		if m.observationsUnavailableForMutation() {
			return unavailable("Action unavailable: listener/exposure state is stale")
		}
		if m.readyErr != nil {
			return unavailable("Action unavailable: readiness state is stale")
		}
	}
	if item.Listener == nil {
		return unavailable("Enable unavailable: no current exact local listener")
	}
	if len(item.Routes) > 1 {
		return unavailable("Action unavailable: existing routes are ambiguous")
	}
	if len(item.Routes) == 1 && item.Routes[0].ProviderKey == "" && item.Routes[0].Mode != mode {
		return unavailable("Action unavailable: existing route has no exact provider selector")
	}
	if status := modeStatus(m.readiness, mode); status != model.ReadinessReady && !m.sameStateForItem(item, mode) {
		return unavailable(readinessActionReason(m.readiness, mode))
	}
	return exposureActionAvailability{}
}

func (m *workspaceModel) observationsUnavailableForMutation() bool {
	return m.viewErr != nil || !m.view.Listeners.Authoritative || m.view.Listeners.Stale || !m.view.Exposures.Authoritative || m.view.Exposures.Stale
}

func (m *workspaceModel) sameStateForItem(item exposure.ReconciledItem, mode model.ExposureMode) bool {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || !m.view.Exposures.Authoritative || m.view.Exposures.Stale || item.State == model.ExposureUnknown || item.State == model.ExposureUnavailable || item.State == model.ExposureAmbiguous {
		return false
	}
	if mode == model.ExposureDisabled {
		return len(item.Routes) == 0 && item.State == model.ExposureState("disabled")
	}
	if len(item.Routes) != 1 {
		return false
	}
	route := item.Routes[0]
	return route.Mode == mode && item.State == model.ExposureActive && !requiresBackendRepair(route)
}

func requiresBackendRepair(route model.ExposureRoute) bool {
	// Wildcard addresses are valid listener bind addresses but not reachable
	// proxy destinations. Older Tailge versions could persist them as the
	// Tailscale backend target, producing a 502; re-apply instead of reporting
	// a verified no-op so the provider can normalize the backend to loopback.
	return route.Target.Address == "0.0.0.0" || route.Target.Address == "::"
}

func alreadyMessage(mode model.ExposureMode) string {
	if mode == model.ExposureDisabled {
		return "Already disabled"
	}
	return "Already active: " + string(mode)
}

func (m *workspaceModel) previewRoute(item exposure.ReconciledItem) *model.ExposureRoute {
	if m.actionSession.routeKey != "" {
		for i := range item.Routes {
			if item.Routes[i].ProviderKey == m.actionSession.routeKey {
				return &item.Routes[i]
			}
		}
		return nil
	}
	if len(item.Routes) == 1 {
		return &item.Routes[0]
	}
	return nil
}

func (m *workspaceModel) externalPreview() bool {
	item, ok := m.actionAnchorItem()
	if !ok {
		return false
	}
	return m.externalPreviewFor(item, m.actionSession.routeKey)
}

func (m *workspaceModel) externalPreviewFor(item exposure.ReconciledItem, routeKey string) bool {
	if routeKey != "" {
		for _, route := range item.Routes {
			if route.ProviderKey == routeKey {
				return route.Ownership != model.OwnershipManaged
			}
		}
		return true
	}
	return len(item.Routes) == 1 && item.Routes[0].Ownership != model.OwnershipManaged
}

func observedRouteURL(route model.ExposureRoute) (string, bool) {
	url := strings.TrimSpace(route.URL)
	if strings.HasPrefix(strings.ToLower(url), "http://") || strings.HasPrefix(strings.ToLower(url), "https://") {
		return url, true
	}
	return "", false
}

func routeTransport(route model.ExposureRoute) string {
	_, selector, ok := strings.Cut(route.ProviderKey, ":")
	if !ok {
		return ""
	}
	switch {
	case strings.HasPrefix(selector, "tcp="):
		return "tcp"
	case strings.HasPrefix(selector, "https="):
		return "https"
	default:
		return ""
	}
}

func rawTCPRouteSelector(item exposure.ReconciledItem) string {
	for _, route := range item.Routes {
		if routeTransport(route) == "tcp" {
			return route.ProviderKey
		}
	}
	return ""
}

func serveTCPBrowserURL(status tailscale.Status, route model.ExposureRoute) (string, error) {
	selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
	if err != nil || route.Mode != model.ExposureServe || selector.Transport != "tcp" {
		return "", fmt.Errorf("route is not an exact Serve TCP listener")
	}
	host := strings.TrimSuffix(strings.TrimSpace(status.Self.DNSName), ".")
	if host == "" || strings.ContainsAny(host, "/?#\\: \t\r\n") {
		return "", fmt.Errorf("Tailscale did not report a safe DNS name")
	}
	endpoint := net.JoinHostPort(host, strconv.Itoa(selector.Port))
	return "http://" + endpoint + "/", nil
}

func processFingerprint(listener model.Listener) string {
	return strings.Join([]string{
		strconv.Itoa(listener.PID),
		listener.Target.Normalized().Key(),
		listener.Process,
		listener.ProcessStart,
		listener.CommandLine,
	}, "\x00")
}

func processGroupFingerprint(listener model.Listener) string {
	return strings.Join([]string{
		strconv.Itoa(listener.PID),
		listener.Process,
		listener.CommandLine,
	}, "\x00")
}

func (m *workspaceModel) collectProcessTargets() ([]processTarget, string) {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil {
		return nil, "listener state is stale"
	}
	items := m.actionItems()
	seenProcesses := map[string]bool{}
	targets := make([]processTarget, 0, len(items))
	for _, item := range items {
		if item.Listener == nil {
			if len(item.Routes) > 0 {
				return nil, item.ID + ": exposure route is inactive; no current local process exists (use d to disable the route)"
			}
			return nil, item.ID + ": no current local listener exists"
		}
		listener := *item.Listener
		if _, busy := m.activeOps[listener.Target.Key()]; busy {
			return nil, "an exposure operation is Applying for a selected listener"
		}
		if listener.PID <= 1 || listener.PID == os.Getpid() {
			return nil, "a selected process is protected"
		}
		if strings.TrimSpace(listener.Process) == "" || strings.TrimSpace(listener.ProcessStart) == "" {
			return nil, "a selected process has incomplete identity"
		}
		group := processGroupFingerprint(listener)
		if seenProcesses[group] {
			continue
		}
		seenProcesses[group] = true
		targets = append(targets, processTarget{itemID: item.ID, listener: listener, fingerprint: processFingerprint(listener)})
	}
	return targets, ""
}

func (m *workspaceModel) revalidateProcessTarget(target processTarget) (model.Listener, error) {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil {
		return model.Listener{}, fmt.Errorf("listener state is stale")
	}
	for _, item := range m.items() {
		if item.ID != target.itemID || item.Listener == nil {
			continue
		}
		listener := *item.Listener
		if processFingerprint(listener) != target.fingerprint {
			return model.Listener{}, fmt.Errorf("listener or process identity changed")
		}
		if _, busy := m.activeOps[listener.Target.Key()]; busy {
			return model.Listener{}, fmt.Errorf("an exposure operation is Applying")
		}
		if listener.PID <= 1 || listener.PID == os.Getpid() {
			return model.Listener{}, fmt.Errorf("process is protected")
		}
		if strings.TrimSpace(listener.Process) == "" || strings.TrimSpace(listener.ProcessStart) == "" {
			return model.Listener{}, fmt.Errorf("process identity is incomplete")
		}
		return listener, nil
	}
	return model.Listener{}, fmt.Errorf("selected listener is no longer available")
}

func (m *workspaceModel) hasPendingOperations() bool {
	return len(m.activeOps) > 0 || m.processBusy
}

func (m *workspaceModel) markApplying(key string) {
	for i := range m.view.Items {
		itemKey, ok := itemTarget(m.view.Items[i])
		if ok && itemKey.Key() == key {
			m.view.Items[i].OperationState = model.ExposureApplying
			m.view.Items[i].State = model.ExposureApplying
		}
	}
}

func (m *workspaceModel) applyLocalOperationResult(message operationDoneMsg) {
	for i := range m.view.Items {
		key, ok := itemTarget(m.view.Items[i])
		if !ok || key.Key() != message.targetKey {
			continue
		}
		if message.err != nil {
			m.view.Items[i].OperationState = operationStateForError(message.err)
		} else if message.receipt.Verified {
			m.view.Items[i].OperationState = model.ExposureSucceeded
		} else {
			m.view.Items[i].OperationState = model.ExposureUnverified
		}
		copyReceipt := message.receipt
		m.view.Items[i].LastOperation = &copyReceipt
	}
}

func (m *workspaceModel) markOperationUnverified(targetKey string) {
	for i := range m.view.Items {
		target, ok := itemTarget(m.view.Items[i])
		if ok && target.Key() == targetKey {
			m.view.Items[i].OperationState = model.ExposureUnverified
		}
	}
}

func (m *workspaceModel) markActiveOperationsUnverified() {
	for targetKey := range m.activeOps {
		m.markOperationUnverified(targetKey)
	}
}

func operationStateForError(err error) model.ExposureState {
	switch model.AsAppError(err).Code {
	case model.ErrCancelled:
		return model.ExposureCancelled
	case model.ErrVerification, model.ErrUnknown, model.ErrTimeout:
		return model.ExposureUnverified
	default:
		return model.ExposureFailed
	}
}
