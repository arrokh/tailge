package workspace

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
)

// ActionAvailability is the workspace mutation gate result. Wait distinguishes
// a refresh-pending action from a permanently unavailable action so the TUI can
// explain the former without labeling it unavailable.
type ActionAvailability struct {
	Disabled bool
	Reason   string
	Wait     bool
}

// ActionContext contains the observations and lifecycle state required to
// decide whether a requested exposure mutation is safe.
type ActionContext struct {
	ConfigError    error
	View           exposure.View
	Readiness      readiness.Readiness
	ViewError      error
	ReadinessError error
	RefreshPending bool
	ProcessBusy    bool
}

// ActionAvailabilityForItems applies the complete fail-closed mutation policy
// to the selected workspace items.
func ActionAvailabilityForItems(items []exposure.ReconciledItem, mode exposuredata.ExposureMode, context ActionContext) ActionAvailability {
	if len(items) == 0 {
		return ActionAvailability{Disabled: true, Reason: "No service is selected"}
	}
	for _, item := range items {
		availability := actionAvailabilityForItem(item, mode, len(items) > 1, context)
		if availability.Disabled {
			availability.Reason = item.ID + ": " + availability.Reason
			return availability
		}
	}
	return ActionAvailability{}
}

func actionAvailabilityForItem(item exposure.ReconciledItem, mode exposuredata.ExposureMode, batch bool, context ActionContext) ActionAvailability {
	unavailable := func(reason string) ActionAvailability {
		return ActionAvailability{Disabled: true, Reason: reason}
	}
	if context.ConfigError != nil {
		return unavailable("Action unavailable: configuration is invalid")
	}
	if item.OperationState == exposuredata.ExposureApplying || item.State == exposuredata.ExposureApplying {
		return unavailable("Action unavailable: an operation is Applying for this target")
	}
	if context.ProcessBusy {
		return unavailable("Action unavailable: process termination is in progress")
	}
	// A target with multiple exact routes is ambiguous for enable/replace. A
	// batch cannot open one route chooser per item, so it is also blocked for
	// disable when batch is true.
	if item.State == exposuredata.ExposureUnknown || item.State == exposuredata.ExposureUnavailable || item.State == exposuredata.ExposureUnsupported || (item.State == exposuredata.ExposureAmbiguous && (mode != exposuredata.ExposureDisabled || len(item.Routes) <= 1 || batch)) {
		return unavailable("Action unavailable: selected state is not authoritative or exact")
	}
	if mode == exposuredata.ExposureDisabled {
		if len(item.Routes) == 0 {
			if item.State == exposuredata.ExposureState("disabled") {
				return ActionAvailability{}
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
		if !SameStateForItem(context.View, item, mode) {
			if context.RefreshPending {
				return ActionAvailability{Disabled: true, Reason: "Action unavailable: refresh is in progress", Wait: true}
			}
			if ObservationsUnavailableForMutation(context) {
				return unavailable("Action unavailable: listener/exposure state is stale")
			}
		}
		return ActionAvailability{}
	}
	if !SameStateForItem(context.View, item, mode) {
		if context.RefreshPending {
			return ActionAvailability{Disabled: true, Reason: "Action unavailable: refresh is in progress", Wait: true}
		}
		if ObservationsUnavailableForMutation(context) {
			return unavailable("Action unavailable: listener/exposure state is stale")
		}
		if context.ReadinessError != nil {
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
	if status := ModeStatus(context.Readiness, mode); status != readiness.ReadinessReady && !SameStateForItem(context.View, item, mode) {
		return unavailable(ReadinessActionReason(context.Readiness, mode))
	}
	return ActionAvailability{}
}

// ObservationsUnavailableForMutation reports whether route/listener state is
// incomplete enough to block a mutation.
func ObservationsUnavailableForMutation(context ActionContext) bool {
	return context.ViewError != nil || !context.View.Listeners.Authoritative || context.View.Listeners.Stale || !context.View.Exposures.Authoritative || context.View.Exposures.Stale
}

// SameStateForItem reports whether an operation would be a verified no-op.
func SameStateForItem(view exposure.View, item exposure.ReconciledItem, mode exposuredata.ExposureMode) bool {
	if !view.Listeners.Authoritative || view.Listeners.Stale || !view.Exposures.Authoritative || view.Exposures.Stale || item.State == exposuredata.ExposureUnknown || item.State == exposuredata.ExposureUnavailable || item.State == exposuredata.ExposureAmbiguous {
		return false
	}
	if mode == exposuredata.ExposureDisabled {
		return len(item.Routes) == 0 && item.State == exposuredata.ExposureState("disabled")
	}
	if len(item.Routes) != 1 {
		return false
	}
	route := item.Routes[0]
	return route.Mode == mode && item.State == exposuredata.ExposureActive && !requiresBackendRepair(route)
}

func requiresBackendRepair(route exposuredata.ExposureRoute) bool {
	// Wildcard addresses are valid listener bind addresses but not reachable
	// proxy destinations. Re-apply so the provider can normalize the backend to
	// loopback instead of reporting a verified no-op that can produce a 502.
	return route.Target.Address == "0.0.0.0" || route.Target.Address == "::"
}

// ModeStatus returns the observed readiness for one exposure mode.
func ModeStatus(report readiness.Readiness, wanted exposuredata.ExposureMode) readiness.ReadinessStatus {
	for _, mode := range report.Modes {
		if mode.Mode == wanted {
			return mode.Status
		}
	}
	return readiness.ReadinessUnknown
}

// ReadinessActionReason renders the first actionable readiness check.
func ReadinessActionReason(report readiness.Readiness, wanted exposuredata.ExposureMode) string {
	for _, mode := range report.Modes {
		if mode.Mode != wanted {
			continue
		}
		for _, check := range mode.Checks {
			if check.Status == readiness.ReadinessReady {
				continue
			}
			reason := fmt.Sprintf("%s — %s: %s", wanted, mode.Status, check.Message)
			if check.Remediation != "" {
				reason += " Next: " + check.Remediation
			}
			return reason
		}
		return fmt.Sprintf("%s — %s", wanted, mode.Status)
	}
	return fmt.Sprintf("%s — %s", wanted, readiness.ReadinessUnknown)
}

// AlreadyMessage renders a verified no-op status for the requested mode.
func AlreadyMessage(mode exposuredata.ExposureMode) string {
	if mode == exposuredata.ExposureDisabled {
		return "Already disabled"
	}
	return "Already active: " + string(mode)
}

// PreviewRoute returns the route selected by the current action session.
func PreviewRoute(item exposure.ReconciledItem, routeKey string) *exposuredata.ExposureRoute {
	if routeKey != "" {
		for i := range item.Routes {
			if item.Routes[i].ProviderKey == routeKey {
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

// ExternalPreview reports whether the selected route requires explicit
// external-ownership confirmation.
func ExternalPreview(item exposure.ReconciledItem, routeKey string) bool {
	if routeKey != "" {
		for _, route := range item.Routes {
			if route.ProviderKey == routeKey {
				return route.Ownership != exposuredata.OwnershipManaged
			}
		}
		return true
	}
	return len(item.Routes) == 1 && item.Routes[0].Ownership != exposuredata.OwnershipManaged
}

// ObservedRouteURL returns only an explicitly observed HTTP(S) route URL.
func ObservedRouteURL(route exposuredata.ExposureRoute) (string, bool) {
	url := strings.TrimSpace(route.URL)
	if strings.HasPrefix(strings.ToLower(url), "http://") || strings.HasPrefix(strings.ToLower(url), "https://") {
		return url, true
	}
	return "", false
}

// RouteTransport classifies a provider selector without guessing a transport.
func RouteTransport(route exposuredata.ExposureRoute) string {
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

// RawTCPRouteSelector returns an exact observed TCP selector, if present.
func RawTCPRouteSelector(item exposure.ReconciledItem) string {
	for _, route := range item.Routes {
		if RouteTransport(route) == "tcp" {
			return route.ProviderKey
		}
	}
	return ""
}

// ServeTCPBrowserURL resolves a UI-only HTTP preview for an observed Serve TCP
// route. It never changes route identity or mutation selectors.
func ServeTCPBrowserURL(status tailscale.Status, route exposuredata.ExposureRoute) (string, error) {
	selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
	if err != nil || route.Mode != exposuredata.ExposureServe || selector.Transport != "tcp" {
		return "", fmt.Errorf("route is not an exact serve TCP listener")
	}
	host := strings.TrimSuffix(strings.TrimSpace(status.Self.DNSName), ".")
	if host == "" || strings.ContainsAny(host, "/?#\\: \t\r\n") {
		return "", fmt.Errorf("tailscale did not report a safe DNS name")
	}
	endpoint := net.JoinHostPort(host, strconv.Itoa(selector.Port))
	return "http://" + endpoint + "/", nil
}

// ProcessFingerprint identifies the selected process and its listener without
// relying on a PID alone.
func ProcessFingerprint(listener discovery.Listener) string {
	return strings.Join([]string{
		strconv.Itoa(listener.PID),
		listener.Target.Normalized().Key(),
		listener.Process,
		listener.ProcessStart,
		listener.CommandLine,
	}, "\x00")
}

// ProcessGroupFingerprint identifies duplicate listener rows belonging to the
// same process group.
func ProcessGroupFingerprint(listener discovery.Listener) string {
	return strings.Join([]string{
		strconv.Itoa(listener.PID),
		listener.Process,
		listener.CommandLine,
	}, "\x00")
}

// OperationStateForError maps an uncertain operation to a visible workspace
// state without claiming that provider state is known.
func OperationStateForError(err error) exposuredata.ExposureState {
	switch fault.AsAppError(err).Code {
	case fault.ErrCancelled:
		return exposuredata.ExposureCancelled
	case fault.ErrVerification, fault.ErrUnknown, fault.ErrTimeout:
		return exposuredata.ExposureUnverified
	default:
		return exposuredata.ExposureFailed
	}
}
