package workspace

import (
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
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
		{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady},
		{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReady},
	}}
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

func TestURLPolicyPreservesExplicitObservationAndTCPOnlyClassification(t *testing.T) {
	item := workspaceItem()
	if selector := RawTCPRouteSelector(item); selector != "serve:tcp=3000" {
		t.Fatalf("raw TCP selector = %q", selector)
	}
	if _, ok := ObservedRouteURL(item.Routes[0]); ok {
		t.Fatal("TCP selector was treated as an observed browser URL")
	}
	item.Routes[0].URL = "https://dev.example.ts.net"
	if url, ok := ObservedRouteURL(item.Routes[0]); !ok || url != item.Routes[0].URL {
		t.Fatalf("explicit URL was not preserved: %q %t", url, ok)
	}
	status := tailscale.Status{}
	status.Self.DNSName = "dev.example.ts.net"
	preview, err := ServeTCPBrowserURL(status, item.Routes[0])
	if err != nil || preview != "http://dev.example.ts.net:3000/" {
		t.Fatalf("Serve TCP preview = %q, err=%v", preview, err)
	}
}
