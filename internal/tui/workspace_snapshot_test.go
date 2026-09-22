package tui

import (
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
)

func TestWorkspaceSnapshotSharesPortIdentityAcrossPresentationFlows(t *testing.T) {
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	now := time.Now()
	listener := discovery.Listener{ID: "listener", Name: "web", Target: target, LastSeen: now}
	route := exposuredata.ExposureRoute{ID: "route", ProviderKey: "tcp:3000", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	view := exposure.View{
		Items: []exposure.ReconciledItem{
			{ID: listener.ID, Listener: &listener, State: exposuredata.ExposureActive, Routes: []exposuredata.ExposureRoute{route}},
			{ID: "inactive", Routes: []exposuredata.ExposureRoute{{ID: "inactive-route", ProviderKey: "funnel:3000", Target: target, Mode: exposuredata.ExposureFunnel}}, State: exposuredata.ExposureInactive},
		},
	}
	snapshot := newWorkspaceSnapshot(view, "", config.Defaults())
	items := snapshot.Items()
	if len(items) != 1 || items[0].ID != listener.ID || len(items[0].Routes) != 2 {
		t.Fatalf("snapshot did not preserve one target identity: %#v", items)
	}
	if target, ok := itemTarget(items[0]); !ok || target.Key() != listener.Target.Key() {
		t.Fatalf("snapshot target=%#v, ok=%t", target, ok)
	}
}

func TestWorkspaceSnapshotInheritsRouteStateWhenListenerHasNoRoute(t *testing.T) {
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := discovery.Listener{ID: "listener", Name: "web", Target: target}
	route := exposuredata.ExposureRoute{ID: "route", ProviderKey: "tcp:3000", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: listener.ID, Listener: &listener, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled},
		{ID: "route-only", Routes: []exposuredata.ExposureRoute{route}, State: exposuredata.ExposureActive, Mode: exposuredata.ExposureServe},
	}}
	items := newWorkspaceSnapshot(view, "", config.Defaults()).Items()
	if len(items) != 1 || items[0].State != exposuredata.ExposureActive || items[0].Mode != exposuredata.ExposureServe {
		t.Fatalf("collapsed listener did not inherit route state: %#v", items)
	}
}

func TestWorkspaceSnapshotFiltersAfterPortIdentityCollapse(t *testing.T) {
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := discovery.Listener{ID: "listener", Name: "web", Target: target}
	route := exposuredata.ExposureRoute{ID: "route", ProviderKey: "funnel:https=3000", Target: target, Mode: exposuredata.ExposureFunnel, State: exposuredata.ExposureActive}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "listener", Listener: &listener, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled},
		{ID: "route-only", Routes: []exposuredata.ExposureRoute{route}, State: exposuredata.ExposureActive, Mode: exposuredata.ExposureFunnel},
	}}
	items := newWorkspaceSnapshot(view, "funnel:https=3000", config.Defaults()).Items()
	if len(items) != 1 || items[0].ID != "listener" || len(items[0].Routes) != 1 {
		t.Fatalf("filter split the collapsed target identity: %#v", items)
	}
}

func TestWorkspaceSnapshotDoesNotMutateViewRoutes(t *testing.T) {
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := discovery.Listener{ID: "listener", Target: target}
	primaryRoutes := make([]exposuredata.ExposureRoute, 1, 2)
	primaryRoutes[0] = exposuredata.ExposureRoute{ID: "primary", Target: target, Mode: exposuredata.ExposureServe}
	duplicateRoute := exposuredata.ExposureRoute{ID: "duplicate", Target: target, Mode: exposuredata.ExposureFunnel}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "listener", Listener: &listener, Routes: primaryRoutes, State: exposuredata.ExposureActive},
		{ID: "duplicate", Routes: []exposuredata.ExposureRoute{duplicateRoute}, State: exposuredata.ExposureActive},
	}}
	items := newWorkspaceSnapshot(view, "", config.Defaults()).Items()
	if len(items) != 1 || len(items[0].Routes) != 2 {
		t.Fatalf("snapshot did not merge routes: %#v", items)
	}
	if len(view.Items[0].Routes) != 1 || view.Items[0].Routes[0].ID != "primary" {
		t.Fatalf("snapshot mutated source routes: %#v", view.Items[0].Routes)
	}
}
