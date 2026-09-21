// Package probe owns the disposable compatibility-probe lifecycle. It keeps
// mutation, uncertain-operation cleanup, verification, and evidence persistence
// together so command reporting does not duplicate exposure safety policy.
package probe

import (
	"context"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

type Discoverer interface {
	List(context.Context) (model.ListenerSnapshot, error)
}

type Provider interface {
	tailscale.Exposer
	Version(context.Context) (string, error)
}

type ConfigStore interface {
	Save(context.Context, config.Config) error
}

type Runner struct {
	Discoverer       Discoverer
	Provider         Provider
	Config           ConfigStore
	MutationLockPath string
	Now              func() time.Time
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) Run(ctx context.Context, cfg *config.Config, target model.Target, mode model.ExposureMode, confirmPublic bool) error {
	if r.Discoverer == nil || r.Provider == nil || r.Config == nil {
		return model.NewError(model.ErrDependency, "probe", "compatibility probe providers are unavailable", true, "unavailable", "Configure local discovery, Tailscale, and config storage, then retry.")
	}
	if cfg == nil {
		return model.NewError(model.ErrInvalidInput, "probe", "compatibility probe requires configuration", false, "invalid", "Load configuration before starting the probe.")
	}
	unlock, err := exposure.AcquireMutationLock(ctx, r.MutationLockPath)
	if err != nil {
		return err
	}
	defer unlock()
	return r.runLocked(ctx, cfg, target, mode, confirmPublic)
}

func (r Runner) runLocked(ctx context.Context, cfg *config.Config, target model.Target, mode model.ExposureMode, confirmPublic bool) error {
	if mode != model.ExposureServe && mode != model.ExposureFunnel {
		return model.NewError(model.ErrInvalidInput, "probe", "--probe must be serve or funnel", false, "invalid", "Choose serve or funnel for the compatibility probe.")
	}
	if mode == model.ExposureFunnel && !confirmPublic {
		return model.NewError(model.ErrUnsafe, "probe", "Funnel compatibility probe requires explicit public confirmation", false, "not_confirmed", "Supply --confirm-public only for a disposable target.")
	}
	if model.ScopeForAddress(target.Address) != model.ScopeLoopback {
		return model.NewError(model.ErrUnsafe, "probe", "compatibility probes require a loopback target", false, "unsafe", "Use an exact 127.0.0.1 or ::1 disposable listener.")
	}
	if err := r.requireSingleListener(ctx, target); err != nil {
		return err
	}
	before, err := r.Provider.List(ctx)
	if err != nil {
		return err
	}
	if !before.Authoritative || before.Error != nil {
		return model.NewError(model.ErrUnknown, "probe", "cannot probe with incomplete Tailscale exposure state", true, "unknown", "Fix Tailscale status and retry.")
	}
	if len(exposure.RouteIDs(before.Routes, target)) != 0 {
		return model.NewError(model.ErrUnsafe, "probe", "probe target already has an exposure route", false, "unsafe", "Choose a fresh disposable target; existing routes are never overwritten.")
	}
	caps, err := r.Provider.Capabilities(ctx)
	if err != nil {
		return err
	}
	if err := r.requireSingleListener(ctx, target); err != nil {
		return model.WrapError(model.ErrInvalidInput, "probe", "probe target changed during capability inspection", false, "changed", "Keep the disposable listener running and retry.", err)
	}
	if mode == model.ExposureServe && !caps.ExactServe {
		return model.NewError(model.ErrUnsupported, "probe", "Serve exact compatibility probing is not supported by this Tailscale CLI", false, "unsupported", "Use a provider/version with exact route cleanup.")
	}
	if mode == model.ExposureFunnel && !caps.ExactFunnel {
		return model.NewError(model.ErrUnsupported, "probe", "Funnel exact compatibility probing is not supported by this Tailscale CLI", false, "unsupported", "Use a provider/version with exact route cleanup; broad reset is disabled.")
	}
	change := tailscale.ExposureChange{
		Target: target,
		Mode:   mode,
		Preconditions: tailscale.ExposurePrecondition{
			RouteIDs:      []string{},
			RouteIDsHash:  exposure.RouteIDsHash(before.Routes, target),
			AllRoutesHash: tailscale.RoutesHash(before.Routes),
		},
	}
	expectedProviderKey := tailscale.ProviderKeyForTargetWithCapabilities(mode, target, caps)
	if mode == model.ExposureFunnel && caps.FunnelLegacy {
		expectedProviderKey = ""
	}
	if _, err := r.Provider.Set(ctx, change); err != nil {
		return r.handleUncertainCreation(ctx, target, mode, expectedProviderKey, err)
	}
	after, route, err := r.waitRoute(ctx, target, mode, true, expectedProviderKey)
	if err != nil {
		if cleanupErr := r.cleanupProbeRouteFresh(target, mode, expectedProviderKey); cleanupErr != nil {
			return model.WrapError(model.ErrVerification, "probe", "probe route verification failed and cleanup could not be completed", false, "cleanup_failed", "Inspect the exact temporary route manually; readiness remains disabled.", cleanupErr)
		}
		return model.WrapError(model.ErrVerification, "probe", "probe route could not be verified after creation", true, "unknown", "The temporary route was not observed; retry only after inspecting Tailscale.", err)
	}
	if route.ProviderKey == "" {
		return model.NewError(model.ErrVerification, "probe", "probe route has no exact cleanup selector", false, "cleanup_failed", "Inspect and remove the temporary route manually; readiness remains disabled.")
	}
	if err := r.cleanupProbeRoute(ctx, target, mode, after, expectedProviderKey); err != nil {
		return model.WrapError(model.ErrVerification, "probe", "probe route cleanup failed", false, "cleanup_failed", "Remove the named temporary route manually; readiness remains disabled.", err)
	}
	version, err := r.Provider.Version(ctx)
	if err != nil {
		return err
	}
	probeAt := r.now().UTC().Format(time.RFC3339Nano)
	if mode == model.ExposureServe {
		cfg.ServeProbeVersion, cfg.ServeProbeAt = version, probeAt
	} else {
		cfg.FunnelProbeVersion, cfg.FunnelProbeAt = version, probeAt
	}
	if err := r.Config.Save(ctx, *cfg); err != nil {
		return model.WrapError(model.ErrConfig, "probe", "compatibility probe passed but evidence could not be persisted", true, "unknown", "Keep the evidence output and retry config persistence; mutations remain disabled.", err)
	}
	return nil
}

func (r Runner) requireSingleListener(ctx context.Context, target model.Target) error {
	listeners, err := r.Discoverer.List(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, listener := range listeners.Listeners {
		if listener.Target.Normalized().Key() == target.Key() {
			count++
		}
	}
	if !listeners.Authoritative || listeners.Error != nil || count != 1 {
		return model.NewError(model.ErrInvalidInput, "probe", "probe target must match exactly one authoritative local listener", false, "invalid", "Start or select a disposable listener and retry.")
	}
	return nil
}

func (r Runner) handleUncertainCreation(ctx context.Context, target model.Target, mode model.ExposureMode, expectedProviderKey string, creationErr error) error {
	if !shouldProbeUncertainCreation(creationErr) {
		return creationErr
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	after, _, verifyErr := r.waitRoute(cleanupCtx, target, mode, true, expectedProviderKey)
	if probeSnapshotHasTargetRoute(after, target, mode) {
		if cleanupErr := r.cleanupProbeRoute(cleanupCtx, target, mode, after, ""); cleanupErr != nil {
			return model.WrapError(model.ErrVerification, "probe", "probe creation failed and cleanup could not be completed", false, "cleanup_failed", "Inspect the exact temporary route manually; readiness remains disabled.", cleanupErr)
		}
	}
	if verifyErr != nil {
		return model.WrapError(model.ErrVerification, "probe", "probe creation failed and final route state could not be verified", true, "unknown", "Inspect Tailscale manually; readiness remains disabled.", creationErr)
	}
	return creationErr
}

func shouldProbeUncertainCreation(err error) bool {
	switch model.AsAppError(err).Code {
	case model.ErrOperation, model.ErrTimeout, model.ErrUnknown, model.ErrVerification:
		return true
	default:
		return false
	}
}

func probeSnapshotHasTargetRoute(snapshot model.ExposureSnapshot, target model.Target, mode model.ExposureMode) bool {
	if !snapshot.Authoritative || snapshot.Error != nil {
		return false
	}
	for _, route := range snapshot.Routes {
		if route.Mode == mode && exposure.Matches(route.Target, target) {
			return true
		}
	}
	return false
}

// RouteIdentityComplete reports whether a status route has enough identity for
// exact cleanup. It is exported for focused tests and diagnostic callers.
func RouteIdentityComplete(route model.ExposureRoute) bool {
	selector, err := tailscale.ParseListenerSelector(route.ProviderKey, route.Mode)
	if err != nil {
		return false
	}
	if selector.Transport == "https" {
		return strings.HasPrefix(strings.ToLower(route.URL), "https://")
	}
	return selector.Transport == "tcp"
}

// CleanupRoute removes and verifies one exact observed probe route.
func (r Runner) CleanupRoute(ctx context.Context, target model.Target, mode model.ExposureMode, snapshot model.ExposureSnapshot, expectedProviderKey string) error {
	return r.cleanupProbeRoute(ctx, target, mode, snapshot, expectedProviderKey)
}

func (r Runner) cleanupProbeRoute(ctx context.Context, target model.Target, mode model.ExposureMode, snapshot model.ExposureSnapshot, expectedProviderKey string) error {
	if !snapshot.Authoritative || snapshot.Error != nil {
		return model.NewError(model.ErrVerification, "probe", "temporary route cleanup lacks an authoritative exposure snapshot", false, "cleanup_failed", "Inspect Tailscale manually; readiness remains disabled.")
	}
	matches := make([]model.ExposureRoute, 0, 1)
	for _, route := range snapshot.Routes {
		if route.Mode == mode && exposure.Matches(route.Target, target) && (expectedProviderKey == "" || route.ProviderKey == expectedProviderKey) {
			matches = append(matches, route)
		}
	}
	if len(matches) == 0 {
		return model.NewError(model.ErrVerification, "probe", "temporary route was not observed for exact cleanup", false, "cleanup_failed", "Inspect Tailscale manually; the route may have been applied but is not yet visible.")
	}
	if len(matches) != 1 || matches[0].ProviderKey == "" {
		return model.NewError(model.ErrVerification, "probe", "temporary route cleanup is ambiguous", false, "cleanup_failed", "Inspect the exact temporary route manually; tailge will not guess or use a broad reset.")
	}
	hash := exposure.RouteIDsHash(snapshot.Routes, target)
	if _, err := r.Provider.Remove(ctx, tailscale.RouteSelector{ID: matches[0].ProviderKey, Target: &target, Mode: mode, Service: matches[0].Service, Path: matches[0].Path, Backend: matches[0].Backend, AllRoutesHash: tailscale.RoutesHash(snapshot.Routes)}, hash); err != nil {
		return err
	}
	_, _, err := r.waitRoute(ctx, target, mode, false, "")
	return err
}

func (r Runner) cleanupProbeRouteFresh(target model.Target, mode model.ExposureMode, expectedProviderKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, route, waitErr := r.waitRoute(ctx, target, mode, true, expectedProviderKey)
	if route != nil {
		return r.cleanupProbeRoute(ctx, target, mode, snapshot, expectedProviderKey)
	}
	if waitErr != nil && probeSnapshotHasTargetRoute(snapshot, target, mode) {
		return r.cleanupProbeRoute(ctx, target, mode, snapshot, "")
	}
	return waitErr
}

func (r Runner) waitRoute(ctx context.Context, target model.Target, mode model.ExposureMode, wantPresent bool, expectedProviderKey string) (model.ExposureSnapshot, *model.ExposureRoute, error) {
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	var lastErr error
	var lastSnapshot model.ExposureSnapshot
	for {
		snapshot, err := r.Provider.List(ctx)
		if err != nil {
			lastErr = err
		} else if !snapshot.Authoritative || snapshot.Error != nil {
			lastErr = model.NewError(model.ErrVerification, "probe", "probe exposure state is not authoritative", true, "unknown", "Inspect Tailscale and retry the probe.")
		} else {
			lastSnapshot = snapshot
			matches := make([]*model.ExposureRoute, 0, 1)
			for i := range snapshot.Routes {
				if snapshot.Routes[i].Mode == mode && exposure.Matches(snapshot.Routes[i].Target, target) && (expectedProviderKey == "" || snapshot.Routes[i].ProviderKey == expectedProviderKey) {
					matches = append(matches, &snapshot.Routes[i])
				}
			}
			if len(matches) > 1 {
				return snapshot, nil, model.NewError(model.ErrVerification, "probe", "probe created multiple routes for its target", false, "ambiguous", "Inspect Tailscale manually; cleanup was not attempted because route identity is ambiguous.")
			}
			if wantPresent && len(matches) == 1 && !RouteIdentityComplete(*matches[0]) {
				lastErr = model.NewError(model.ErrVerification, "probe", "probe route is missing its expected URL or exact selector", true, "unknown", "Wait for complete provider status and retry; readiness remains disabled.")
			} else if (wantPresent && len(matches) == 1) || (!wantPresent && len(matches) == 0) {
				if len(matches) == 1 {
					return snapshot, matches[0], nil
				}
				return snapshot, nil, nil
			} else {
				lastErr = model.NewError(model.ErrVerification, "probe", "probe route state has not converged", true, "pending", "Wait for Tailscale status to converge and retry the probe.")
			}
		}
		select {
		case <-ctx.Done():
			return lastSnapshot, nil, ctx.Err()
		case <-deadline.C:
			if lastErr != nil {
				return lastSnapshot, nil, lastErr
			}
			return lastSnapshot, nil, model.NewError(model.ErrTimeout, "probe", "timed out waiting for probe route state", true, "timeout", "Retry after checking Tailscale status.")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
