package tui

import (
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
)

func TestWorkspaceSnapshotSharesPortIdentityAcrossPresentationFlows(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	now := time.Now()
	listener := model.Listener{ID: "listener", Name: "web", Target: target, LastSeen: now}
	route := model.ExposureRoute{ID: "route", ProviderKey: "tcp:3000", Target: target, Mode: model.ExposureServe, State: model.ExposureActive}
	view := exposure.View{
		Items: []exposure.ReconciledItem{
			{ID: listener.ID, Listener: &listener, State: model.ExposureActive, Routes: []model.ExposureRoute{route}},
			{ID: "inactive", Routes: []model.ExposureRoute{{ID: "inactive-route", ProviderKey: "funnel:3000", Target: target, Mode: model.ExposureFunnel}}, State: model.ExposureInactive},
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
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := model.Listener{ID: "listener", Name: "web", Target: target}
	route := model.ExposureRoute{ID: "route", ProviderKey: "tcp:3000", Target: target, Mode: model.ExposureServe, State: model.ExposureActive}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: listener.ID, Listener: &listener, State: model.ExposureState("disabled"), Mode: model.ExposureDisabled},
		{ID: "route-only", Routes: []model.ExposureRoute{route}, State: model.ExposureActive, Mode: model.ExposureServe},
	}}
	items := newWorkspaceSnapshot(view, "", config.Defaults()).Items()
	if len(items) != 1 || items[0].State != model.ExposureActive || items[0].Mode != model.ExposureServe {
		t.Fatalf("collapsed listener did not inherit route state: %#v", items)
	}
}

func TestWorkspaceSnapshotFiltersAfterPortIdentityCollapse(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := model.Listener{ID: "listener", Name: "web", Target: target}
	route := model.ExposureRoute{ID: "route", ProviderKey: "funnel:https=3000", Target: target, Mode: model.ExposureFunnel, State: model.ExposureActive}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "listener", Listener: &listener, State: model.ExposureState("disabled"), Mode: model.ExposureDisabled},
		{ID: "route-only", Routes: []model.ExposureRoute{route}, State: model.ExposureActive, Mode: model.ExposureFunnel},
	}}
	items := newWorkspaceSnapshot(view, "funnel:https=3000", config.Defaults()).Items()
	if len(items) != 1 || items[0].ID != "listener" || len(items[0].Routes) != 1 {
		t.Fatalf("filter split the collapsed target identity: %#v", items)
	}
}

func TestWorkspaceSnapshotDoesNotMutateViewRoutes(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	listener := model.Listener{ID: "listener", Target: target}
	primaryRoutes := make([]model.ExposureRoute, 1, 2)
	primaryRoutes[0] = model.ExposureRoute{ID: "primary", Target: target, Mode: model.ExposureServe}
	duplicateRoute := model.ExposureRoute{ID: "duplicate", Target: target, Mode: model.ExposureFunnel}
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "listener", Listener: &listener, Routes: primaryRoutes, State: model.ExposureActive},
		{ID: "duplicate", Routes: []model.ExposureRoute{duplicateRoute}, State: model.ExposureActive},
	}}
	items := newWorkspaceSnapshot(view, "", config.Defaults()).Items()
	if len(items) != 1 || len(items[0].Routes) != 2 {
		t.Fatalf("snapshot did not merge routes: %#v", items)
	}
	if len(view.Items[0].Routes) != 1 || view.Items[0].Routes[0].ID != "primary" {
		t.Fatalf("snapshot mutated source routes: %#v", view.Items[0].Routes)
	}
}
