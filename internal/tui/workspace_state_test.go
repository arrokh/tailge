package tui

import (
	"testing"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
)

func TestWorkspaceStateSelectionPreservesStableIdentity(t *testing.T) {
	m := workspaceFixture()
	m.selectedID = m.view.Items[0].ID
	m.selectedIdx = 0

	m.toggleVisualSelection()
	if !m.visualSelection || !m.isMarked(m.selectedID) {
		t.Fatalf("visual selection did not mark the stable selected item: %#v", m.workspaceState)
	}

	first := m.view.Items[0]
	second := first
	second.ID = "another-item"
	second.Listener = &model.Listener{ID: "listener-two", Name: "api", Target: model.Target{Address: "127.0.0.1", Port: 2000, Protocol: "tcp"}, Scope: model.ScopeLoopback}
	second.Routes = []model.ExposureRoute{{ID: "route-two", ProviderKey: "serve:tcp=2000", Target: model.Target{Address: "127.0.0.1", Port: 2000, Protocol: "tcp"}, Mode: model.ExposureServe, State: model.ExposureActive}}
	m.view.Items = []exposure.ReconciledItem{second, first}
	m.reselect(m.selectedID, 0)
	if m.selectedID != "listener-app" || m.selectedIdx != 1 {
		t.Fatalf("selection identity was not preserved: id=%q index=%d", m.selectedID, m.selectedIdx)
	}
}

func TestWorkspaceStateMapsExternalKeysToExplicitEffects(t *testing.T) {
	state := newWorkspaceState()
	tests := []struct {
		key  string
		kind workspaceEffectKind
	}{
		{"r", workspaceEffectRefresh},
		{"R", workspaceEffectRetry},
		{"o", workspaceEffectOpenObservedURL},
		{"O", workspaceEffectOpenLocalURL},
		{"y", workspaceEffectCopyURL},
		{"c", workspaceEffectCancelOperation},
		{"x", workspaceEffectTerminateProcess},
		{"q", workspaceEffectQuit},
		{"ctrl+c", workspaceEffectQuit},
	}
	for _, test := range tests {
		effect, ok := state.effectForKey(test.key)
		if !ok || effect.kind != test.kind {
			t.Errorf("key %q mapped to %#v, want %v", test.key, effect, test.kind)
		}
	}
	if _, ok := state.effectForKey("s"); ok {
		t.Fatal("local action key unexpectedly produced an external effect")
	}
}

func TestWorkspaceViewDoesNotMutateDecisionState(t *testing.T) {
	m := workspaceFixture()
	m.width = 0
	m.height = 0
	m.listScroll = 17
	beforeWidth, beforeHeight, beforeScroll := m.width, m.height, m.listScroll

	_ = m.View()

	if m.width != beforeWidth || m.height != beforeHeight || m.listScroll != beforeScroll {
		t.Fatalf("View mutated workspace state: before=(%d,%d,%d) after=(%d,%d,%d)", beforeWidth, beforeHeight, beforeScroll, m.width, m.height, m.listScroll)
	}
}

func TestWorkspaceStateNoopEffectKindIsNotExecutable(t *testing.T) {
	m := workspaceFixture()
	m.modal = modalNone
	if cmd := m.executeWorkspaceEffect(workspaceEffect{kind: workspaceEffectNone}); cmd != nil {
		t.Fatal("no-op workspace effect produced a command")
	}
	if m.modal != modalNone || m.view.Items[0].State != model.ExposureActive {
		t.Fatal("no-op workspace effect changed state")
	}
}
