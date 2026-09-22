package exposure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

const (
	defaultOperationTimeout = 15 * time.Second
	maxOperationTimeout     = 10 * time.Minute
)

type ReconciledItem struct {
	ID             string                  `json:"id"`
	Listener       *model.Listener         `json:"listener,omitempty"`
	Routes         []model.ExposureRoute   `json:"routes,omitempty"`
	State          model.ExposureState     `json:"state"`
	Mode           model.ExposureMode      `json:"mode"`
	DesiredMode    model.ExposureMode      `json:"desired_mode,omitempty"`
	OperationState model.ExposureState     `json:"operation_state,omitempty"`
	LastOperation  *model.OperationReceipt `json:"last_operation,omitempty"`
	Warning        string                  `json:"warning,omitempty"`
	Recommendation string                  `json:"recommendation,omitempty"`
}

type View struct {
	At        time.Time              `json:"at"`
	Items     []ReconciledItem       `json:"items"`
	Listeners model.ListenerSnapshot `json:"listeners"`
	Exposures model.ExposureSnapshot `json:"exposures"`
	Warnings  []string               `json:"warnings,omitempty"`
	Events    []model.OperationEvent `json:"events,omitempty"`
}

func Reconcile(listeners model.ListenerSnapshot, exposures model.ExposureSnapshot, managed map[string]bool) View {
	if listeners.Error != nil {
		listeners.Authoritative = false
	}
	if exposures.Error != nil {
		exposures.Authoritative = false
	}
	warnings := append([]string{}, listeners.Warnings...)
	warnings = append(warnings, exposures.Warnings...)
	if listeners.Error != nil {
		warnings = append(warnings, "listener source error: "+listeners.Error.Message)
	}
	if exposures.Error != nil {
		warnings = append(warnings, "exposure source error: "+exposures.Error.Message)
	}
	view := View{At: maxTime(listeners.At, exposures.At), Listeners: listeners, Exposures: exposures, Warnings: warnings}
	matched := make(map[string][]int)
	for i := range listeners.Listeners {
		listener := listeners.Listeners[i]
		item := ReconciledItem{ID: listener.ID, Listener: &listener, State: model.ExposureState("disabled"), Mode: model.ExposureDisabled}
		if listener.Scope != model.ScopeLoopback {
			item.Warning = appendWarning(item.Warning, "listener may be reachable from the local network")
		}
		if sensitivePort(listener.Target.Port) {
			item.Warning = appendWarning(item.Warning, "port may host a sensitive service; inspect before exposing it")
		}
		for routeIndex := range exposures.Routes {
			route := exposures.Routes[routeIndex]
			if Matches(route.Target, listener.Target) {
				matched[route.ID] = append(matched[route.ID], i)
				route = applyOwnership(route, managed)
				if !exposures.Authoritative {
					route.State = model.ExposureUnknown
				}
				item.Routes = append(item.Routes, route)
			}
		}
		for _, route := range item.Routes {
			if route.Mode == model.ExposureFunnel {
				item.Warning = appendWarning(item.Warning, "WARNING: Funnel makes this service reachable from the public internet")
			}
		}
		if !exposures.Authoritative || !listeners.Authoritative {
			item.State = model.ExposureUnknown
			item.Warning = appendWarning(item.Warning, "listener or exposure state is incomplete; no route change is safe")
		} else if len(item.Routes) > 1 {
			item.State = model.ExposureAmbiguous
			item.Warning = appendWarning(item.Warning, "multiple exposure routes match this listener; choose an exact route before changing it")
		} else if len(item.Routes) == 1 {
			item.Mode = item.Routes[0].Mode
			item.State = item.Routes[0].State
		} else {
			item.State = model.ExposureState("disabled")
		}
		view.Items = append(view.Items, item)
	}
	for _, matches := range matched {
		if len(matches) > 1 {
			for _, listenerIndex := range matches {
				view.Items[listenerIndex].State = model.ExposureAmbiguous
				view.Items[listenerIndex].Warning = appendWarning(view.Items[listenerIndex].Warning, "one exposure route matches multiple local listeners; choose an address-specific listener before changing it")
			}
		}
	}
	for _, original := range exposures.Routes {
		route := applyOwnership(original, managed)
		matches := matched[route.ID]
		if len(matches) == 1 {
			continue
		}
		item := ReconciledItem{ID: route.ID, Routes: []model.ExposureRoute{route}, Mode: route.Mode, State: route.State}
		if route.Mode == model.ExposureFunnel {
			item.Warning = "WARNING: Funnel route is reachable from the public internet"
		}
		if !exposures.Authoritative {
			item.State = model.ExposureUnknown
			item.Warning = appendWarning(item.Warning, "exposure state could not be determined; this route is not safe to classify or mutate")
		} else if len(matches) > 1 {
			item.State = model.ExposureAmbiguous
			item.Warning = appendWarning(item.Warning, "multiple local listeners match this configured route; select an exact listener before changing it")
		} else if !listeners.Authoritative {
			item.State = model.ExposureUnknown
			item.Warning = appendWarning(item.Warning, "listener presence could not be determined; this route is not classified as inactive")
		} else {
			item.State = model.ExposureInactive
			item.Recommendation = model.InactiveRecommendation(route.Target)
			route.State = model.ExposureInactive
			item.Routes[0] = route
		}
		view.Items = append(view.Items, item)
	}
	sort.SliceStable(view.Items, func(i, j int) bool {
		left, right := view.Items[i], view.Items[j]
		lp, rp := 0, 0
		if left.Listener != nil {
			lp = left.Listener.Target.Port
		} else if len(left.Routes) > 0 {
			lp = left.Routes[0].Target.Port
		}
		if right.Listener != nil {
			rp = right.Listener.Target.Port
		} else if len(right.Routes) > 0 {
			rp = right.Routes[0].Target.Port
		}
		if lp != rp {
			return lp < rp
		}
		return left.ID < right.ID
	})
	return view
}

func appendWarning(existing, addition string) string {
	if existing == "" {
		return addition
	}
	if strings.Contains(existing, addition) {
		return existing
	}
	return existing + "; " + addition
}

func sensitivePort(port int) bool {
	switch port {
	case 22, 3306, 5432, 6379, 9200, 27017:
		return true
	default:
		return false
	}
}

func applyOwnership(route model.ExposureRoute, managed map[string]bool) model.ExposureRoute {
	if managed != nil && (managed[route.ID] || (route.ProviderKey != "" && managed[route.ProviderKey])) {
		route.Ownership = model.OwnershipManaged
	}
	return route
}

func Matches(route, listener model.Target) bool {
	route, listener = route.Normalized(), listener.Normalized()
	if route.Protocol != listener.Protocol || route.Port != listener.Port {
		return false
	}
	if route.Address == listener.Address {
		return true
	}
	// A route normally targets loopback while a process may listen on the
	// wildcard address. This is a valid correlation but callers must retain the
	// listener's wildcard/network-reachable warning.
	if isLoopback(route.Address) && isLoopback(listener.Address) {
		// A provider hostname such as localhost may resolve to either loopback
		// family. Treat it as a logical loopback match; Reconcile still marks
		// multiple matching listeners ambiguous instead of guessing.
		return true
	}
	if isLoopback(route.Address) && isWildcard(listener.Address) {
		return true
	}
	if isWildcard(route.Address) && isWildcard(listener.Address) {
		return true
	}
	return false
}

func isLoopback(address string) bool {
	return model.ScopeForAddress(address) == model.ScopeLoopback
}
func isWildcard(address string) bool {
	a := model.NormalizeAddress(address)
	return a == "0.0.0.0" || a == "::"
}

func RouteIDsHash(routes []model.ExposureRoute, target model.Target) string {
	ids := make([]string, 0)
	for _, route := range routes {
		if Matches(route.Target, target) {
			ids = append(ids, route.ID)
		}
	}
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(h[:])
}

func RouteIDs(routes []model.ExposureRoute, target model.Target) []string {
	ids := make([]string, 0)
	for _, route := range routes {
		if Matches(route.Target, target) {
			ids = append(ids, route.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// RouteIdentityHash fingerprints the complete provider route identity for one
// target. It is stronger than RouteIDsHash, which is retained for provider
// preconditions but cannot detect a selector/backend change under the same ID.
func RouteIdentityHash(routes []model.ExposureRoute, target model.Target) string {
	matched := make([]model.ExposureRoute, 0)
	for _, route := range routes {
		if Matches(route.Target, target) {
			matched = append(matched, route)
		}
	}
	return tailscale.RoutesHash(matched)
}

type Provider interface {
	Capabilities(context.Context) (tailscale.Capabilities, error)
	List(context.Context) (model.ExposureSnapshot, error)
	Set(context.Context, tailscale.ExposureChange) (model.OperationReceipt, error)
	Remove(context.Context, tailscale.RouteSelector, string) (model.OperationReceipt, error)
}

type ReadinessProvider interface {
	Readiness(context.Context, tailscale.ReadinessOptions) (model.Readiness, error)
}

// MutationApproval binds a confirmed TUI action to the exact observations that
// were shown to the operator. CLI callers may omit it, but interactive callers
// must not silently replace a listener or route after confirmation. Batch
// callers may allow unrelated routes to change because earlier batch members
// are expected to mutate the provider route set; TargetRoutesHash and
// RouteIDsHash still protect the exact route for this target.
type MutationApproval struct {
	Target                 model.Target
	ListenerID             string
	ListenerPID            int
	ListenerProcess        string
	ListenerStart          string
	ListenerCommandLine    string
	ListenerTarget         model.Target
	RouteIDsHash           string
	TargetRoutesHash       string
	AllRoutesHash          string
	AllowOtherRouteChanges bool
}

type Controller struct {
	Discoverer interface {
		List(context.Context) (model.ListenerSnapshot, error)
	}
	Provider            Provider
	Now                 func() time.Time
	mu                  sync.Mutex
	busy                map[string]bool
	managed             map[string]bool
	managedFingerprints map[string]string
	MutationLockPath    string
	ReadinessOptions    tailscale.ReadinessOptions
	refreshSeq          uint64
	lastListeners       *model.ListenerSnapshot
	lastExposures       *model.ExposureSnapshot
	desired             map[string]model.ExposureMode
	operations          map[string]model.OperationReceipt
	operationStates     map[string]model.ExposureState
	operationStarted    map[string]time.Time
	events              []model.OperationEvent
}

func NewController(d interface {
	List(context.Context) (model.ListenerSnapshot, error)
}, p Provider) *Controller {
	return &Controller{Discoverer: d, Provider: p, Now: time.Now, busy: map[string]bool{}, managed: map[string]bool{}, managedFingerprints: map[string]string{}, desired: map[string]model.ExposureMode{}, operations: map[string]model.OperationReceipt{}, operationStates: map[string]model.ExposureState{}, operationStarted: map[string]time.Time{}}
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) MarkManaged(routeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.managed[routeID] = true
	// A selector without a route fingerprint is never trusted for a future
	// mutation. Keep this method for compatibility, but verified operations use
	// markManagedRoute below.
	delete(c.managedFingerprints, routeID)
}

func routeFingerprint(route model.ExposureRoute) string {
	return tailscale.RouteFingerprint(route)
}

func (c *Controller) markManagedRoute(route model.ExposureRoute) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.managed == nil {
		c.managed = map[string]bool{}
	}
	if c.managedFingerprints == nil {
		c.managedFingerprints = map[string]string{}
	}
	fingerprint := routeFingerprint(route)
	if route.ID != "" {
		c.managed[route.ID] = true
		c.managedFingerprints[route.ID] = fingerprint
	}
	if route.ProviderKey != "" {
		c.managed[route.ProviderKey] = true
		c.managedFingerprints[route.ProviderKey] = fingerprint
	}
}

func (c *Controller) revokeManagedRoute(route model.ExposureRoute) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fingerprint := routeFingerprint(route)
	for _, key := range []string{route.ID, route.ProviderKey} {
		if key != "" && c.managedFingerprints[key] == fingerprint {
			delete(c.managed, key)
			delete(c.managedFingerprints, key)
		}
	}
}

func (c *Controller) routeManagedLocked(route model.ExposureRoute) bool {
	fingerprint := routeFingerprint(route)
	for _, key := range []string{route.ID, route.ProviderKey} {
		if key != "" && c.managed[key] && c.managedFingerprints[key] == fingerprint {
			return true
		}
	}
	return false
}

func (c *Controller) decorateExposureRoutes(snapshot *model.ExposureSnapshot) {
	if snapshot == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range snapshot.Routes {
		if c.routeManagedLocked(snapshot.Routes[i]) {
			snapshot.Routes[i].Ownership = model.OwnershipManaged
		}
	}
}

func (c *Controller) Refresh(ctx context.Context) (View, error) {
	if c.Discoverer == nil || c.Provider == nil {
		return View{}, model.NewError(model.ErrDependency, "exposure", "discovery and exposure providers are required", true, "unavailable", "Configure providers and retry.")
	}
	c.mu.Lock()
	c.refreshSeq++
	generation := c.refreshSeq
	c.mu.Unlock()
	type listenerResult struct {
		snapshot model.ListenerSnapshot
		err      error
	}
	type exposureResult struct {
		snapshot model.ExposureSnapshot
		err      error
	}
	listenerCh := make(chan listenerResult, 1)
	exposureCh := make(chan exposureResult, 1)
	go func() {
		snapshot, err := c.Discoverer.List(ctx)
		listenerCh <- listenerResult{snapshot: snapshot, err: err}
	}()
	go func() {
		snapshot, err := c.Provider.List(ctx)
		exposureCh <- exposureResult{snapshot: snapshot, err: err}
	}()
	var listenerOutcome listenerResult
	var exposureOutcome exposureResult
	listenerDone, exposureDone := false, false
	for !listenerDone || !exposureDone {
		// Prefer already-buffered results before observing cancellation so a
		// completed source is not discarded merely because the other source timed
		// out at the same instant.
		if !listenerDone {
			select {
			case listenerOutcome = <-listenerCh:
				listenerDone = true
				continue
			default:
			}
		}
		if !exposureDone {
			select {
			case exposureOutcome = <-exposureCh:
				exposureDone = true
				continue
			default:
			}
		}
		select {
		case outcome := <-listenerCh:
			if !listenerDone {
				listenerOutcome, listenerDone = outcome, true
			}
		case outcome := <-exposureCh:
			if !exposureDone {
				exposureOutcome, exposureDone = outcome, true
			}
		case <-ctx.Done():
			if !listenerDone {
				select {
				case outcome := <-listenerCh:
					listenerOutcome = outcome
				default:
					err := refreshContextError("listener discovery", ctx.Err())
					listenerOutcome = listenerResult{snapshot: model.ListenerSnapshot{At: c.now(), Source: "listener", Error: ptr(err.Safe())}, err: err}
				}
			}
			if !exposureDone {
				select {
				case outcome := <-exposureCh:
					exposureOutcome = outcome
				default:
					err := refreshContextError("exposure status", ctx.Err())
					exposureOutcome = exposureResult{snapshot: model.ExposureSnapshot{At: c.now(), Source: "exposure", Error: ptr(err.Safe())}, err: err}
				}
			}
			listenerDone, exposureDone = true, true
		}
	}
	listeners, listenerErr := listenerOutcome.snapshot, listenerOutcome.err
	exposures, exposureErr := exposureOutcome.snapshot, exposureOutcome.err
	if listenerErr == nil && (!listeners.Authoritative || listeners.Error != nil) {
		listenerErr = model.NewError(model.ErrUnknown, "exposure", "listener source is not authoritative", true, "unknown", "Refresh listener discovery before changing exposure.")
	}
	if exposureErr == nil && (!exposures.Authoritative || exposures.Error != nil) {
		exposureErr = model.NewError(model.ErrUnknown, "exposure", "exposure source is not authoritative", true, "unknown", "Refresh Tailscale state before changing exposure.")
	}

	c.mu.Lock()
	if generation == c.refreshSeq {
		listeners = c.publishListenerSnapshot(listeners, listenerErr)
		exposures = c.publishExposureSnapshot(exposures, exposureErr)
	} else {
		// A newer refresh has already superseded this result. Do not let a slow
		// provider response overwrite the last published source snapshots or
		// report its error against the newer view.
		listenerErr = nil
		exposureErr = nil
		if c.lastListeners != nil {
			listeners = cloneListenerSnapshot(*c.lastListeners)
		}
		if c.lastExposures != nil {
			exposures = cloneExposureSnapshot(*c.lastExposures)
		}
	}
	if exposures.Authoritative && exposures.Error == nil {
		for key, fingerprint := range c.managedFingerprints {
			stillOwned := false
			for _, route := range exposures.Routes {
				if (route.ID == key || route.ProviderKey == key) && routeFingerprint(route) == fingerprint {
					stillOwned = true
					break
				}
			}
			if !stillOwned {
				delete(c.managed, key)
				delete(c.managedFingerprints, key)
			}
		}
	}
	managed := make(map[string]bool, len(c.managed))
	for _, route := range exposures.Routes {
		if c.routeManagedLocked(route) {
			if route.ID != "" {
				managed[route.ID] = true
			}
			if route.ProviderKey != "" {
				managed[route.ProviderKey] = true
			}
		}
	}
	c.mu.Unlock()
	view := Reconcile(listeners, exposures, managed)
	c.mu.Lock()
	view.Events = append([]model.OperationEvent(nil), c.events...)
	c.decorateView(&view)
	c.mu.Unlock()
	if listenerErr != nil {
		view.Warnings = append(view.Warnings, listenerErr.Error())
	}
	if exposureErr != nil {
		view.Warnings = append(view.Warnings, exposureErr.Error())
	}
	if listenerErr != nil {
		return view, listenerErr
	}
	if exposureErr != nil {
		return view, exposureErr
	}
	return view, nil
}

func (c *Controller) publishListenerSnapshot(current model.ListenerSnapshot, sourceErr error) model.ListenerSnapshot {
	if sourceErr == nil && current.Authoritative {
		current.Stale = false
		c.lastListeners = ptr(cloneListenerSnapshot(current))
		return current
	}
	if c.lastListeners != nil {
		stale := cloneListenerSnapshot(*c.lastListeners)
		stale.At = current.At
		stale.Authoritative = false
		stale.Stale = true
		stale.Warnings = append(stale.Warnings, "listener source unavailable; showing the last known snapshot")
		if sourceErr != nil {
			stale.Error = ptr(model.AsAppError(sourceErr).Safe())
		}
		return stale
	}
	if sourceErr != nil && current.Error == nil {
		current.Error = ptr(model.AsAppError(sourceErr).Safe())
	}
	return current
}

func (c *Controller) publishExposureSnapshot(current model.ExposureSnapshot, sourceErr error) model.ExposureSnapshot {
	if sourceErr == nil && current.Authoritative {
		current.Stale = false
		c.lastExposures = ptr(cloneExposureSnapshot(current))
		return current
	}
	if c.lastExposures != nil {
		stale := cloneExposureSnapshot(*c.lastExposures)
		stale.At = current.At
		stale.Authoritative = false
		stale.Stale = true
		stale.Warnings = append(stale.Warnings, "exposure source unavailable; showing the last known snapshot")
		if sourceErr != nil {
			stale.Error = ptr(model.AsAppError(sourceErr).Safe())
		}
		return stale
	}
	if sourceErr != nil && current.Error == nil {
		current.Error = ptr(model.AsAppError(sourceErr).Safe())
	}
	return current
}

func refreshContextError(source string, cause error) *model.AppError {
	if errors.Is(cause, context.DeadlineExceeded) {
		return model.WrapError(model.ErrTimeout, "exposure", source+" timed out", true, "timeout", "Retry after checking the dependency.", cause)
	}
	return model.WrapError(model.ErrCancelled, "exposure", source+" was cancelled", true, "cancelled", "Retry the refresh.", cause)
}

func cloneListenerSnapshot(snapshot model.ListenerSnapshot) model.ListenerSnapshot {
	snapshot.Listeners = append([]model.Listener(nil), snapshot.Listeners...)
	snapshot.Warnings = append([]string(nil), snapshot.Warnings...)
	return snapshot
}

func cloneExposureSnapshot(snapshot model.ExposureSnapshot) model.ExposureSnapshot {
	snapshot.Routes = append([]model.ExposureRoute(nil), snapshot.Routes...)
	snapshot.Warnings = append([]string(nil), snapshot.Warnings...)
	return snapshot
}

func (c *Controller) recordEvent(event model.OperationEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEventLocked(event)
}

func (c *Controller) recordEventLocked(event model.OperationEvent) {
	c.events = append(c.events, event)
	if len(c.events) > 100 {
		c.events = append([]model.OperationEvent(nil), c.events[len(c.events)-100:]...)
	}
}

func (c *Controller) recordVerificationEvent(operationID string, target model.Target, mode model.ExposureMode, err error) {
	state := model.ExposureSucceeded
	if err != nil {
		state = operationStateForError(err)
	}
	at := c.now()
	c.mu.Lock()
	started := c.operationStarted[operationID]
	c.mu.Unlock()
	c.recordEvent(model.OperationEvent{OperationID: operationID, Phase: "verification", At: at, Duration: eventDuration(started, at), Target: target, Mode: mode, Capability: operationCapability(mode), State: state, ErrorCode: operationErrorCode(err)})
}

func (c *Controller) recordReceiptEvent(operationID, phase string, target model.Target, mode model.ExposureMode, err error) {
	state := model.ExposureApplying
	if err != nil {
		state = operationStateForError(err)
	}
	at := c.now()
	c.mu.Lock()
	started := c.operationStarted[operationID]
	c.mu.Unlock()
	c.recordEvent(model.OperationEvent{OperationID: operationID, Phase: phase, At: at, Duration: eventDuration(started, at), Target: target, Mode: mode, Capability: operationCapability(mode), State: state, ErrorCode: operationErrorCode(err)})
}

func operationStateForError(err error) model.ExposureState {
	if err == nil {
		return model.ExposureFailed
	}
	switch model.AsAppError(err).Code {
	case model.ErrCancelled:
		return model.ExposureCancelled
	case model.ErrVerification, model.ErrUnknown, model.ErrTimeout:
		return model.ExposureUnverified
	default:
		return model.ExposureFailed
	}
}

func eventDuration(start, end time.Time) string {
	if start.IsZero() || end.Before(start) {
		return ""
	}
	return end.Sub(start).String()
}

func operationCapability(mode model.ExposureMode) string {
	switch mode {
	case model.ExposureServe:
		return "serve_exact_route"
	case model.ExposureFunnel:
		return "funnel_exact_route"
	case model.ExposureDisabled:
		return "exact_route_removal"
	default:
		return ""
	}
}

func (c *Controller) decorateView(view *View) {
	for i := range view.Items {
		key, ok := reconciledTargetKey(view.Items[i])
		if !ok {
			continue
		}
		if desired, exists := c.desired[key]; exists {
			view.Items[i].DesiredMode = desired
		}
		if operation, exists := c.operationStates[key]; exists {
			view.Items[i].OperationState = operation
		}
		if receipt, exists := c.operations[key]; exists {
			copyReceipt := receipt
			view.Items[i].LastOperation = &copyReceipt
		}
	}
}

func reconciledTargetKey(item ReconciledItem) (string, bool) {
	if item.Listener != nil {
		return item.Listener.Target.Normalized().Key(), true
	}
	if len(item.Routes) > 0 {
		return item.Routes[0].Target.Normalized().Key(), true
	}
	return "", false
}

func (c *Controller) Apply(ctx context.Context, target model.Target, mode model.ExposureMode, confirmFunnel, confirmExternal bool, timeout time.Duration) (receipt model.OperationReceipt, applyErr error) {
	return c.apply(ctx, target, mode, "", model.ExposureDisabled, confirmFunnel, confirmExternal, timeout, nil)
}

func (c *Controller) ApplyApproved(ctx context.Context, target model.Target, mode model.ExposureMode, confirmFunnel, confirmExternal bool, timeout time.Duration, approval MutationApproval) (receipt model.OperationReceipt, applyErr error) {
	return c.apply(ctx, target, mode, "", model.ExposureDisabled, confirmFunnel, confirmExternal, timeout, &approval)
}

// ApplyRoute removes one exact observed route. It is used when a target has
// more than one exposure route (for example, both Serve and Funnel) and the
// caller has explicitly selected the provider selector shown in the preview.
func (c *Controller) ApplyRoute(ctx context.Context, target model.Target, providerKey string, confirmExternal bool, timeout time.Duration) (receipt model.OperationReceipt, applyErr error) {
	return c.applyRoute(ctx, target, providerKey, confirmExternal, timeout, nil)
}

func (c *Controller) ApplyRouteApproved(ctx context.Context, target model.Target, providerKey string, confirmExternal bool, timeout time.Duration, approval MutationApproval) (receipt model.OperationReceipt, applyErr error) {
	return c.applyRoute(ctx, target, providerKey, confirmExternal, timeout, &approval)
}

func (c *Controller) applyRoute(ctx context.Context, target model.Target, providerKey string, confirmExternal bool, timeout time.Duration, approval *MutationApproval) (receipt model.OperationReceipt, applyErr error) {
	if providerKey == "" {
		return model.OperationReceipt{}, model.NewError(model.ErrInvalidInput, "exposure", "an exact route selector is required", false, "invalid", "Refresh and select one exact route before disabling.")
	}
	return c.apply(ctx, target, model.ExposureDisabled, providerKey, model.ExposureDisabled, false, confirmExternal, timeout, approval)
}

// ApplyRouteMode removes the one exact route for target with the requested
// exposure mode. It is useful to noninteractive callers that cannot present a
// route picker; the controller still rejects zero or multiple matches.
func (c *Controller) ApplyRouteMode(ctx context.Context, target model.Target, routeMode model.ExposureMode, confirmExternal bool, timeout time.Duration) (receipt model.OperationReceipt, applyErr error) {
	if routeMode != model.ExposureServe && routeMode != model.ExposureFunnel {
		return model.OperationReceipt{}, model.NewError(model.ErrInvalidInput, "exposure", "unsupported route mode", false, "invalid", "Choose serve or funnel when selecting a route to disable.")
	}
	return c.apply(ctx, target, model.ExposureDisabled, "", routeMode, false, confirmExternal, timeout, nil)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func AcquireMutationLock(ctx context.Context, path string) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return func() {}, nil
	}
	info, err := os.Lstat(path)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
		return nil, model.NewError(model.ErrUnsafe, "exposure", "refusing unsafe mutation lock path", false, "unsafe", "Replace the lock with an owner-only regular file and retry.")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, model.WrapError(model.ErrOperation, "exposure", "cannot inspect mutation lock", true, "unavailable", "Check the config directory permissions and retry.", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, model.WrapError(model.ErrOperation, "exposure", "cannot open mutation lock", true, "unavailable", "Check the config directory permissions and retry.", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, model.WrapError(model.ErrOperation, "exposure", "cannot restrict mutation lock permissions", false, "unsafe", "Fix the lock permissions and retry.", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, model.WrapError(model.ErrTimeout, "exposure", "mutation lock acquisition timed out", true, "timeout", "Retry after another tailge process finishes.", err)
			}
			return nil, model.WrapError(model.ErrCancelled, "exposure", "mutation lock acquisition cancelled", true, "cancelled", "Retry the operation.", err)
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, model.WrapError(model.ErrOperation, "exposure", "cannot lock mutations", true, "busy", "Retry after another tailge process finishes.", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func ptr[T any](v T) *T { return &v }
