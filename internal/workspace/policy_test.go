package workspace

import (
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/target"
)

func workspaceItem() exposure.ReconciledItem {
	t := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := discovery.Listener{ID: "listener", Target: t, Name: "web", PID: 4242, Process: "web", ProcessStart: "start", CommandLine: "web --serve"}
	route := exposuredata.ExposureRoute{ID: "route", ProviderKey: "serve:tcp=3000", Target: t, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	return exposure.ReconciledItem{ID: listener.ID, Listener: &listener, Routes: []exposuredata.ExposureRoute{route}, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
}

func authoritativeView(item exposure.ReconciledItem) exposure.View {
	now := time.Now()
	return exposure.View{
		At:        now,
		Items:     []exposure.ReconciledItem{item},
		Listeners: discovery.ListenerSnapshot{At: now, Authoritative: true, Listeners: []discovery.Listener{*item.Listener}},
		Exposures: exposuredata.ExposureSnapshot{At: now, Authoritative: true, Routes: item.Routes},
	}
}

func readyWorkspaceReadiness() readiness.Readiness {
	return readiness.Readiness{Status: readiness.ReadinessReady, Modes: []readiness.ModeReadiness{
		{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady},
		{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady},
	}}
}

func TestKnownMultipleRoutesBlockAggregateEnableButAllowExactDisable(t *testing.T) {
	listenerTarget := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	backendTarget := target.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	listener := discovery.Listener{ID: "listener", Target: listenerTarget, Name: "blog", Process: "node", PID: 4242, Scope: target.ScopeLoopback}
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{
		{ID: "root-route", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "blog-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
	}}
	view := exposure.Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{listener}}, routes, nil)
	item := view.Items[0]
	context := ActionContext{View: view, Readiness: readyWorkspaceReadiness()}

	enable := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureServe, context)
	if !enable.Disabled || !strings.Contains(enable.Reason, "multiple exact routes") || strings.Contains(enable.Reason, "ambiguous") {
		t.Fatalf("aggregate enable did not explain exact multiple routes: %#v", enable)
	}
	disable := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureDisabled, context)
	if disable.Disabled {
		t.Fatalf("exact disable route chooser was blocked: %#v", disable)
	}
}

func TestAmbiguousRouteSetBlocksExactDisable(t *testing.T) {
	item := workspaceItem()
	first := item.Routes[0]
	first.ProviderKey, first.Kind, first.Path, first.Backend = "serve:https=443", exposuredata.RouteKindHTTPPath, "/api", "http://127.0.0.1:3000"
	second := first
	second.ID, second.Backend = "duplicate-status-entry", "http://localhost:3000"
	item.Routes = []exposuredata.ExposureRoute{first, second}
	item.State = exposuredata.ExposureAmbiguous
	view := authoritativeView(item)
	got := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureDisabled, ActionContext{View: view, Readiness: readyWorkspaceReadiness()})
	if !got.Disabled || !strings.Contains(got.Reason, "not authoritative or exact") {
		t.Fatalf("ambiguous duplicate selectors remained mutable: %#v", got)
	}
}

func TestActionAvailabilityDistinguishesRefreshWaitFromStaleState(t *testing.T) {
	item := workspaceItem()
	view := authoritativeView(item)
	view.Exposures.Stale = true
	context := ActionContext{View: view, Readiness: readyWorkspaceReadiness(), RefreshPending: true}
	got := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureFunnel, context)
	if !got.Disabled || !got.Wait || got.Reason != "listener: Action unavailable: refresh is in progress" {
		t.Fatalf("unexpected refresh-pending availability: %#v", got)
	}
	view.Exposures.Stale = false
	context.View = view
	context.RefreshPending = false
	got = ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureFunnel, context)
	if got.Disabled {
		t.Fatalf("ready item was blocked: %#v", got)
	}
}

func TestDisableActionShowsHTTPPathReadinessReason(t *testing.T) {
	item := workspaceItem()
	item.Routes[0].ProviderKey = "serve:https=443"
	item.Routes[0].Kind = exposuredata.RouteKindHTTPPath
	item.Routes[0].Path = "/api"
	item.Routes[0].Backend = "http://127.0.0.1:3000"
	view := authoritativeView(item)
	report := readyWorkspaceReadiness()
	report.Modes[0].HTTPPathStatus = readiness.ReadinessReadOnly
	report.Modes[0].HTTPPathMessage = "the installed Tailscale CLI lacks --set-path"
	report.Modes[0].HTTPPathRemediation = "Upgrade Tailscale."
	got := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureDisabled, ActionContext{View: view, Readiness: report})
	if !got.Disabled || !strings.Contains(got.Reason, "lacks --set-path") || !strings.Contains(got.Reason, "Upgrade Tailscale") {
		t.Fatalf("HTTP path readiness reason missing from Disable: %#v", got)
	}
}

func TestRawModeActionIsUnavailableForNamedHTTPPath(t *testing.T) {
	item := workspaceItem()
	item.Routes[0].ProviderKey = "serve:https=443"
	item.Routes[0].Kind = exposuredata.RouteKindHTTPPath
	item.Routes[0].Path = "/api"
	item.Routes[0].Backend = "http://127.0.0.1:3000"
	view := authoritativeView(item)
	got := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureFunnel, ActionContext{View: view, Readiness: readyWorkspaceReadiness()})
	if !got.Disabled || !strings.Contains(got.Reason, "Named HTTPS paths") {
		t.Fatalf("scope action was not blocked for a named path: %#v", got)
	}
}

func TestHTTPSRootCanChangeVisibilityThroughAnExactScopeAction(t *testing.T) {
	item := workspaceItem()
	item.Routes[0].ProviderKey = "serve:https=4321"
	item.Routes[0].Kind = exposuredata.RouteKindHTTPSRoot
	item.Routes[0].Path = "/"
	view := authoritativeView(item)
	got := ActionAvailabilityForItems([]exposure.ReconciledItem{item}, exposuredata.ExposureFunnel, ActionContext{View: view, Readiness: readyWorkspaceReadiness()})
	if got.Disabled {
		t.Fatalf("exact HTTPS root was blocked from a controlled visibility change: %#v", got)
	}
}

func TestSameStateDoesNotTreatWildcardBackendAsVerifiedNoop(t *testing.T) {
	item := workspaceItem()
	item.Routes[0].Target.Address = "0.0.0.0"
	item.Routes[0].Target = item.Routes[0].Target.Normalized()
	item.Listener.Target.Address = "0.0.0.0"
	view := authoritativeView(item)
	if SameStateForItem(view, item, exposuredata.ExposureServe) {
		t.Fatal("wildcard backend was treated as a verified no-op")
	}
}

func TestURLPolicyAllowsOnlyExplicitValidHTTPSRoutes(t *testing.T) {
	item := workspaceItem()
	if selector := RawTCPRouteSelector(item); selector != "serve:tcp=3000" {
		t.Fatalf("raw TCP selector = %q", selector)
	}
	if _, ok := ObservedHTTPSRouteURL(item.Routes[0]); ok {
		t.Fatal("TCP selector without an observed URL was treated as a browser URL")
	}
	for _, value := range []string{"http://dev.example.ts.net", "ftp://dev.example.ts.net", "https:///missing-host", "https://user:secret@dev.example.ts.net", "not a URL"} {
		item.Routes[0].URL = value
		if url, ok := ObservedHTTPSRouteURL(item.Routes[0]); ok {
			t.Errorf("non-HTTPS/invalid URL %q was accepted as %q", value, url)
		}
	}
	for _, value := range []string{"https://dev.example.ts.net", "HTTPS://dev.example.ts.net:8443/api"} {
		item.Routes[0].URL = value
		if url, ok := ObservedHTTPSRouteURL(item.Routes[0]); !ok || url != value {
			t.Errorf("explicit HTTPS URL was not preserved: got=%q ok=%t", url, ok)
		}
	}
}
