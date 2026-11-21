package tui

import (
	"strings"
	"testing"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	tea "github.com/charmbracelet/bubbletea"
)

func TestProcessHideViewsComposeWithSearchAndSort(t *testing.T) {
	m := workspaceFixture()
	base := m.view.Items[0]
	for i, name := range []string{"aardvark", "zebra"} {
		listener := *base.Listener
		listener.ID, listener.Name, listener.Target.Port = name, name, 4000+i
		if i == 1 {
			listener.Process = "other"
		}
		m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: name, Listener: &listener, State: "disabled", Mode: exposuredata.ExposureDisabled})
	}
	m.reselect(base.ID, 0)
	m.selectedItems = map[string]bool{base.ID: true}
	m.Update(keyRune('i'))
	if items := m.actionItems(); len(items) != 1 || items[0].ID != "zebra" {
		t.Fatalf("hidden rows were still actionable: %#v", items)
	}
	m.Update(keyRune('I'))
	if m.isMarked(base.ID) {
		t.Fatal("revealing a process must not restore its old mutation selection")
	}
	if items := m.items(); len(items) != 3 || items[0].ID != "zebra" || items[1].ID != "aardvark" {
		t.Fatalf("All should group hidden rows last and keep name sort: %#v", items)
	}
	m.Update(keyRune('I'))
	m.query = "aardvark"
	if items := m.items(); len(items) != 1 || items[0].ID != "aardvark" {
		t.Fatalf("Hidden view did not compose with search: %#v", items)
	}
	m.Update(keyRune('i'))
	m.Update(keyRune('I'))
	if items := m.items(); len(items) != 1 || items[0].ID != "aardvark" {
		t.Fatalf("unhide should restore the process: %#v", items)
	}
}

func TestInactiveHTTPSRootDisableShortcutRemovesExactRoute(t *testing.T) {
	m := workspaceFixture()
	route := m.view.Exposures.Routes[0]
	route.ProviderKey, route.Backend, route.Path = "serve:https=443", "http://127.0.0.1:3000", "/"
	route.Kind, route.Ownership = exposuredata.RouteKindHTTPSRoot, exposuredata.OwnershipUnknown
	m.view.Listeners.Listeners = nil
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{route}
	m.view = exposure.Reconcile(m.view.Listeners, m.view.Exposures, nil)
	provider := &httpsRootTestProvider{snapshot: m.view.Exposures, caps: httpsRootCapabilities(), readiness: httpsRootReadiness()}
	m.controller = exposure.NewController(&httpsRootTestListener{snapshot: m.view.Listeners}, provider)

	m.Update(keyRune('d'))
	if m.modal != modalConfirm || m.actionSession.confirm || !strings.Contains(m.View(), "serve:https=443") {
		t.Fatalf("d should open a Cancel-first exact route preview: %s", m.View())
	}
	m.Update(keyType(tea.KeyTab))
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatalf("confirmed removal was blocked: %s", m.banner)
	}
	result := cmd().(operationDoneMsg)
	if result.err != nil || !result.receipt.Verified || len(provider.removes) != 1 || provider.removes[0].ID != route.ProviderKey {
		t.Fatalf("exact removal failed: %v, removed: %#v", result.err, provider.removes)
	}
}
