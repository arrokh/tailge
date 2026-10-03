package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/buildinfo"
	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func TestDisplayedOnlyHidesConfiguredPresentationRows(t *testing.T) {
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "system", Listener: &discovery.Listener{Name: "systemd", Process: "systemd", Target: targetmodel.Target{Address: "127.0.0.1", Port: 53, Protocol: "tcp"}}, State: exposuredata.ExposureState("disabled")},
		{ID: "stale", Routes: []exposuredata.ExposureRoute{{Target: targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}}}, State: exposuredata.ExposureInactive},
		{ID: "app", Listener: &discovery.Listener{Name: "app", Process: "app", Target: targetmodel.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}}, State: exposuredata.ExposureState("disabled")},
	}}
	cfg := config.Defaults()
	cfg.ShowInactiveConfiguredPorts = false
	cfg.ShowSystemListeners = false
	items := displayed(view, "", cfg)
	if len(items) != 1 || items[0].ID != "app" {
		t.Fatalf("unexpected filtered items: %#v", items)
	}
}

func TestServiceListDeduplicatesItemsByPort(t *testing.T) {
	m := workspaceFixture()
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{
		ID: "inactive-route-3000",
		Routes: []exposuredata.ExposureRoute{{
			ID: "route-inactive-3000", ProviderKey: "tcp:3000:inactive",
			Target: targetmodel.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"},
			Mode:   exposuredata.ExposureFunnel, State: exposuredata.ExposureInactive,
		}},
		State: exposuredata.ExposureInactive, Mode: exposuredata.ExposureFunnel,
	})
	items := m.items()
	if len(items) != 1 || items[0].ID != "listener-app" {
		t.Fatalf("same-port items were not collapsed: %#v", items)
	}
	if items[0].State == exposuredata.ExposureAmbiguous || len(items[0].Routes) != 2 {
		t.Fatalf("collapsed item was incorrectly marked ambiguous or lost routes: %#v", items[0])
	}
}

func TestPortCollapseRetainsPathHandlersSharingProviderSelector(t *testing.T) {
	m := workspaceFixture()
	first := exposuredata.ExposureRoute{ID: "api-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Target: m.view.Items[0].Listener.Target, Mode: exposuredata.ExposureServe}
	second := first
	second.ID, second.Path, second.URL = "docs-route", "/docs", "https://dev.example.ts.net/docs"
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{first}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: "docs-route-item", Routes: []exposuredata.ExposureRoute{second}, State: exposuredata.ExposureInactive, Mode: exposuredata.ExposureServe})
	items := m.items()
	if len(items) != 1 || len(items[0].Routes) != 2 || items[0].Routes[0].Path == items[0].Routes[1].Path {
		t.Fatalf("same-selector HTTPS path handlers were collapsed as duplicate routes: %#v", items)
	}
}

func TestOrderedUsesConfiguredSortKey(t *testing.T) {
	view := exposure.View{Items: []exposure.ReconciledItem{
		{ID: "b", Listener: &discovery.Listener{Name: "zulu", Target: targetmodel.Target{Address: "127.0.0.1", Port: 80, Protocol: "tcp"}}, State: exposuredata.ExposureState("disabled")},
		{ID: "a", Listener: &discovery.Listener{Name: "alpha", Target: targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}}, State: exposuredata.ExposureState("disabled")},
	}}
	items := ordered(visible(view, ""), "name")
	if len(items) != 2 || items[0].ID != "a" {
		t.Fatalf("unexpected name order: %#v", items)
	}
	items = ordered(visible(view, ""), "port")
	if len(items) != 2 || items[0].ID != "b" {
		t.Fatalf("unexpected port order: %#v", items)
	}
}

func TestOrderedSupportsDescendingNameAndUnsortedModes(t *testing.T) {
	items := []exposure.ReconciledItem{
		{ID: "z", Listener: &discovery.Listener{Name: "zulu", Target: targetmodel.Target{Address: "127.0.0.1", Port: 80, Protocol: "tcp"}}},
		{ID: "a", Listener: &discovery.Listener{Name: "alpha", Target: targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}}},
		{ID: "m", Listener: &discovery.Listener{Name: "middle", Target: targetmodel.Target{Address: "127.0.0.1", Port: 9000, Protocol: "tcp"}}},
	}
	orderedIDs := func(key string) []string {
		result := ordered(items, key)
		ids := make([]string, len(result))
		for index, item := range result {
			ids[index] = item.ID
		}
		return ids
	}
	if got := strings.Join(orderedIDs("name-desc"), ","); got != "z,m,a" {
		t.Fatalf("descending name order = %q, want z,m,a", got)
	}
	if got := strings.Join(orderedIDs("none"), ","); got != "z,a,m" {
		t.Fatalf("unsorted order = %q, want original z,a,m", got)
	}
}

func TestSortShortcutCyclesAndPreservesSelectedIdentity(t *testing.T) {
	m := workspaceFixture()
	m.cfg.Sort = "name"
	second := discovery.Listener{ID: "listener-api", Name: "api", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}}
	third := discovery.Listener{ID: "listener-db", Name: "db", Target: targetmodel.Target{Address: "127.0.0.1", Port: 5000, Protocol: "tcp"}}
	m.view.Items = append(m.view.Items,
		exposure.ReconciledItem{ID: second.ID, Listener: &second},
		exposure.ReconciledItem{ID: third.ID, Listener: &third},
	)
	m.selectedID = "listener-app"
	m.reselect(m.selectedID, m.selectedIdx)
	for index, sortKey := range []string{"name-desc", "none", "name"} {
		key := rune('e')
		if index == 1 {
			key = 'S'
		}
		m.Update(keyRune(key))
		if m.cfg.Sort != sortKey {
			t.Fatalf("sort after e/S = %q, want %q", m.cfg.Sort, sortKey)
		}
		if m.selectedID != "listener-app" {
			t.Fatalf("sort %q changed selected identity to %q", sortKey, m.selectedID)
		}
	}
}

func TestModeFilterShortcutTogglesNonOffRowsAndPreservesSelection(t *testing.T) {
	m := workspaceFixture()
	offListener := discovery.Listener{ID: "listener-off", Name: "off", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: offListener.ID, Listener: &offListener, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.selectedID = offListener.ID
	m.selectedIdx = 1

	m.Update(keyRune('w'))
	if !m.modeNotOffOnly {
		t.Fatal("w did not enable the not-OFF mode filter")
	}
	items := m.items()
	if len(items) != 1 || items[0].ID != "listener-app" || m.selectedID != "listener-app" {
		t.Fatalf("mode filter rows or selection are wrong: items=%#v selected=%q", items, m.selectedID)
	}
	if status := m.modeFilterStatusLabel(); status != "w:not-OFF" {
		t.Fatalf("active mode-filter status=%q", status)
	}

	m.Update(keyRune('w'))
	if m.modeNotOffOnly || len(m.items()) != 2 || m.selectedID != "listener-app" {
		t.Fatalf("second w did not restore all rows while preserving visible selection: filtered=%t items=%#v selected=%q", m.modeNotOffOnly, m.items(), m.selectedID)
	}
	if status := m.modeFilterStatusLabel(); status != "w:all" {
		t.Fatalf("default mode-filter status=%q", status)
	}
}

func TestPaletteSortPreservesSelectionAndOffersEverySupportedSort(t *testing.T) {
	m := workspaceFixture()
	second := discovery.Listener{ID: "listener-api", Name: "api", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second})
	m.selectedID = "listener-app"
	m.reselect(m.selectedID, m.selectedIdx)
	m.openPalette()
	m.paletteInput.SetValue(":sort")
	for _, sortKey := range []string{"name", "name-desc", "none", "port", "address", "exposure"} {
		if !strings.Contains(strings.Join(m.filteredPalette(), "\n"), ":sort "+sortKey) {
			t.Errorf("sort mode %q missing from palette suggestions: %#v", sortKey, m.filteredPalette())
		}
	}
	m.executePalette(":sort name-desc")
	if m.cfg.Sort != "name-desc" || m.selectedID != "listener-app" || m.selectedIdx != 0 {
		t.Fatalf("palette sort failed to retain focused item: sort=%q id=%q index=%d", m.cfg.Sort, m.selectedID, m.selectedIdx)
	}
}

func TestCopySelectedURLKeepsURLVisibleWhenClipboardFails(t *testing.T) {
	items := []exposure.ReconciledItem{{Routes: []exposuredata.ExposureRoute{{URL: "https://dev.example.ts.net"}}}}
	var out, errOut bytes.Buffer
	copySelectedURL(&out, &errOut, items, 0, ClipboardFunc(func(context.Context, string) error { return errors.New("no clipboard") }))
	if !strings.Contains(out.String(), "https://dev.example.ts.net") || !strings.Contains(errOut.String(), "normal terminal text selection") {
		t.Fatalf("clipboard failure hid URL: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestCopySelectedURLHandlesMissingClipboard(t *testing.T) {
	items := []exposure.ReconciledItem{{Routes: []exposuredata.ExposureRoute{{URL: "https://dev.example.ts.net"}}}}
	var out, errOut bytes.Buffer
	copySelectedURL(&out, &errOut, items, 0, nil)
	if !strings.Contains(out.String(), "https://dev.example.ts.net") || !strings.Contains(errOut.String(), "no clipboard integration") {
		t.Fatalf("missing clipboard integration was not handled: out=%q err=%q", out.String(), errOut.String())
	}
}

func TestOSC52ClipboardWritesTerminalSequence(t *testing.T) {
	var out bytes.Buffer
	clipboard := OSC52Clipboard{Output: &out}
	if err := clipboard.Copy(context.Background(), "https://dev.example.ts.net"); err != nil {
		t.Fatalf("OSC52 copy failed: %v", err)
	}
	const want = "\x1b]52;c;aHR0cHM6Ly9kZXYuZXhhbXBsZS50cy5uZXQ=\x07"
	if out.String() != want {
		t.Fatalf("OSC52 sequence = %q, want %q", out.String(), want)
	}
}

func TestRefreshBackoffIsBounded(t *testing.T) {
	if got := refreshBackoff(time.Second, 0); got != time.Second {
		t.Fatalf("initial interval=%s", got)
	}
	if got := refreshBackoff(time.Second, 4); got != 16*time.Second {
		t.Fatalf("backoff interval=%s", got)
	}
	if got := refreshBackoff(time.Second, 6); got != 30*time.Second {
		t.Fatalf("bounded interval=%s", got)
	}
}

func TestRefreshCommandsPublishReadinessBeforeBlockedDiscovery(t *testing.T) {
	release := make(chan struct{})
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(ctx context.Context, _ string, _ ...string) (runner.Result, error) {
		select {
		case <-release:
			return runner.Result{Stdout: "p1\nctest\nn127.0.0.1:8080\n"}, nil
		case <-ctx.Done():
			return runner.Result{ExitCode: -1}, ctx.Err()
		}
	})}
	provider := &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		switch strings.Join(args, " ") {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":true}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		default:
			return runner.Result{}, errors.New("unexpected provider command")
		}
	})}
	controller := exposure.NewController(discoverer, provider)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	viewResult := make(chan tea.Msg, 1)
	readinessResult := make(chan tea.Msg, 1)
	go func() { viewResult <- refreshViewCmd(parent, controller, config.Defaults(), 1)() }()
	go func() {
		readinessResult <- readinessCmd(parent, provider, config.Defaults(), tailscale.ReadinessOptions{}, 1)()
	}()
	select {
	case message := <-readinessResult:
		if message, ok := message.(readinessLoadedMsg); !ok || message.err != nil {
			t.Fatalf("readiness command failed while discovery was blocked: %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("readiness did not publish while discovery was blocked")
	}
	close(release)
	select {
	case <-viewResult:
	case <-time.After(time.Second):
		t.Fatal("listener refresh did not finish after discovery was released")
	}
}

func TestSanitizeTUITextRemovesControlCharacters(t *testing.T) {
	if got := sanitizeTUIText("api\x1b[31m"); strings.Contains(got, "\x1b") {
		t.Fatalf("sanitizer emitted unsafe control characters: %q", got)
	}
}

func TestSanitizeTUITextRemovesANSIFragments(t *testing.T) {
	if got := sanitizeTUIText("\x1b[7m  \x1b[0m"); got != "  " {
		t.Fatalf("sanitizer leaked ANSI fragments: %q", got)
	}
}

func TestInterruptedTUIStopIsQuiet(t *testing.T) {
	var errOut bytes.Buffer
	wrappedInterrupt := fmt.Errorf("%w: %w", tea.ErrProgramKilled, tea.ErrInterrupted)
	if code := reportTUIStop(&errOut, wrappedInterrupt); code != fault.ErrInterrupted.ExitCode() || errOut.Len() != 0 {
		t.Fatalf("user interrupt was reported as an error: code=%d output=%q", code, errOut.String())
	}

	errOut.Reset()
	if code := reportTUIStop(&errOut, errors.New("renderer failed")); code != fault.ErrInterrupted.ExitCode() || !strings.Contains(errOut.String(), "renderer failed") {
		t.Fatalf("real TUI failure was hidden: code=%d output=%q", code, errOut.String())
	}
}

func TestInterruptedTUIStopReportsUnverifiedWork(t *testing.T) {
	m := workspaceFixture()
	m.processBusy = true
	var errOut bytes.Buffer
	reportUnverifiedTUIState(&errOut, m)
	if !strings.Contains(errOut.String(), "operation state is unverified") {
		t.Fatalf("interrupted active work lacked safety warning: %q", errOut.String())
	}
}

func TestCommandPaletteInputUsesSafeVisibleCursor(t *testing.T) {
	m := workspaceFixture()
	m.openPalette()
	view := m.modalView()
	if strings.Contains(view, "[7m") || !strings.Contains(view, "▌") {
		t.Fatalf("command palette leaked cursor control text or lost cursor: %q", view)
	}
}

func TestCommandPaletteInputPreservesHorizontalViewport(t *testing.T) {
	m := workspaceFixture()
	m.openPalette()
	m.paletteInput.SetValue(strings.Repeat("x", 100))
	m.paletteInput.CursorEnd()
	view := m.paletteInputView()
	if width := lipgloss.Width(view); width > lipgloss.Width(m.paletteInput.Prompt)+m.paletteInput.Width+1 {
		t.Fatalf("palette input exceeded its viewport: width=%d view=%q", width, view)
	}
	if !strings.HasSuffix(view, "x▌") {
		t.Fatalf("palette cursor was not kept at the visible end: %q", view)
	}
}

func TestConfirmationOptionsUseSemanticStyles(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cancelFocused := styleConfirmationOptions("dark", "[Cancel] Confirm")
	if !strings.Contains(cancelFocused, "\x1b[1;33m[Cancel]\x1b[0m") || !strings.Contains(cancelFocused, "\x1b[32mConfirm\x1b[0m") {
		t.Fatalf("cancel-focused options lack semantic styles: %q", cancelFocused)
	}
	confirmFocused := styleConfirmationOptions("dark", "Cancel [Confirm]")
	if !strings.Contains(confirmFocused, "\x1b[33mCancel\x1b[0m") || !strings.Contains(confirmFocused, "\x1b[1;32m[Confirm]\x1b[0m") {
		t.Fatalf("confirm-focused options lack semantic styles: %q", confirmFocused)
	}
	if width := lipgloss.Width(sanitizeTUIText(styleConfirmationOptions("dark", "[Cancel] Confirm"))); width != 16 {
		t.Fatalf("compact confirmation option expanded unexpectedly: width=%d", width)
	}
}

func TestPaintAutoUsesColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if got := paint("auto", "1;36", "title"); !strings.Contains(got, "\x1b[1;36m") {
		t.Fatalf("auto theme did not render color: %q", got)
	}
}

func TestPaintHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := paint("dark", "1;36", "title"); got != "title" {
		t.Fatalf("NO_COLOR was ignored: %q", got)
	}
}

func TestModeStatusUnknownWhenProviderOmitsMode(t *testing.T) {
	status := workspace.ModeStatus(readiness.Readiness{At: time.Now()}, exposuredata.ExposureServe)
	if status != readiness.ReadinessUnknown {
		t.Fatalf("status=%s", status)
	}
}

func workspaceFixture() *workspaceModel {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	now := time.Now()
	listener := discovery.Listener{ID: "listener-app", Name: "web", Process: "node", ProcessStart: "test:4242", PID: 4242, CommandLine: "node dev-server", Target: target, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete, FirstSeen: now, LastSeen: now}
	route := exposuredata.ExposureRoute{ID: "route-app", ProviderKey: "tcp:3000", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive, LastSeen: now, LastVerifiedAt: now}
	return &workspaceModel{
		workspaceState: workspaceState{
			cfg: config.Defaults(),
			view: exposure.View{
				At:        now,
				Listeners: discovery.ListenerSnapshot{At: now, Authoritative: true, Listeners: []discovery.Listener{listener}},
				Exposures: exposuredata.ExposureSnapshot{At: now, Authoritative: true, Routes: []exposuredata.ExposureRoute{route}},
				Items:     []exposure.ReconciledItem{{ID: listener.ID, Listener: &listener, Routes: []exposuredata.ExposureRoute{route}, State: exposuredata.ExposureActive, Mode: exposuredata.ExposureServe}},
			},
			readiness: readiness.Readiness{At: now, Status: readiness.ReadinessReady, Modes: []readiness.ModeReadiness{{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady}, {Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady}}},
			hasView:   true, hasReadiness: true, refreshState: refreshCoordinator{seq: 1, viewDone: true, readinessDone: true},
			focus: focusList, width: 120, height: 30,
			activeOps: map[string]context.CancelFunc{},
		},
		ctx:         context.Background(),
		controller:  exposure.NewController(nil, nil),
		searchInput: newInput("/ "), paletteInput: newInput(": "),
	}
}

type httpsRootTestListener struct{ snapshot discovery.ListenerSnapshot }

func (observer *httpsRootTestListener) List(context.Context) (discovery.ListenerSnapshot, error) {
	return observer.snapshot, nil
}

type httpsRootTestProvider struct {
	snapshot  exposuredata.ExposureSnapshot
	caps      tailscale.Capabilities
	readiness readiness.Readiness
	sets      []tailscale.ExposureChange
	removes   []tailscale.RouteSelector
}

func (provider *httpsRootTestProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return provider.caps, nil
}

func (provider *httpsRootTestProvider) Readiness(context.Context, tailscale.ReadinessOptions) (readiness.Readiness, error) {
	return provider.readiness, nil
}

func (provider *httpsRootTestProvider) List(context.Context) (exposuredata.ExposureSnapshot, error) {
	copySnapshot := provider.snapshot
	copySnapshot.Routes = append([]exposuredata.ExposureRoute(nil), provider.snapshot.Routes...)
	return copySnapshot, nil
}

func (provider *httpsRootTestProvider) Set(_ context.Context, change tailscale.ExposureChange) (exposuredata.OperationReceipt, error) {
	provider.sets = append(provider.sets, change)
	routeTarget := change.Target.Normalized()
	if parsed, err := targetmodel.ParseTarget(change.Backend, "tcp"); err == nil {
		routeTarget = parsed
	}
	kind := exposuredata.RouteKindRawTCP
	url := ""
	if change.HTTPSRoot {
		kind = exposuredata.RouteKindHTTPSRoot
		url = fmt.Sprintf("https://dev.example.ts.net:%d/", change.HTTPSPort)
	} else if change.HTTPPath {
		kind = exposuredata.RouteKindHTTPPath
		url = "https://dev.example.ts.net" + change.Path
	}
	route := exposuredata.ExposureRoute{
		ID:          targetmodel.StableID(string(change.Mode), change.Target.Key(), change.ProviderKey, change.Path),
		ProviderKey: change.ProviderKey, Kind: kind,
		Path: change.Path, Backend: change.Backend, Target: routeTarget,
		Mode: change.Mode, URL: url,
		Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive,
	}
	provider.snapshot.Routes = append(provider.snapshot.Routes, route)
	return exposuredata.OperationReceipt{ID: route.ID}, nil
}

func (provider *httpsRootTestProvider) Remove(_ context.Context, selector tailscale.RouteSelector, _ string) (exposuredata.OperationReceipt, error) {
	provider.removes = append(provider.removes, selector)
	for index, route := range provider.snapshot.Routes {
		if route.ProviderKey == selector.ID && route.Path == selector.Path && route.Backend == selector.Backend {
			provider.snapshot.Routes = append(provider.snapshot.Routes[:index], provider.snapshot.Routes[index+1:]...)
			break
		}
	}
	return exposuredata.OperationReceipt{ID: "remove-" + selector.ID}, nil
}

func httpsRootCapabilities() tailscale.Capabilities {
	return tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true, ServeTCP: true, ServeHTTPS: true, FunnelHTTPS: true, ServePath: true, FunnelPath: true}
}

func httpsRootReadiness() readiness.Readiness {
	return readiness.Readiness{Status: readiness.ReadinessReady, Modes: []readiness.ModeReadiness{
		{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady},
		{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReady, HTTPPathStatus: readiness.ReadinessReady},
	}}
}

func keyRune(r rune) tea.KeyMsg           { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyType(kind tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: kind} }

func TestRefreshPreservesDiscoveryWhenExposureProviderIsMissing(t *testing.T) {
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p0\ncweb\nn127.0.0.1:3000\n"}, nil
	})}
	controller := exposure.NewController(discoverer, nil)
	view, err := refresh(context.Background(), controller, time.Second)
	if err == nil || len(view.Listeners.Listeners) != 1 || len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureUnknown {
		t.Fatalf("missing provider erased safe local discovery: view=%#v err=%v", view, err)
	}
}

func TestWrapTextLinesRespectsViewportWidth(t *testing.T) {
	lines := wrapTextLines([]string{
		"  command: " + strings.Repeat("very-long-command-argument ", 8),
		"  reason: this adapter version has not passed an explicit compatibility probe",
	}, 28)
	if len(lines) < 4 {
		t.Fatalf("long detail content was not wrapped: %#v", lines)
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > 28 {
			t.Fatalf("wrapped line exceeds viewport: width=%d line=%q", width, line)
		}
	}

	m := workspaceFixture()
	m.view.Items[0].Listener.CommandLine = strings.Repeat("--feature-flag=very-long-value ", 12)
	for _, line := range strings.Split(m.renderDetails(32, 80), "\n") {
		if width := lipgloss.Width(line); width > 32 {
			t.Fatalf("detail line exceeds narrow pane: width=%d line=%q", width, line)
		}
	}
}

func TestDetailsGroupsRoutesAndStatusBadges(t *testing.T) {
	m := workspaceFixture()
	numericIPv6Path := exposuredata.ExposureRoute{ID: "path-app", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Target: targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	m.view.Items[0].Routes[0] = numericIPv6Path
	m.view.Items[0].Recommendation = exposuredata.NumericIPv6HTTPSBackendRecommendation
	funnel := exposuredata.ExposureRoute{ID: "funnel-app", ProviderKey: "funnel:https=3000", Target: m.view.Items[0].Routes[0].Target, Mode: exposuredata.ExposureFunnel, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = append(m.view.Exposures.Routes, funnel)
	m.view.Items[0].Routes = append(m.view.Items[0].Routes, funnel)
	m.view.Items[0].State = exposuredata.ExposureAmbiguous
	m.view.Items[0].Warning = "multiple exposure routes match this listener"
	m.readiness.Modes[1].Status = readiness.ReadinessReadOnly
	view := m.renderDetails(120, 100)
	for _, want := range []string{"[! AMBIG]", "── ALERTS ──", "── EXPOSURE ROUTES (2) ──", "[MANAGED]", "[UNKNOWN]", "── ACTION ITEMS ──", "unknown proxy destination", "--localhost-backend", "[! READ-ONLY]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("details missing %q: %s", want, view)
		}
	}
}

func TestFooterShowsBuildCommitAndLinkedRepository(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efb"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.cfg.ColorTheme = "dark"
	m.width = 120
	lines := strings.Split(m.renderBottom(), "\n")
	if len(lines) != 2 {
		t.Fatalf("footer has %d lines, want 2: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "3d16efb") || !strings.Contains(lines[0], repositoryLabel) {
		t.Fatalf("wide footer omitted build or repository identity: %q", lines[0])
	}
	if !strings.Contains(lines[0], ansi.SetHyperlink(repositoryURL)) || !strings.Contains(lines[0], ansi.ResetHyperlink()) {
		t.Fatalf("repository label is not wrapped in an OSC 8 hyperlink: %q", lines[0])
	}
	if !strings.Contains(lines[0], "\x1b[1;36m3d16efb\x1b[0m") || !strings.Contains(lines[0], "\x1b[4;34m"+repositoryLabel+"\x1b[0m") {
		t.Fatalf("build identity is missing its theme-aware TUI styling: %q", lines[0])
	}
	if width := lipgloss.Width(lines[0]); width > m.width {
		t.Fatalf("footer identity line exceeds width: got %d, want <= %d: %q", width, m.width, lines[0])
	}
	if !strings.Contains(lines[1], "↑↓ Move") || !strings.Contains(lines[1], "e/S Sort") || !strings.Contains(lines[1], "? Help") {
		t.Fatalf("build identity displaced grouped navigation hints: %q", lines[1])
	}
}

func TestFooterUsesGroupedHintsThatAdaptToWidth(t *testing.T) {
	m := workspaceFixture()
	for _, test := range []struct {
		width int
		want  []string
	}{
		{120, []string{"↑↓ Move", "Tab Focus", "v/V", "s/f/d Routes", "b HTTPS", "x Term", "e/S Sort", "w Filter", "/ Find", "z Zoom", "? Help", "q Quit"}},
		{100, []string{"↑↓ Move", "Tab Focus", "s/f/d", "e/S Sort", "w Filter", "/ Find", "z Zoom", "? Help", "q Quit"}},
		{72, []string{"↑↓ Move", "Tab Focus", "e/S Sort", "/ Find", "? Help", "q Quit"}},
		{30, []string{"o:off", "y:off", "w", "? Help"}},
	} {
		m.width = test.width
		lines := strings.Split(m.renderBottom(), "\n")
		visible := ansi.Strip(lines[1])
		for _, hint := range test.want {
			if !strings.Contains(visible, hint) {
				t.Errorf("width %d footer omitted %q: %q", test.width, hint, visible)
			}
		}
		if width := lipgloss.Width(lines[1]); width > test.width {
			t.Errorf("width %d footer hint row overflowed at %d columns: %q", test.width, width, visible)
		}
	}
	m.width, m.height, m.zoomed = 100, 30, true
	if line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[1]); !strings.Contains(line, "z Restore") {
		t.Fatalf("zoomed footer did not explain restore shortcut: %q", line)
	}
	m.width, m.height, m.zoomed, m.focus = 120, 30, false, focusDetails
	if line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[1]); !strings.Contains(line, "↑↓ Scroll") || !strings.Contains(line, "Tab List") {
		t.Fatalf("details footer did not explain scroll/focus navigation: %q", line)
	}
}

func TestFooterShowsSortModeAndLargeOperationsKeepPriority(t *testing.T) {
	m := workspaceFixture()
	if line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0]); !strings.Contains(line, "w:all") {
		t.Fatalf("footer did not show the default unfiltered mode: %q", line)
	}
	m.modeNotOffOnly = true
	if line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0]); !strings.Contains(line, "w:not-OFF") {
		t.Fatalf("footer did not show active mode filter: %q", line)
	}
	m.width, m.modeNotOffOnly = 30, true
	if line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0]); !strings.Contains(line, "w:on") {
		t.Fatalf("narrow footer did not compactly show the active mode filter: %q", line)
	}
	m.width, m.modeNotOffOnly = 120, false
	for _, test := range []struct {
		sortKey string
		label   string
	}{{"name", "Name ↑"}, {"name-desc", "Name ↓"}, {"none", "Unsorted"}} {
		m.cfg.Sort = test.sortKey
		line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0])
		if !strings.Contains(line, "Sort: "+test.label) {
			t.Errorf("sort %q missing from footer: %q", test.sortKey, line)
		}
	}

	m.width, m.cfg.Sort, m.transient = 80, "name", "Applying batch (2/4)"
	line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0])
	if !strings.Contains(line, "Applying batch (2/4)") || !strings.Contains(line, "o:off") || !strings.Contains(line, "y:off") {
		t.Fatalf("applying status obscured shortcut availability: %q", line)
	}

	m.width, m.searching, m.transient = 30, true, ""
	lines := strings.Split(m.renderBottom(), "\n")
	visible := ansi.Strip(lines[1])
	for _, hint := range []string{"Enter/Esc", "o:off", "y:off"} {
		if !strings.Contains(visible, hint) {
			t.Errorf("narrow search footer omitted %q: %q", hint, visible)
		}
	}
	if width := lipgloss.Width(lines[1]); width > m.width {
		t.Errorf("narrow search footer overflowed: %d > %d: %q", width, m.width, visible)
	}
}

func TestFooterLinkAndBuildStyleRespectNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efb"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.cfg.ColorTheme = "dark"
	line := strings.Split(m.renderBottom(), "\n")[0]
	visible := ansi.Strip(line)
	if !strings.Contains(visible, "3d16efb") || !strings.Contains(visible, repositoryLabel) {
		t.Fatalf("NO_COLOR hid build identity: visible=%q", visible)
	}
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("NO_COLOR did not disable TUI styling: %q", line)
	}
	if !strings.Contains(line, ansi.SetHyperlink(repositoryURL)) || !strings.Contains(line, ansi.ResetHyperlink()) {
		t.Fatalf("NO_COLOR disabled repository hyperlink: %q", line)
	}
}

func TestFooterPrefersCompactLinkWhenFullURLWouldClipStatus(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efb"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.width = 80
	m.transient = strings.Repeat("status", 4)
	lines := strings.Split(m.renderBottom(), "\n")
	visible := ansi.Strip(lines[0])
	if !strings.Contains(visible, m.transient) || !strings.Contains(visible, "3d16efb GitHub") {
		t.Fatalf("footer clipped status instead of compacting repository label: %q", visible)
	}
	if strings.Contains(visible, repositoryLabel) || !strings.Contains(lines[0], ansi.SetHyperlink(repositoryURL)) {
		t.Fatalf("compact repository label lost its hyperlink: %q", lines[0])
	}
	if width := lipgloss.Width(lines[0]); width > m.width {
		t.Fatalf("footer identity line exceeds width: got %d, want <= %d: %q", width, m.width, lines[0])
	}
}

func TestFooterUsesDevelopmentFallbackAndCompactRepositoryLink(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = ""
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.width = 30
	lines := strings.Split(m.renderBottom(), "\n")
	if len(lines) != 2 {
		t.Fatalf("footer has %d lines, want 2: %q", len(lines), lines)
	}
	visible := ansi.Strip(lines[0])
	if !strings.Contains(visible, "L w:all b[off]") {
		t.Fatalf("narrow footer lost compact focus/path status: %q", visible)
	}
	if !strings.Contains(visible, "dev GitHub") {
		t.Fatalf("narrow footer omitted development fallback or compact link label: %q", visible)
	}
	if strings.Contains(visible, repositoryLabel) || !strings.Contains(lines[0], ansi.SetHyperlink(repositoryURL)) || !strings.Contains(lines[0], ansi.ResetHyperlink()) {
		t.Fatalf("narrow repository link was not compactly hyperlinked: %q", lines[0])
	}
	if width := lipgloss.Width(lines[0]); width > m.width {
		t.Fatalf("narrow footer identity line exceeds width: got %d, want <= %d: %q", width, m.width, lines[0])
	}
	if !strings.Contains(lines[1], "? Help") || !strings.Contains(lines[1], "o:off") {
		t.Fatalf("narrow footer hid help or shortcut availability: %q", lines[1])
	}
}

func TestFooterShortensCommitToPreserveNarrowStatus(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efbb9058b146749de0d81aa7dd5eede3e9da"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.width = 30
	line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0])
	if !strings.Contains(line, "3d16efb GitHub") || !strings.Contains(line, "L w:all b[off]") {
		t.Fatalf("long commit obscured narrow footer status: %q", line)
	}
	if width := lipgloss.Width(line); width > m.width {
		t.Fatalf("narrow footer exceeds width: got %d, want <= %d: %q", width, m.width, line)
	}
}

func TestFooterKeepsApplyingStatusAtMinimumWidth(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efb"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	m := workspaceFixture()
	m.width = 30
	m.transient = "Applying batch (2/4)"
	line := ansi.Strip(strings.Split(m.renderBottom(), "\n")[0])
	if !strings.Contains(line, "Applying") || !strings.Contains(line, "3d16efb GitHub") {
		t.Fatalf("minimum-width footer hid active operation or build identity: %q", line)
	}
	if width := lipgloss.Width(line); width > m.width {
		t.Fatalf("minimum-width footer exceeds width: got %d, want <= %d: %q", width, m.width, line)
	}
}

func TestWorkspaceResponsiveLayoutAndFocusNavigation(t *testing.T) {
	m := workspaceFixture()
	m.selectedID = m.view.Items[0].ID
	wide := m.View()
	if !strings.Contains(wide, "SERVICE LIST") || strings.Contains(wide, "SERVICE LIST · FOCUSED") || !strings.Contains(wide, "LOCAL LISTENERS") || !strings.Contains(wide, "SERVICE") || !strings.Contains(wide, "TARGET") || !strings.Contains(wide, "MODE") || !strings.Contains(wide, "STATUS") || !strings.Contains(wide, "> ") || !strings.Contains(wide, "DETAILS") || !strings.Contains(wide, "READINESS") || !strings.Contains(wide, "Focus: List") {
		t.Fatalf("wide workspace missing panels/chrome: %q", wide)
	}
	m.Update(keyType(tea.KeyTab))
	if m.focus != focusDetails || !strings.Contains(m.View(), "DETAILS") || strings.Contains(m.View(), "DETAILS · FOCUSED") {
		t.Fatalf("Tab did not focus details without redundant focus label: focus=%v", m.focus)
	}
	m.Update(keyType(tea.KeyEsc))
	if m.focus != focusList {
		t.Fatalf("Esc did not return to list")
	}
	m.width, m.height = 99, 23
	narrow := m.View()
	if strings.Contains(narrow, "DETAILS · FOCUSED") || !strings.Contains(narrow, "SERVICE LIST") {
		t.Fatalf("narrow layout did not collapse: %q", narrow)
	}
	m.width, m.height = 29, 7
	if !strings.Contains(m.View(), "Terminal too small") {
		t.Fatalf("small terminal lacked resize notice")
	}
	m.modal = modalAction
	if strings.Contains(m.View(), "EXPOSURE ACTION") {
		t.Fatal("modal rendered over the compact-terminal resize notice")
	}
	m.width, m.height = 100, 11
	if !strings.Contains(m.View(), "Terminal too small for this dialog") {
		t.Fatal("short terminal rendered a clipped dialog")
	}
}

func TestHelpDocumentsShortcutGroups(t *testing.T) {
	help := strings.Join(helpLines(), "\n")
	for _, text := range []string{
		"WORKSPACE / NAVIGATION", "SEARCH / FILTER", "ACTION PREVIEW", "CONFIRMATION / APPLYING", "COMMAND PALETTE", "HELP", "SAFETY / STATE",
		"C or Ctrl-l", "U                    clear all selected items", "s                    preview private Serve", "f                    preview public Funnel", "d                    preview Disable", "b                    preview a private HTTPS root", "HTTPS root           replaces only the exact conflicting Serve TCP route", "choose among exact paths", "Ctrl-d/Page Down", "Ctrl-u/Page Up", "Home/End", "Funnel               remains public", "z                    zoom the focused pane", "e/S                  cycle Name ↑, Name ↓, and unsorted order", "w                    toggle mode filter", "CPU% / MEM",
	} {
		if !strings.Contains(help, text) {
			t.Fatalf("help missing %q:\n%s", text, help)
		}
	}
}

func TestOverlayPreservesBackgroundBesideModal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	got := overlay("01234567890123456789", "BOX", 20, 1)
	if got != "01234567BOX123456789" {
		t.Fatalf("modal overlay discarded background content: %q", got)
	}
}

func TestWorkspaceReadinessReasonIsVisible(t *testing.T) {
	m := workspaceFixture()
	m.readiness.Modes = []readiness.ModeReadiness{{
		Mode:   exposuredata.ExposureServe,
		Status: readiness.ReadinessReadOnly,
		Checks: []readiness.ReadinessCheck{{Name: "compatibility probe", Status: readiness.ReadinessReadOnly, Message: "probe evidence is missing", Remediation: "Run the disposable compatibility probe."}},
	}}
	view := m.View()
	if !strings.Contains(view, "serve: read_only") || !strings.Contains(view, "reason: probe evidence is missing") || !strings.Contains(view, "next: Run the disposable compatibility probe.") {
		t.Fatalf("readiness reason was not visible in details: %q", view)
	}
}

func TestDetailLabelsServeAndFunnelReadinessAndOperationOwners(t *testing.T) {
	m := workspaceFixture()
	m.height = 80
	target, _ := itemTarget(m.view.Items[0])
	m.readiness.Modes = []readiness.ModeReadiness{
		{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady, Checks: []readiness.ReadinessCheck{{Status: readiness.ReadinessReady}}},
		{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReadOnly, Checks: []readiness.ReadinessCheck{{Status: readiness.ReadinessReadOnly, Message: "public probe is missing", Remediation: "Run the Funnel compatibility probe."}}},
	}
	m.view.Events = []exposuredata.OperationEvent{{Target: target, Mode: exposuredata.ExposureFunnel, Phase: "final", State: exposuredata.ExposureFailed}}
	m.view.Items[0].DesiredMode = exposuredata.ExposureFunnel
	m.view.Items[0].OperationState = exposuredata.ExposureFailed
	m.view.Items[0].LastOperation = &exposuredata.OperationReceipt{Error: &fault.SafeError{Message: "Funnel apply failed", Remediation: "Review Funnel status."}}
	view := m.View()
	for _, text := range []string{"serve: ready", "funnel: read_only", "[owner: SERVE]", "[owner: FUNNEL]", "SERVE operation: idle", "FUNNEL operation: failed", "FUNNEL next: press R", "FUNNEL issue: Funnel apply failed"} {
		if !strings.Contains(view, text) {
			t.Fatalf("detail panel missing %q: %q", text, view)
		}
	}
}

func TestWorkspaceClearFilterShortcut(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRune('C'), keyType(tea.KeyCtrlL)} {
		m := workspaceFixture()
		m.query = "does-not-match"
		m.selectedID = ""
		m.selectedIdx = 0
		m.reselect("", 0)
		m.Update(key)
		if m.query != "" || m.searching || m.searchInput.Value() != "" {
			t.Fatalf("key %q did not clear accepted filter: query=%q searching=%t input=%q", key.String(), m.query, m.searching, m.searchInput.Value())
		}
		if m.selectedID == "" || !strings.Contains(m.View(), "web") {
			t.Fatalf("key %q did not restore the unfiltered selection: selected=%q view=%q", key.String(), m.selectedID, m.View())
		}
	}
}

func TestWorkspaceSearchIsIncrementalAndCancelsSafely(t *testing.T) {
	m := workspaceFixture()
	m.Update(keyRune('/'))
	if !m.searching {
		t.Fatal("slash did not enter search")
	}
	m.Update(keyRune('w'))
	if m.query != "w" {
		t.Fatalf("query=%q", m.query)
	}
	m.Update(keyType(tea.KeyEsc))
	if m.searching || m.query != "" {
		t.Fatalf("Esc failed to cancel search: searching=%t query=%q", m.searching, m.query)
	}
	m.Update(keyRune('/'))
	m.Update(keyRune('z'))
	m.Update(keyType(tea.KeyCtrlU))
	if m.query != "" || m.searchInput.Value() != "" {
		t.Fatalf("Ctrl-u did not clear search")
	}
	m.Update(keyType(tea.KeyEnter))
	if m.searching {
		t.Fatal("Enter did not accept search")
	}
}

func TestWorkspaceDocumentedGNavigation(t *testing.T) {
	m := workspaceFixture()
	second := discovery.Listener{ID: "listener-two", Name: "api", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.reselect("", 0)
	firstID := m.selectedID
	lastID := m.items()[len(m.items())-1].ID
	if firstID == lastID {
		t.Fatal("fixture requires two distinct visible items")
	}
	m.Update(keyRune('G'))
	if m.selectedID != lastID {
		t.Fatalf("G did not select last item: got %q want %q", m.selectedID, lastID)
	}
	m.Update(keyRune('g'))
	m.Update(keyRune('g'))
	if m.selectedID != firstID {
		t.Fatalf("gg did not select first item: got %q want %q", m.selectedID, firstID)
	}
}

func TestWorkspacePendingGRejectsStaleTimeout(t *testing.T) {
	m := workspaceFixture()
	m.Update(keyRune('g'))
	stale := m.gGeneration
	m.Update(keyType(tea.KeyEsc))
	m.Update(keyRune('g'))
	if m.gGeneration == 0 || m.gGeneration == stale {
		t.Fatalf("new pending g reused prior timer generation: prior=%d current=%d", stale, m.gGeneration)
	}
	m.Update(gTimeoutMsg{generation: stale})
	if m.gGeneration == 0 {
		t.Fatal("stale timer cancelled a newer pending g")
	}
}

func TestWorkspaceDetailNavigationInterruptsPendingG(t *testing.T) {
	m := workspaceFixture()
	m.focus = focusDetails
	m.Update(keyRune('g'))
	m.Update(keyRune('j'))
	if m.gGeneration != 0 {
		t.Fatalf("detail scrolling left g pending: generation=%d", m.gGeneration)
	}
}

func TestWorkspaceEscCancelsPendingGFromDetails(t *testing.T) {
	m := workspaceFixture()
	m.focus = focusDetails
	m.Update(keyRune('g'))
	m.Update(keyType(tea.KeyEsc))
	if m.focus != focusList || m.gGeneration != 0 {
		t.Fatalf("Esc did not return to list and cancel pending g: focus=%v pending=%d", m.focus, m.gGeneration)
	}
}

func TestWorkspaceDetailAndHelpScrollingAreBounded(t *testing.T) {
	m := workspaceFixture()
	m.focus = focusDetails
	before := m.detailOffset
	m.Update(keyRune('j'))
	if m.detailOffset != before+1 {
		t.Fatalf("detail j did not scroll: before=%d after=%d", before, m.detailOffset)
	}
	m.Update(keyType(tea.KeyCtrlU))
	if m.detailOffset != 0 {
		t.Fatalf("detail Ctrl-u did not page up: offset=%d", m.detailOffset)
	}
	m.modal = modalHelp
	m.height = 12
	m.Update(keyRune('j'))
	if m.helpOffset <= 0 {
		t.Fatal("help j did not scroll")
	}
	m.Update(keyType(tea.KeyEsc))
	if m.modal != modalNone {
		t.Fatal("Esc did not close help")
	}
}

func TestHelpModalSupportsLineAndPageScrolling(t *testing.T) {
	m := workspaceFixture()
	m.modal = modalHelp
	m.height = 20
	page := m.modalPage()
	m.Update(keyType(tea.KeyEnd))
	if m.helpOffset != m.helpMaxOffset() {
		t.Fatalf("End did not reach help bottom: got=%d want=%d", m.helpOffset, m.helpMaxOffset())
	}
	m.Update(keyType(tea.KeyHome))
	if m.helpOffset != 0 {
		t.Fatalf("Home did not reach help top: offset=%d", m.helpOffset)
	}
	m.Update(keyRune('j'))
	if m.helpOffset != 1 {
		t.Fatalf("j did not move one help line: offset=%d", m.helpOffset)
	}
	m.Update(keyType(tea.KeyPgDown))
	if m.helpOffset != page+1 {
		t.Fatalf("PageDown did not advance one page: offset=%d page=%d", m.helpOffset, page)
	}
	m.Update(keyType(tea.KeyPgUp))
	if m.helpOffset != 1 {
		t.Fatalf("PageUp did not return one page: offset=%d", m.helpOffset)
	}
}

func TestWorkspacePreservesStableSelectionAcrossRefresh(t *testing.T) {
	m := workspaceFixture()
	second := discovery.Listener{ID: "listener-two", Name: "api", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.reselect("listener-two", 1)
	m.startRefresh()
	replacement := second
	replacement.Name = "api-renamed"
	view := m.view
	view.Items = []exposure.ReconciledItem{{ID: replacement.ID, Listener: &replacement, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled}, view.Items[0]}
	m.Update(viewLoadedMsg{seq: m.refreshState.sequence(), view: view})
	if m.selectedID != "listener-two" {
		t.Fatalf("selection was not preserved: %q", m.selectedID)
	}
}

func TestWorkspaceRefreshClearsStaleUnavailableURLBannerWhenObservedURLAppears(t *testing.T) {
	m := workspaceFixture()
	m.banner, m.bannerSticky = unavailableOpenURLBanner+" | existing warning", true
	m.view.Items[0].Routes[0].URL = ""
	m.view.Exposures.Routes[0].URL = ""
	m.startRefresh()
	refreshed := m.view
	refreshed.Items = append([]exposure.ReconciledItem(nil), refreshed.Items...)
	refreshed.Items[0].Routes = append([]exposuredata.ExposureRoute(nil), refreshed.Items[0].Routes...)
	refreshed.Items[0].Routes[0].URL = "https://dev.example.ts.net/node-4321"
	refreshed.Exposures.Routes = append([]exposuredata.ExposureRoute(nil), refreshed.Exposures.Routes...)
	refreshed.Exposures.Routes[0].URL = "https://dev.example.ts.net/node-4321"
	m.Update(viewLoadedMsg{seq: m.refreshState.sequence(), view: refreshed})
	if m.banner != "existing warning" || !m.bannerSticky {
		t.Fatalf("recovery did not clear only the stale URL notice: banner=%q sticky=%t", m.banner, m.bannerSticky)
	}
}

func TestWorkspaceTopBarReportsListenerAndExposureFreshnessSeparately(t *testing.T) {
	m := workspaceFixture()
	m.viewErr = errors.New("exposure state is incomplete")
	m.view.Listeners.Authoritative = true
	m.view.Listeners.Stale = false
	m.view.Exposures.Authoritative = false
	m.view.Exposures.Stale = true
	top := m.renderTop()
	if !strings.Contains(top, "listeners:ready") || !strings.Contains(top, "exposure:stale/unavailable") {
		t.Fatalf("top bar conflated independent source freshness: %q", top)
	}
}

func TestWorkspaceRefreshKeepsLastViewVisible(t *testing.T) {
	m := workspaceFixture()
	before := m.View()
	if m.startRefresh() == nil || !m.refreshState.isPending() {
		t.Fatal("refresh did not start")
	}
	during := m.View()
	if !strings.Contains(during, "web") || strings.Contains(during, "Loading listeners") || strings.Count(during, "Refreshing") != 1 {
		t.Fatalf("refresh hid the cached workspace or duplicated its indicator: before=%q during=%q", before, during)
	}

	m.Update(viewLoadedMsg{seq: m.refreshState.sequence(), view: exposure.View{}, err: errors.New("temporary discovery failure")})
	afterFailure := m.View()
	if !strings.Contains(afterFailure, "web") || !strings.Contains(afterFailure, "temporary discovery failure") || !strings.Contains(afterFailure, "listeners:stale") {
		t.Fatalf("failed refresh did not preserve cached view and stale warning: %q", afterFailure)
	}
	if !m.hasView {
		t.Fatal("failed refresh discarded the cached view")
	}
	availability := m.actionAvailability(exposuredata.ExposureFunnel)
	if !availability.Disabled || (!strings.Contains(availability.Reason, "stale") && !strings.Contains(availability.Reason, "refresh")) {
		t.Fatalf("stale refresh did not block unsafe mutation: availability=%#v", availability)
	}
}

func TestWorkspaceClosesPreviewWhenRouteFingerprintChanges(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	changed := m.view
	changed.Exposures.Routes = append([]exposuredata.ExposureRoute(nil), changed.Exposures.Routes...)
	changed.Exposures.Routes[0].Target.Port = 3001
	changed.Items = append([]exposure.ReconciledItem(nil), changed.Items...)
	changed.Items[0].Routes = append([]exposuredata.ExposureRoute(nil), changed.Items[0].Routes...)
	changed.Items[0].Routes[0].Target.Port = 3001
	m.startRefresh()
	m.Update(viewLoadedMsg{seq: m.refreshState.sequence(), view: changed})
	if m.modal != modalNone || !strings.Contains(m.banner, "Selection changed") {
		t.Fatalf("route fingerprint change left stale preview: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestWorkspaceActionModalKeepsHeightDuringRefreshStateChanges(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	before := lipgloss.Height(m.modalView())
	m.refreshState.pending = true
	during := lipgloss.Height(m.modalView())
	m.refreshState.pending = false
	m.readyErr = errors.New("readiness temporarily unavailable")
	after := lipgloss.Height(m.modalView())
	if before != during || before != after {
		t.Fatalf("action modal shifted as refresh state changed: before=%d during=%d after=%d", before, during, after)
	}
}

func TestWorkspaceRefreshDoesNotPaintTransientActionWarning(t *testing.T) {
	m := workspaceFixture()
	m.refreshState.pending = true
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	view := m.View()
	if strings.Contains(view, "refresh is in progress") {
		t.Fatalf("refresh warning leaked into action choices: %q", view)
	}
	if strings.Contains(view, "Serve (s, tailnet only) [unavailable]") || strings.Contains(view, "Funnel (f, public internet) [unavailable]") {
		t.Fatalf("refresh-waiting actions were presented as unavailable: %q", view)
	}
	availability := m.actionAvailability(exposuredata.ExposureFunnel)
	if !availability.Disabled || !strings.Contains(availability.Reason, "refresh is in progress") {
		t.Fatalf("refresh did not continue blocking unsafe action: availability=%#v", availability)
	}
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalAction {
		t.Fatalf("refresh-blocked action preview closed instead of waiting: modal=%v", m.modal)
	}
}

func TestWorkspaceActionModalLabelsGenuineUnavailableChoice(t *testing.T) {
	m := workspaceFixture()
	m.readiness.Modes[1].Status = readiness.ReadinessReadOnly
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	if view := m.View(); !strings.Contains(view, "Funnel (f, public internet) [unavailable]") {
		t.Fatalf("genuinely unavailable action lost its label: %q", view)
	}
}

func TestExposureActionModalUsesWorkspaceSuppliedAvailability(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	if m.startRefresh() == nil || !m.refreshState.isPending() {
		t.Fatal("refresh did not start")
	}
	if len(m.actionSession.choices) != 3 || !m.actionSession.choices[2].disabled || !m.actionSession.choices[2].wait {
		t.Fatalf("workspace did not supply refresh-blocked action choice: %#v", m.actionSession.choices)
	}

	// The modal consumes its supplied choice state; it must not inspect the
	// workspace refresh flag itself. The workspace republishes choices when the
	// refresh lifecycle advances.
	m.Update(viewLoadedMsg{seq: m.refreshState.sequence(), view: m.view})
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalAction {
		t.Fatalf("modal bypassed supplied waiting state: modal=%v", m.modal)
	}
	m.Update(readinessLoadedMsg{seq: m.refreshState.sequence(), data: m.readiness})
	if m.refreshState.isPending() || m.actionSession.choices[2].disabled {
		t.Fatalf("workspace did not refresh modal choices: pending=%t choices=%#v", m.refreshState.isPending(), m.actionSession.choices)
	}
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm {
		t.Fatalf("refreshed action choice did not open confirmation: modal=%v", m.modal)
	}
}

func TestWorkspaceBlocksMutationWhenListenerSnapshotIsStale(t *testing.T) {
	m := workspaceFixture()
	m.view.Listeners.Stale = true
	availability := m.actionAvailability(exposuredata.ExposureFunnel)
	if !availability.Disabled || !strings.Contains(availability.Reason, "listener/exposure state is stale") {
		t.Fatalf("stale listener snapshot enabled mutation: %#v", availability)
	}
}

func TestWorkspaceConfirmationDefersToRefresh(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyType(tea.KeyTab))
	if m.modal != modalConfirm || !m.actionSession.confirm {
		t.Fatalf("confirmation was not focused: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	if m.startRefresh() == nil || !m.refreshState.isPending() {
		t.Fatal("refresh did not start")
	}
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalAction || len(m.activeOps) != 0 {
		t.Fatalf("confirmation bypassed refresh guard: modal=%v operations=%d", m.modal, len(m.activeOps))
	}
}

func TestWorkspaceConfirmationRemainsVisibleUntilOperatorActsDuringRefresh(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyType(tea.KeyTab))
	if m.modal != modalConfirm || !m.actionSession.confirm {
		t.Fatalf("confirmation was not focused: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	if m.startRefresh() == nil || !m.refreshState.isPending() {
		t.Fatal("refresh did not start")
	}
	changed := m.view
	changed.Exposures.Routes = append([]exposuredata.ExposureRoute(nil), changed.Exposures.Routes...)
	changed.Exposures.Routes[0].URL = "https://updated.example.ts.net"
	seq := m.refreshState.sequence()
	m.Update(viewLoadedMsg{seq: seq, view: changed})
	m.Update(readinessLoadedMsg{seq: seq, data: m.readiness})
	if m.modal != modalConfirm || !m.actionSession.confirm {
		t.Fatalf("refresh closed confirmation without operator input: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	m.Update(keyType(tea.KeyEnter))
	if len(m.activeOps) != 0 || !strings.Contains(m.banner, "Selection changed") {
		t.Fatalf("stale confirmation bypassed preview recheck: operations=%d banner=%q", len(m.activeOps), m.banner)
	}
}

func TestWorkspaceConfirmationRechecksReadinessBeforeMutation(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyType(tea.KeyTab))
	if m.modal != modalConfirm || !m.actionSession.confirm {
		t.Fatalf("confirmation was not focused: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	m.readiness.Modes[1].Status = readiness.ReadinessReadOnly
	m.readyErr = errors.New("readiness changed while confirming")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalAction || len(m.activeOps) != 0 || !strings.Contains(m.banner, "readiness state is stale") {
		t.Fatalf("confirmation bypassed a changed readiness guard: modal=%v operations=%d banner=%q", m.modal, len(m.activeOps), m.banner)
	}
}

func TestWorkspaceDirectActionKeysOpenPreview(t *testing.T) {
	for _, test := range []struct {
		key  rune
		mode exposuredata.ExposureMode
	}{

		{key: 's', mode: exposuredata.ExposureServe},
		{key: 'f', mode: exposuredata.ExposureFunnel},
		{key: 'd', mode: exposuredata.ExposureDisabled},
	} {
		m := workspaceFixture()
		m.Update(keyRune(test.key))
		if m.modal != modalAction || m.actionSession.mode != test.mode || m.actionSession.index != modeIndex(test.mode) || m.actionSession.itemID == "" {
			t.Fatalf("key %q did not open the expected action preview: modal=%v mode=%q index=%d item=%q", test.key, m.modal, m.actionSession.mode, m.actionSession.index, m.actionSession.itemID)
		}
		if view := m.View(); !strings.Contains(view, "EXPOSURE ACTION") {
			t.Fatalf("key %q opened action state but did not render the modal: %q", test.key, view)
		}
	}
}

func TestWorkspaceActionModalKeysChooseWithoutArrowAndEnter(t *testing.T) {
	for _, test := range []struct {
		key  rune
		mode exposuredata.ExposureMode
	}{
		{key: 'd', mode: exposuredata.ExposureDisabled},
		{key: 's', mode: exposuredata.ExposureServe},
		{key: 'f', mode: exposuredata.ExposureFunnel},
	} {
		t.Run(string(test.key), func(t *testing.T) {
			m := workspaceFixture()
			if test.mode != exposuredata.ExposureDisabled {
				m.view.Items[0].Routes = nil
				m.view.Items[0].State = exposuredata.ExposureState("disabled")
				m.view.Items[0].Mode = exposuredata.ExposureDisabled
				m.view.Exposures.Routes = nil
			}
			m.Update(keyRune(' '))
			if m.modal != modalAction {
				t.Fatalf("space did not open action selector: modal=%v", m.modal)
			}
			view := m.View()
			for _, label := range []string{"Disabled (d)", "Serve (s, tailnet only)", "Funnel (f, public internet)", "d/s/f choose"} {
				if !strings.Contains(view, label) {
					t.Fatalf("action selector omitted shortcut %q: %q", label, view)
				}
			}

			m.Update(keyRune(test.key))
			if m.modal != modalConfirm || m.actionSession.mode != test.mode {
				t.Fatalf("shortcut %q did not advance to the matching confirmation: modal=%v mode=%q", test.key, m.modal, m.actionSession.mode)
			}
			if m.actionSession.confirm || len(m.activeOps) != 0 {
				t.Fatalf("shortcut %q bypassed the Cancel-focused confirmation: confirm=%t active=%d", test.key, m.actionSession.confirm, len(m.activeOps))
			}
			if test.mode == exposuredata.ExposureFunnel && !strings.Contains(m.View(), "WARNING: public internet exposure") {
				t.Fatalf("Funnel shortcut omitted public exposure warning: %q", m.View())
			}
			if test.mode == exposuredata.ExposureDisabled && (m.actionSession.routeID != "route-app" || m.actionSession.routeKey != "tcp:3000") {
				t.Fatalf("Disable shortcut lost exact route identity: routeID=%q selector=%q", m.actionSession.routeID, m.actionSession.routeKey)
			}
		})
	}
}

func TestWorkspaceActionModalShortcutRespectsUnavailableAndWaitingChoices(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		m := workspaceFixture()
		m.readiness.Modes[1].Status = readiness.ReadinessReadOnly
		m.Update(keyRune(' '))
		reason := m.actionSession.choices[2].reason
		m.Update(keyRune('f'))
		if m.modal != modalNone || m.banner != reason || len(m.activeOps) != 0 {
			t.Fatalf("Funnel shortcut bypassed its unavailable state: modal=%v banner=%q active=%d wantReason=%q", m.modal, m.banner, len(m.activeOps), reason)
		}
	})

	t.Run("waiting for refresh", func(t *testing.T) {
		m := workspaceFixture()
		m.refreshState.pending = true
		m.Update(keyRune(' '))
		m.Update(keyRune('f'))
		if m.modal != modalAction || len(m.activeOps) != 0 {
			t.Fatalf("Funnel shortcut bypassed its refresh wait: modal=%v active=%d", m.modal, len(m.activeOps))
		}
	})
}

func TestWorkspaceDisableChoosesOneExactRoute(t *testing.T) {
	m := workspaceFixture()
	funnel := exposuredata.ExposureRoute{ID: "funnel-app", ProviderKey: "funnel:https=3000", Target: m.view.Items[0].Routes[0].Target, Mode: exposuredata.ExposureFunnel, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = append(m.view.Exposures.Routes, funnel)
	m.view.Items[0].Routes = append(m.view.Items[0].Routes, funnel)
	m.view.Items[0].State = exposuredata.ExposureActive
	if view := m.View(); !strings.Contains(view, "mode: multiple (choose exact route)") {
		t.Fatalf("ambiguous route state was presented as a single mode: %q", view)
	}
	m.Update(keyRune('d'))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalDisableRoute || m.disableRouteIndex != 0 {
		t.Fatalf("disable did not open exact-route picker: modal=%v index=%d", m.modal, m.disableRouteIndex)
	}
	m.Update(keyType(tea.KeyDown))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.routeKey != "funnel:https=3000" || m.actionSession.routeID != funnel.ID || m.actionSession.confirm {
		t.Fatalf("selected route was not carried into focused confirmation: modal=%v route=%q focus=%t", m.modal, m.actionSession.routeKey, m.actionSession.confirm)
	}
	if view := m.View(); !strings.Contains(view, "funnel:https=3000") || !strings.Contains(view, "external/unknown") {
		t.Fatalf("confirmation did not show selected route and ownership warning: %q", view)
	}
}

func TestWorkspaceKnownMultipleRoutesShowsActiveAndUsesExactRouteChooser(t *testing.T) {
	m := workspaceFixture()
	listener := *m.view.Items[0].Listener
	listener.Target = targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	backendTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	listeners := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{listener}}
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{
		{ID: "root-route", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "blog-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
	}}
	m.view = exposure.Reconcile(listeners, routes, nil)

	view := m.View()
	if !strings.Contains(view, "[● ACTIVE]") || strings.Contains(view, "[! AMBIG]") || !strings.Contains(view, "mode: multiple (choose exact route)") {
		t.Fatalf("known exact routes were not presented as active/multiple: %q", view)
	}
	if strings.Contains(view, "multiple exposure routes match this listener") {
		t.Fatalf("known exact routes still raised the ambiguous-state alert: %q", view)
	}

	m.Update(keyRune('d'))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalDisableRoute || m.disableRouteIndex != 0 {
		t.Fatalf("active multiple routes did not open exact-route chooser: modal=%v index=%d", m.modal, m.disableRouteIndex)
	}
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.routeID != "root-route" || m.actionSession.routeKey != "serve:https=4321" {
		t.Fatalf("exact root identity was not carried into confirmation: modal=%v route=%q selector=%q", m.modal, m.actionSession.routeID, m.actionSession.routeKey)
	}
}

func TestWorkspaceDisableDistinguishesHandlersSharingOneSelector(t *testing.T) {
	m := workspaceFixture()
	first := exposuredata.ExposureRoute{ID: "api-docs", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", URL: "https://dev.example.ts.net/docs", Target: m.view.Items[0].Listener.Target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Path, second.URL = "api-app", "/api", "https://dev.example.ts.net/api"
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Items[0].State = exposuredata.ExposureActive
	m.Update(keyRune('d'))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyType(tea.KeyDown))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.routeKey != first.ProviderKey || m.actionSession.routeID != second.ID {
		t.Fatalf("exact selected path was lost behind shared selector: modal=%v selector=%q routeID=%q", m.modal, m.actionSession.routeKey, m.actionSession.routeID)
	}
	if view := m.View(); !strings.Contains(view, "Current path: /api") || !strings.Contains(view, "https://dev.example.ts.net/api") {
		t.Fatalf("route confirmation omitted the selected path identity: %q", view)
	}
}

func TestWorkspaceAmbiguousDuplicateRoutesCannotOpenDisableChooser(t *testing.T) {
	m := workspaceFixture()
	first := exposuredata.ExposureRoute{ID: "route-one", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: m.view.Items[0].Listener.Target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Backend = "route-two", "http://localhost:4321"
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Items[0].State = exposuredata.ExposureAmbiguous
	m.Update(keyRune('d'))
	m.Update(keyType(tea.KeyEnter))
	if m.modal == modalDisableRoute || m.modal == modalConfirm || !strings.Contains(m.banner, "not authoritative or exact") {
		t.Fatalf("ambiguous exact-selector collision reached route removal: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestBrowserHTTPSRootShortcutPreviewsAndConvertsExactRawTCPRoute(t *testing.T) {
	m := workspaceFixture()
	target := m.view.Items[0].Listener.Target.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-3000", ProviderKey: "serve:tcp=3000", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:3000", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureActive, exposuredata.ExposureServe
	provider := &httpsRootTestProvider{snapshot: m.view.Exposures, caps: httpsRootCapabilities(), readiness: httpsRootReadiness()}
	m.controller = exposure.NewController(&httpsRootTestListener{snapshot: m.view.Listeners}, provider)
	m.Update(keyRune('b'))
	if m.modal != modalConfirm || m.actionSession.confirm {
		t.Fatalf("browser HTTPS shortcut did not open a Cancel-first preview: modal=%v confirm=%t", m.modal, m.actionSession.confirm)
	}
	preview := m.View()
	for _, want := range []string{"https://<Tailscale DNS>:3000/", "Provider selector: serve:https=3000", "Local HTTP backend: http://127.0.0.1:3000", "Replacing exact raw-TCP route: serve:tcp=3000", "ownership=unknown", "[Cancel]"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("same-port HTTPS preview omitted %q: %s", want, preview)
		}
	}
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("default Cancel changed the exact route: modal=%v removes=%#v sets=%#v", m.modal, provider.removes, provider.sets)
	}
	m.Update(keyRune('b'))
	m.Update(keyType(tea.KeyTab))
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("confirmed private HTTPS-root conversion did not invoke the controller")
	}
	result, ok := cmd().(operationDoneMsg)
	if !ok || result.err != nil || !result.receipt.Verified {
		t.Fatalf("same-port conversion was not verified: message=%#v", result)
	}
	if len(provider.removes) != 1 || provider.removes[0].ID != raw.ProviderKey || len(provider.sets) != 1 || !provider.sets[0].HTTPSRoot || provider.sets[0].ProviderKey != "serve:https=3000" || provider.sets[0].Backend != "http://127.0.0.1:3000" {
		t.Fatalf("TUI did not replace only the exact raw route with private HTTPS: removes=%#v sets=%#v", provider.removes, provider.sets)
	}
	if len(provider.snapshot.Routes) != 1 || provider.snapshot.Routes[0].Kind != exposuredata.RouteKindHTTPSRoot || provider.snapshot.Routes[0].URL != "https://dev.example.ts.net:3000/" {
		t.Fatalf("TUI root route was not verified on the exact port: %#v", provider.snapshot.Routes)
	}
	m.Update(keyRune('p'))
	if m.modal != modalNone || m.httpsRootAction {
		t.Fatalf("obsolete p named-path shortcut is still active: modal=%v rootAction=%t", m.modal, m.httpsRootAction)
	}
}

func TestHTTPSRootShortcutStatusTracksReadiness(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes = nil
	m.view.Exposures.Routes = nil
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureState("disabled"), exposuredata.ExposureDisabled
	if status := m.httpsRootStatusLabel(); status != "b HTTPS root[ok]" {
		t.Fatalf("ready explicit root shortcut status = %q", status)
	}
	second := m.view.Items[0]
	second.ID = "listener-two"
	second.Listener = &discovery.Listener{ID: "listener-two", Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}}
	m.view.Items = append(m.view.Items, second)
	m.selectedItems = map[string]bool{m.view.Items[0].ID: true, second.ID: true}
	if status := m.httpsRootStatusLabel(); status != "b HTTPS root[off]" {
		t.Fatalf("multi-listener selection advertised a single-listener root action: %q", status)
	}
	m.selectedItems = nil
	m.readiness.Modes[0].HTTPPathStatus = readiness.ReadinessReadOnly
	m.readiness.Modes[0].HTTPPathMessage = "--set-path is unavailable"
	m.readiness.Modes[0].HTTPPathRemediation = "Upgrade Tailscale"
	if status := m.httpsRootStatusLabel(); status != "b HTTPS root[off]" {
		t.Fatalf("unsupported exact HTTPS-root capability was advertised as available: %q", status)
	}
}

func TestHTTPSRootShortcutRejectsConflictingRootBackend(t *testing.T) {
	m := workspaceFixture()
	target := m.view.Items[0].Listener.Target.Normalized()
	otherBackend := targetmodel.Target{Address: "127.0.0.2", Port: target.Port, Protocol: "tcp"}.Normalized()
	conflict := exposuredata.ExposureRoute{ID: "other-root", ProviderKey: "serve:https=3000", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://127.0.0.2:3000", Target: otherBackend, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{conflict}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{conflict}
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureActive, exposuredata.ExposureServe
	m.Update(keyRune('b'))
	if m.modal != modalNone || !strings.Contains(m.banner, "different backend identity") {
		t.Fatalf("conflicting root handler was not rejected before confirmation: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestHTTPSRootShortcutRejectsRawRouteWithMismatchedBackend(t *testing.T) {
	m := workspaceFixture()
	target := m.view.Items[0].Listener.Target.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-3000", ProviderKey: "serve:tcp=3000", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.2:3000", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureActive, exposuredata.ExposureServe
	m.Update(keyRune('b'))
	if m.modal != modalNone || !strings.Contains(m.banner, "does not match the exact local listener backend") {
		t.Fatalf("mismatched raw backend reached the root confirmation: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestHTTPSRootPreviewInvalidatesWhenConfirmedIdentityChanges(t *testing.T) {
	m := workspaceFixture()
	target := m.view.Items[0].Listener.Target.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-3000", ProviderKey: "serve:tcp=3000", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:3000", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{raw}
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureActive, exposuredata.ExposureServe
	m.Update(keyRune('b'))
	if m.modal != modalConfirm || !m.httpsRootAction {
		t.Fatal("HTTPS-root preview did not open")
	}
	m.view.Listeners.Listeners[0].ID = "changed-listener"
	m.invalidatePreviewIfChanged()
	if m.modal != modalNone || m.httpsRootAction || !strings.Contains(m.banner, "Selection changed") {
		t.Fatalf("stale HTTPS-root intent survived listener identity change: modal=%v action=%t banner=%q", m.modal, m.httpsRootAction, m.banner)
	}
}

func TestHTTPSRootTUIExplicitlyOptsIntoIPv6LocalhostBackend(t *testing.T) {
	m := workspaceFixture()
	listener := *m.view.Items[0].Listener
	listener.Target = targetmodel.Target{Address: "::1", Port: 3000, Protocol: "tcp"}.Normalized()
	m.view.Listeners.Listeners = []discovery.Listener{listener}
	m.view.Items[0].Listener = &listener
	m.view.Items[0].State, m.view.Items[0].Mode = exposuredata.ExposureState("disabled"), exposuredata.ExposureDisabled
	m.view.Items[0].Routes = nil
	m.view.Exposures.Routes = nil
	provider := &httpsRootTestProvider{snapshot: m.view.Exposures, caps: httpsRootCapabilities(), readiness: httpsRootReadiness()}
	m.controller = exposure.NewController(&httpsRootTestListener{snapshot: m.view.Listeners}, provider)
	m.Update(keyRune('b'))
	if m.modal != modalConfirm || m.actionSession.confirm {
		t.Fatalf("IPv6 root preview was not Cancel-first: modal=%v confirm=%t", m.modal, m.actionSession.confirm)
	}
	m.Update(keyType(tea.KeyCtrlB))
	for _, want := range []string{"Local HTTP backend: http://localhost:3000", "hostname resolution weakens the exact IPv6 address guarantee"} {
		if !strings.Contains(m.View(), want) {
			t.Fatalf("explicit IPv6 alias preview omitted %q: %s", want, m.View())
		}
	}
	m.Update(keyType(tea.KeyTab))
	_, command := m.Update(keyType(tea.KeyEnter))
	if command == nil {
		t.Fatal("confirmed HTTPS-root operation did not start")
	}
	result, ok := command().(operationDoneMsg)
	if !ok || result.err != nil || !result.receipt.Verified || len(provider.sets) != 1 || provider.sets[0].Backend != "http://localhost:3000" {
		t.Fatalf("explicit IPv6 alias was not passed to the exact root operation: result=%#v changes=%#v", result, provider.sets)
	}
}

func TestOperationFailureBannerIncludesBackendRemediation(t *testing.T) {
	m := workspaceFixture()
	remediation := "Refresh provider status; disable the exact path and retry with --localhost-backend. Hostname resolution weakens the IPv6 address guarantee."
	err := fault.NewError(fault.ErrOperation, "tailscale", "unknown proxy destination", false, "failed", remediation)
	targetKey := m.view.Items[0].Listener.Target.Normalized().Key()
	m.Update(operationDoneMsg{targetKey: targetKey, err: err})
	if !strings.Contains(m.banner, "unknown proxy destination") || !strings.Contains(m.banner, "Next: "+remediation) {
		t.Fatalf("operation failure banner hid its backend remediation: %q", m.banner)
	}
}

func TestWorkspaceObservedURLPickerChoosesOneExactPathForCopy(t *testing.T) {
	m := workspaceFixture()
	first := exposuredata.ExposureRoute{ID: "api", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", URL: "https://dev.example.ts.net/api"}
	second := exposuredata.ExposureRoute{ID: "docs", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", URL: "https://dev.example.ts.net/docs"}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{first, second}
	var copied string
	m.clipboard = ClipboardFunc(func(_ context.Context, value string) error { copied = value; return nil })
	m.Update(keyRune('y'))
	if m.modal != modalChooseURL || !strings.Contains(m.View(), "https://dev.example.ts.net/api") || !strings.Contains(m.View(), "https://dev.example.ts.net/docs") {
		t.Fatalf("multiple observed URLs were not presented for exact selection: modal=%v view=%q", m.modal, m.View())
	}
	m.Update(keyType(tea.KeyDown))
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("URL picker did not start the selected copy action")
	}
	cmd()
	if copied != "https://dev.example.ts.net/docs" {
		t.Fatalf("copy action selected %q, want second exact path URL", copied)
	}
}

func TestWorkspaceDisableUnknownRouteUsesFocusedConfirmWithoutYES(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].Ownership = exposuredata.OwnershipUnknown
	m.view.Exposures.Routes[0].Ownership = exposuredata.OwnershipUnknown
	m.openAction(ptrMode(exposuredata.ExposureDisabled))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.confirm {
		t.Fatalf("disable confirmation did not start with Cancel focused: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	m.Update(keyType(tea.KeyTab))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || len(m.activeOps) != 1 {
		t.Fatalf("focused disable confirmation did not start: modal=%v ops=%d", m.modal, len(m.activeOps))
	}
}

func TestWorkspaceObservedURLPickerClosesWhenRouteIdentityChanges(t *testing.T) {
	m := workspaceFixture()
	m.selectedID = m.view.Items[0].ID
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{
		{ID: "api", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", URL: "https://dev.example.ts.net/api"},
		{ID: "docs", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", URL: "https://dev.example.ts.net/docs"},
	}
	var copied string
	m.clipboard = ClipboardFunc(func(_ context.Context, value string) error { copied = value; return nil })
	m.Update(keyRune('y'))
	if m.modal != modalChooseURL {
		t.Fatalf("URL picker did not open: modal=%v", m.modal)
	}
	m.view.Items[0].Routes[1].URL = "https://dev.example.ts.net/reference"
	m.invalidatePreviewIfChanged()
	if m.modal != modalNone || !strings.Contains(m.banner, "Observed route URLs changed") {
		t.Fatalf("stale URL selection remained actionable: modal=%v banner=%q", m.modal, m.banner)
	}
	if copied != "" {
		t.Fatalf("stale URL was copied after identity changed: %q", copied)
	}
}

func TestURLShortcutStatusUsesTerminalClipboardTransport(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].URL = "https://dev.example.ts.net"
	m.clipboard = &OSC52Clipboard{Output: &bytes.Buffer{}}
	if got := m.urlShortcutStatus(); !strings.Contains(got, "y copy[ok]") {
		t.Fatalf("terminal clipboard transport was not advertised: %q", got)
	}
}

func TestURLShortcutStatusMarksHTTPSOnlyAvailability(t *testing.T) {
	if got := formatURLShortcutStatus(false, false, false, false, false, false); got != "o HTTPS[off]  y copy[off]" {
		t.Fatalf("unavailable shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(true, false, false, false, true, false); got != "o HTTPS[ok]  y copy[URL-only]" {
		t.Fatalf("URL-only shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(true, false, false, false, true, true); got != "o HTTPS[ok]  y copy[ok]" {
		t.Fatalf("available shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(false, true, false, false, true, false); got != "o HTTPS[HTTPS-only]  y copy[HTTPS-only]" {
		t.Fatalf("insecure route status = %q", got)
	}
	if got := formatURLShortcutStatus(false, false, true, false, true, false); got != "o HTTPS[TCP-only]  y copy[off]" {
		t.Fatalf("TCP-only shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(false, false, true, true, true, true); got != "o HTTP-preview[ok]  y copy[ok]" {
		t.Fatalf("Serve TCP preview shortcut status = %q", got)
	}
	if got := compactURLShortcutStatus("o HTTP-preview[ok]  y copy[ok]  O local[ok]"); got != "o:ok  y:ok  O:ok" {
		t.Fatalf("compact browser shortcut status = %q", got)
	}
}

func rawServeTCPWorkspaceFixture() *workspaceModel {
	m := workspaceFixture()
	route := m.view.Items[0].Routes[0]
	route.ProviderKey = "serve:tcp=3000"
	route.URL = ""
	route.Mode = exposuredata.ExposureServe
	route.Kind = exposuredata.RouteKindRawTCP
	route.State = exposuredata.ExposureActive
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{route}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{route}
	m.view.Items[0].Mode = exposuredata.ExposureServe
	m.view.Items[0].State = exposuredata.ExposureActive
	return m
}

func magicDNSStatusProvider(t *testing.T, dnsName string) *tailscale.Adapter {
	t.Helper()
	return &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		if strings.Join(args, " ") != "status --json" {
			t.Errorf("MagicDNS preview made unexpected provider call: %v", args)
		}
		return runner.Result{Stdout: fmt.Sprintf(`{"Self":{"DNSName":%q}}`, dnsName)}, nil
	})}
}

func TestMagicDNSPreviewURLUsesProviderNameAndRejectsMalformedValues(t *testing.T) {
	got, err := magicDNSPreviewURL("device.tailnet.ts.net.", 3000)
	if err != nil || got != "http://device.tailnet.ts.net:3000/" {
		t.Fatalf("MagicDNS preview URL = %q, %v", got, err)
	}
	for _, test := range []struct {
		name string
		port int
	}{
		{name: "", port: 3000},
		{name: "device/path.tailnet.ts.net", port: 3000},
		{name: "device@evil.tailnet.ts.net", port: 3000},
		{name: "single-label", port: 3000},
		{name: "100.64.0.1", port: 3000},
		{name: "device.tailnet.ts.net", port: 0},
		{name: "device.tailnet.ts.net", port: 65536},
	} {
		if got, err := magicDNSPreviewURL(test.name, test.port); err == nil {
			t.Errorf("invalid MagicDNS preview %q:%d produced %q", test.name, test.port, got)
		}
	}
}

func TestOpenServeTCPPreviewUsesReportedMagicDNSAndExactPort(t *testing.T) {
	capture := installTestBrowserLauncher(t)
	m := rawServeTCPWorkspaceFixture()
	m.provider = magicDNSStatusProvider(t, "device.tailnet.ts.net.")
	command := m.openSelectedURL()
	if command == nil {
		t.Fatalf("o did not resolve a Serve TCP preview: banner=%q", m.banner)
	}
	message, ok := command().(statusMsg)
	if !ok || message.value != "Opened Serve TCP HTTP preview" {
		t.Fatalf("Serve TCP preview returned an unexpected browser result: %#v", message)
	}
	opened, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "http://device.tailnet.ts.net:3000/" {
		t.Fatalf("o opened %q; want provider MagicDNS name and exact listener port", opened)
	}
	if len(m.view.Items[0].Routes) != 1 || m.view.Items[0].Routes[0].ProviderKey != "serve:tcp=3000" {
		t.Fatal("browser preview changed the observed route")
	}
}

func TestServeTCPPreviewClearsStaleURLFailureFeedbackOnRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		open   bool
		banner string
	}{
		{name: "open unavailable", open: true, banner: unavailableOpenURLBanner},
		{name: "copy unavailable", banner: unavailableCopyURLBanner},
		{name: "missing MagicDNS", open: true, banner: magicDNSPreviewUnavailableBanner},
		{name: "MagicDNS status error", banner: "Could not read Tailscale MagicDNS status: status timed out"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.open {
				installTestBrowserLauncher(t)
			}
			m := rawServeTCPWorkspaceFixture()
			m.provider = magicDNSStatusProvider(t, "device.tailnet.ts.net.")
			m.clipboard = ClipboardFunc(func(context.Context, string) error { return nil })
			m.banner, m.bannerSticky = test.banner, true
			var command tea.Cmd
			if test.open {
				command = m.openSelectedURL()
			} else {
				command = m.copyURL()
			}
			if command == nil {
				t.Fatalf("%s command was unavailable: %q", test.name, m.banner)
			}
			if m.banner != "" {
				t.Fatalf("successful preview retry left stale feedback: %q", m.banner)
			}
		})
	}
}

func TestOpenServeTCPPreviewFailsClosedWithoutReportedMagicDNS(t *testing.T) {
	capture := installTestBrowserLauncher(t)
	m := rawServeTCPWorkspaceFixture()
	m.provider = magicDNSStatusProvider(t, "")
	command := m.openSelectedURL()
	if command == nil {
		t.Fatalf("missing MagicDNS name was not checked asynchronously: banner=%q", m.banner)
	}
	message, ok := command().(statusMsg)
	if !ok || !message.sticky || !strings.Contains(message.value, "valid MagicDNS name") {
		t.Fatalf("missing MagicDNS name did not produce actionable failure feedback: %#v", message)
	}
	m.Update(message)
	if m.transient != "" || !strings.Contains(m.banner, "valid MagicDNS name") {
		t.Fatalf("failed preview left stale progress or lost its banner: transient=%q banner=%q", m.transient, m.banner)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("browser launched without a reported MagicDNS name: stat err=%v", err)
	}
}

func TestURLShortcutStatusAdvertisesServePreviewAndLocalO(t *testing.T) {
	installTestBrowserLauncher(t)
	m := rawServeTCPWorkspaceFixture()
	m.provider = magicDNSStatusProvider(t, "device.tailnet.ts.net.")
	m.clipboard = ClipboardFunc(func(context.Context, string) error { return nil })
	status := m.urlShortcutStatus()
	for _, want := range []string{"o HTTP-preview[ok]", "y copy[ok]", "O local[ok]"} {
		if !strings.Contains(status, want) {
			t.Fatalf("browser shortcut status omitted %q: %q", want, status)
		}
	}
}

func TestCopyServeTCPPreviewUsesSameMagicDNSAndExactPort(t *testing.T) {
	m := rawServeTCPWorkspaceFixture()
	m.provider = magicDNSStatusProvider(t, "device.tailnet.ts.net.")
	var copied string
	m.clipboard = ClipboardFunc(func(_ context.Context, value string) error {
		copied = value
		return nil
	})
	command := m.copyURL()
	if command == nil {
		t.Fatalf("y did not resolve the Serve TCP preview: banner=%q", m.banner)
	}
	if _, ok := command().(statusMsg); !ok {
		t.Fatal("Serve TCP preview copy returned an unexpected message")
	}
	if copied != "http://device.tailnet.ts.net:3000/" {
		t.Fatalf("y copied %q; want the same provider MagicDNS preview as o", copied)
	}
}

func TestFunnelTCPPreviewRemainsUnavailable(t *testing.T) {
	m := rawServeTCPWorkspaceFixture()
	m.view.Items[0].Routes[0].ProviderKey = "funnel:tcp=3000"
	m.view.Items[0].Routes[0].Mode = exposuredata.ExposureFunnel
	m.view.Items[0].Mode = exposuredata.ExposureFunnel
	m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		t.Fatalf("Funnel TCP action should not query DNS for an HTTP preview: %v", args)
		return runner.Result{}, nil
	})}
	if command := m.openSelectedURL(); command != nil {
		t.Fatal("Funnel TCP route unexpectedly produced a browser preview")
	}
	if !strings.Contains(m.banner, "TCP-only") {
		t.Fatalf("Funnel TCP feedback omitted the TCP-only reason: %q", m.banner)
	}
	m.banner = ""
	if command := m.copyURL(); command != nil {
		t.Fatal("Funnel TCP route unexpectedly produced a copy command")
	}
	if !strings.Contains(m.banner, "TCP-only") {
		t.Fatalf("Funnel TCP copy feedback omitted the TCP-only reason: %q", m.banner)
	}
}

func installTestBrowserLauncher(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	capture := filepath.Join(directory, "opened-url")
	t.Setenv("TAILGE_OPEN_URL_CAPTURE", capture)
	script := []byte(`#!/bin/sh
printf '%s' "$1" > "$TAILGE_OPEN_URL_CAPTURE"
`)
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(directory, name), script, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
	if browserCommand() == "" {
		t.Skip("browser launcher unsupported on this platform")
	}
	return capture
}

func TestUppercaseOLocalBrowserShortcutOpensSelectedListener(t *testing.T) {
	capture := installTestBrowserLauncher(t)
	m := workspaceFixture()
	m.banner, m.bannerSticky = localURLUnavailableBanner, true
	model, command := m.Update(keyRune('O'))
	if command == nil {
		t.Fatal("uppercase O did not produce a local browser command")
	}
	if updated, ok := model.(*workspaceModel); !ok || updated.modal != modalNone || updated.banner != "" {
		t.Fatalf("uppercase O did not clear stale local-listener feedback or changed workspace mode: %#v", model)
	}
	message, ok := command().(statusMsg)
	if !ok || message.value != "Opened local HTTP URL" {
		t.Fatalf("uppercase O returned an unexpected browser result: %#v", message)
	}
	opened, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "http://localhost:3000/" {
		t.Fatalf("uppercase O opened %q; want the local listener convenience URL", opened)
	}
}

func TestLocalBrowserShortcutRequiresFreshListenerSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspaceModel)
	}{
		{name: "non-authoritative", mutate: func(m *workspaceModel) { m.view.Listeners.Authoritative = false }},
		{name: "stale", mutate: func(m *workspaceModel) { m.view.Listeners.Stale = true }},
		{name: "error", mutate: func(m *workspaceModel) {
			m.view.Listeners.Error = &fault.SafeError{Code: fault.ErrUnknown, Message: "listener refresh failed"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := installTestBrowserLauncher(t)
			m := workspaceFixture()
			test.mutate(m)
			if command := m.openLocalURL(); command != nil {
				t.Fatal("stale listener snapshot unexpectedly produced a local browser command")
			}
			if !strings.Contains(m.banner, "No active local listener") || !strings.Contains(m.urlShortcutStatus(), "O local[off]") {
				t.Fatalf("stale local listener was not refused or advertised: banner=%q status=%q", m.banner, m.urlShortcutStatus())
			}
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Fatalf("browser launched from a stale listener snapshot: stat err=%v", err)
			}
		})
	}
}

func TestLocalBrowserShortcutWorksWhenOnlyExposureRefreshFailed(t *testing.T) {
	capture := installTestBrowserLauncher(t)
	m := workspaceFixture()
	m.viewErr = errors.New("exposure status unavailable")
	command := m.openLocalURL()
	if command == nil {
		t.Fatalf("fresh local listener was blocked by an exposure refresh error: %q", m.banner)
	}
	if _, ok := command().(statusMsg); !ok {
		t.Fatal("local browser command returned an unexpected result")
	}
	opened, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "http://localhost:3000/" {
		t.Fatalf("local browser shortcut opened %q; want current local listener URL", opened)
	}
}

func TestOpenShortcutLaunchesObservedHTTPSPathWithoutCreatingRoutes(t *testing.T) {
	capture := installTestBrowserLauncher(t)
	m := workspaceFixture()
	url := "https://dev.example.ts.net/tailge-probe"
	route := exposuredata.ExposureRoute{ID: "observed-path", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/tailge-probe", URL: url, Target: m.view.Items[0].Listener.Target, Mode: exposuredata.ExposureServe}
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{route}
	command := m.openSelectedURL()
	if command == nil {
		t.Fatalf("o did not launch the observed HTTPS URL: banner=%q", m.banner)
	}
	message, ok := command().(statusMsg)
	if !ok || message.value != "Opened observed HTTPS URL" {
		t.Fatalf("observed HTTPS URL launch failed: %#v", message)
	}
	opened, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != url {
		t.Fatalf("o opened %q instead of the exact observed route URL %q", opened, url)
	}
}

func TestOpenShortcutExplainsHTTPSSetupWithoutCreatingRoutes(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes = nil
	m.view.Exposures.Routes = nil
	if command := m.openSelectedURL(); command != nil {
		t.Fatal("o unexpectedly created or opened a route without an observed URL or Serve TCP route")
	}
	if !strings.Contains(m.banner, "No observed browser URL or Serve TCP preview") || !strings.Contains(m.banner, "Use b to preview a private HTTPS root") {
		t.Fatalf("missing URL feedback did not explain the HTTPS setup action: %q", m.banner)
	}
	m.banner = ""
	if command := m.copyURL(); command != nil {
		t.Fatal("y unexpectedly copied a URL without an observed route or Serve TCP route")
	}
	if !strings.Contains(m.banner, "No observed browser URL or Serve TCP preview") || !strings.Contains(m.banner, "Use b to preview a private HTTPS root") {
		t.Fatalf("missing URL feedback did not explain the copy setup action: %q", m.banner)
	}
}

func TestOpenSelectedObservedURLClearsStaleMissingURLBanner(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].URL = "https://dev.example.ts.net/node-4321"
	m.banner, m.bannerSticky = unavailableURLBanner, true
	cmd := m.openSelectedURL()
	if m.banner == unavailableURLBanner {
		t.Fatalf("stale missing-URL banner remained after observed URL became available (cmd=%t)", cmd != nil)
	}
	if cmd == nil && !strings.Contains(m.banner, "Opening URLs is unsupported") {
		t.Fatalf("observed URL was neither opened nor rejected with a browser capability reason: cmd=%t banner=%q", cmd != nil, m.banner)
	}
}

func TestServeTCPPreviewRequiresExactActiveListenerPort(t *testing.T) {
	m := rawServeTCPWorkspaceFixture()
	m.view.Items[0].Routes[0].ProviderKey = "serve:tcp=3001"
	m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		t.Fatalf("mismatched listener port must not query MagicDNS: %v", args)
		return runner.Result{}, nil
	})}
	if command := m.openSelectedURL(); command != nil {
		t.Fatal("Serve route with a mismatched public port unexpectedly produced a browser command")
	}
	if !strings.Contains(m.banner, "TCP-only") || !strings.Contains(m.banner, "exact active Serve listener") {
		t.Fatalf("mismatched route did not fail closed with actionable feedback: %q", m.banner)
	}
}

func TestServeTCPPreviewRequiresFreshAuthoritativeListenerSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspaceModel)
	}{
		{name: "non-authoritative", mutate: func(m *workspaceModel) { m.view.Listeners.Authoritative = false }},
		{name: "stale", mutate: func(m *workspaceModel) { m.view.Listeners.Stale = true }},
		{name: "error", mutate: func(m *workspaceModel) {
			m.view.Listeners.Error = &fault.SafeError{Code: fault.ErrUnknown, Message: "listener refresh failed"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := rawServeTCPWorkspaceFixture()
			test.mutate(m)
			m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
				t.Fatalf("untrusted listener state must not query MagicDNS: %v", args)
				return runner.Result{}, nil
			})}
			if command := m.openSelectedURL(); command != nil {
				t.Fatal("untrusted listener state unexpectedly produced a browser preview")
			}
			if !strings.Contains(m.banner, "TCP-only") {
				t.Fatalf("stale listener preview was not refused clearly: %q", m.banner)
			}
		})
	}
}

func TestServeTCPPreviewRequiresFreshAuthoritativeExposureIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspaceModel)
	}{
		{name: "non-authoritative exposure", mutate: func(m *workspaceModel) { m.view.Exposures.Authoritative = false }},
		{name: "stale exposure", mutate: func(m *workspaceModel) { m.view.Exposures.Stale = true }},
		{name: "exposure error", mutate: func(m *workspaceModel) {
			m.view.Exposures.Error = &fault.SafeError{Code: fault.ErrUnknown, Message: "exposure refresh failed"}
		}},
		{name: "view error", mutate: func(m *workspaceModel) { m.viewErr = errors.New("view refresh failed") }},
		{name: "wrong route kind", mutate: func(m *workspaceModel) { m.view.Items[0].Routes[0].Kind = exposuredata.RouteKindHTTPPath }},
		{name: "missing route id", mutate: func(m *workspaceModel) { m.view.Items[0].Routes[0].ID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := rawServeTCPWorkspaceFixture()
			test.mutate(m)
			m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
				t.Fatalf("incomplete exposure identity must not query MagicDNS: %v", args)
				return runner.Result{}, nil
			})}
			if command := m.openSelectedURL(); command != nil {
				t.Fatal("incomplete exposure identity unexpectedly produced a browser preview")
			}
			if !strings.Contains(m.banner, "TCP-only") {
				t.Fatalf("incomplete exposure identity was not refused clearly: %q", m.banner)
			}
		})
	}
}

func TestServeTCPPreviewStatusLookupsAreBounded(t *testing.T) {
	for _, test := range []struct {
		name string
		open bool
	}{
		{name: "open"},
		{name: "copy", open: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.open {
				installTestBrowserLauncher(t)
			}
			m := rawServeTCPWorkspaceFixture()
			m.clipboard = ClipboardFunc(func(context.Context, string) error { return nil })
			m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(ctx context.Context, _ string, args ...string) (runner.Result, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Errorf("MagicDNS status lookup has no bounded deadline: deadline=%v present=%t", deadline, ok)
				}
				return runner.Result{Stdout: `{"Self":{"DNSName":"device.tailnet.ts.net."}}`}, nil
			})}
			var command tea.Cmd
			if test.open {
				command = m.openSelectedURL()
			} else {
				command = m.copyURL()
			}
			if command == nil {
				t.Fatalf("%s command was unavailable: %q", test.name, m.banner)
			}
			if _, ok := command().(statusMsg); !ok {
				t.Fatalf("%s command returned an unexpected result", test.name)
			}
		})
	}
}

func TestObservedHTTPSURLMayBelongToExactTCPRoute(t *testing.T) {
	route := exposuredata.ExposureRoute{ProviderKey: "serve:tcp=4321", URL: "https://dev.example.ts.net:4321"}
	if got, ok := workspace.ObservedHTTPSRouteURL(route); !ok || got != route.URL {
		t.Fatalf("explicit HTTPS URL was rejected for TCP selector: got=%q ok=%t", got, ok)
	}
	if got := formatURLShortcutStatus(true, false, true, false, true, false); got != "o HTTPS[ok]  y copy[URL-only]" {
		t.Fatalf("observed HTTPS URL shortcut status = %q", got)
	}
}

func TestTerminateInactiveRouteExplainsNoProcess(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Listener = nil
	m.openTerminateProcess()
	if !strings.Contains(m.banner, "inactive") || !strings.Contains(m.banner, "use d") {
		t.Fatalf("inactive route termination guidance=%q", m.banner)
	}
}

func TestBrowserShortcutsRejectObservedHTTPURLs(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].URL = "http://dev.example.ts.net:3000/"
	if command := m.openSelectedURL(); command != nil || !strings.Contains(m.banner, "not HTTPS") {
		t.Fatalf("HTTP URL was not blocked from opening: command=%t banner=%q", command != nil, m.banner)
	}
	var copied string
	m.clipboard = ClipboardFunc(func(_ context.Context, value string) error {
		copied = value
		return nil
	})
	if command := m.copyURL(); command != nil || copied != "" || !strings.Contains(m.banner, "not HTTPS") {
		t.Fatalf("HTTP URL was not blocked from copying: command=%t copied=%q banner=%q", command != nil, copied, m.banner)
	}
}

func TestURLEffectBoundariesRejectNonHTTPSURLs(t *testing.T) {
	m := workspaceFixture()
	if command := m.openURL("http://dev.example.ts.net", "test"); command != nil || !strings.Contains(m.banner, "HTTPS") {
		t.Fatalf("direct openURL call accepted HTTP: command=%t banner=%q", command != nil, m.banner)
	}
	m.banner = ""
	if command := m.copyURLCommand("http://dev.example.ts.net"); command != nil || !strings.Contains(m.banner, "HTTPS") {
		t.Fatalf("direct copyURLCommand call accepted HTTP: command=%t banner=%q", command != nil, m.banner)
	}
	var out, errOut bytes.Buffer
	var copied string
	copyURLValue(&out, &errOut, "http://dev.example.ts.net", ClipboardFunc(func(_ context.Context, value string) error {
		copied = value
		return nil
	}))
	if out.Len() != 0 || copied != "" || !strings.Contains(errOut.String(), "HTTPS") {
		t.Fatalf("copyURLValue did not fail closed: out=%q error=%q copied=%q", out.String(), errOut.String(), copied)
	}
}

func TestWorkspaceVAndShiftVSelection(t *testing.T) {
	m := workspaceFixture()
	// Anchor the selection before adding a row that sorts before the current item.
	m.selectedID = m.view.Items[0].ID
	second := discovery.Listener{ID: "listener-two", Name: "api", Process: "node", PID: 4343, Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.Update(keyRune('v'))
	if m.selectionCount() != 1 || !m.isMarked("listener-app") {
		t.Fatalf("v did not select current item: selected=%d marks=%#v", m.selectionCount(), m.selectedItems)
	}
	m.Update(keyRune('j'))
	m.Update(keyRune('v'))
	if m.selectionCount() != 2 || !m.isMarked("listener-two") {
		t.Fatalf("v did not allow multiple selections: selected=%d marks=%#v", m.selectionCount(), m.selectedItems)
	}
	m.Update(keyRune('V'))
	if !m.visualSelection || m.selectionCount() != 2 || !strings.Contains(m.View(), "[V]") || !strings.Contains(m.View(), "[✓]") || strings.Contains(m.View(), "[VISUAL SELECT]") {
		t.Fatalf("Shift+v did not preserve previous selected rows: visual=%t selected=%d view=%q", m.visualSelection, m.selectionCount(), m.View())
	}
	m.Update(keyRune('k'))
	if m.selectionCount() != 2 || !m.isMarked("listener-app") || !m.isMarked("listener-two") {
		t.Fatalf("visual navigation did not extend the range: selected=%d marks=%#v", m.selectionCount(), m.selectedItems)
	}
	m.Update(keyRune('V'))
	if m.visualSelection || m.selectionCount() != 2 {
		t.Fatalf("Shift+v did not exit while keeping range: visual=%t selected=%d", m.visualSelection, m.selectionCount())
	}
	if !strings.Contains(m.View(), "[✓]") {
		t.Fatalf("selected marker was not rendered: %q", m.View())
	}
}

func TestWorkspaceZoomTogglesTheFocusedPane(t *testing.T) {
	m := workspaceFixture()
	m.width, m.height = 120, 30
	if view := m.View(); !strings.Contains(view, "SERVICE LIST") || !strings.Contains(view, "DETAILS") {
		t.Fatalf("workspace did not start in split view: %q", view)
	}
	m.Update(keyRune('z'))
	zoomedList := m.View()
	if !m.zoomed || !strings.Contains(zoomedList, "SERVICE LIST") || strings.Contains(zoomedList, "DETAILS") || !strings.Contains(m.transient, "List pane zoomed") {
		t.Fatalf("z did not zoom the focused list pane or show zoom status: zoomed=%t view=%q", m.zoomed, zoomedList)
	}
	if lines := strings.Split(zoomedList, "\n"); len(lines) < 3 || lipgloss.Width(lines[2]) != m.width {
		t.Fatalf("zoomed list did not fill terminal width %d: %q", m.width, zoomedList)
	}
	m.Update(keyType(tea.KeyRight))
	zoomedDetails := m.View()
	if !m.zoomed || !strings.Contains(zoomedDetails, "DETAILS") || strings.Contains(zoomedDetails, "SERVICE LIST") {
		t.Fatalf("focus change did not display the focused pane while zoomed: zoomed=%t view=%q", m.zoomed, zoomedDetails)
	}
	if lines := strings.Split(zoomedDetails, "\n"); len(lines) < 3 || lipgloss.Width(lines[2]) != m.width {
		t.Fatalf("zoomed details did not fill terminal width %d: %q", m.width, zoomedDetails)
	}
	m.Update(keyRune('z'))
	if m.zoomed || !strings.Contains(m.View(), "SERVICE LIST") || !strings.Contains(m.View(), "DETAILS") {
		t.Fatalf("z did not restore split view: zoomed=%t view=%q", m.zoomed, m.View())
	}
}

func TestWorkspaceZoomRequiresSplitView(t *testing.T) {
	m := workspaceFixture()
	m.width, m.height = splitMinWidth-1, splitMinHeight
	m.Update(keyRune('z'))
	if m.zoomed || !strings.Contains(m.transient, "Zoom requires split view") {
		t.Fatalf("zoom changed state without a split view: zoomed=%t transient=%q", m.zoomed, m.transient)
	}
}

func TestVisualSelectionMarkerUsesColorWithoutDependingOnIt(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	item := workspaceFixture().view.Items[0]
	row := listenerTableRow(item, 100, false, true, true, "dark")
	colorStart := strings.Index(row, "\x1b[1;35m")
	serviceText := strings.Index(row, "web")
	colorEnd := strings.Index(row[colorStart:], "\x1b[0m")
	if !strings.Contains(row, "[V]") || colorStart < 0 || serviceText < colorStart || colorEnd < 0 {
		t.Fatalf("visual row lacked full-row color: %q", row)
	}
	if colorStart+colorEnd < serviceText {
		t.Fatalf("visual row color ended before service columns: %q", row)
	}
	t.Setenv("NO_COLOR", "1")
	row = listenerTableRow(item, 100, false, true, true, "dark")
	if !strings.Contains(row, "[V]") || strings.Contains(row, "\x1b[") {
		t.Fatalf("visual marker was not accessible without color: %q", row)
	}
}

func TestListenerDetailsShowProcessWorkingDirectory(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Listener.WorkingDirectory = "/Users/alice/Projects/tailge"
	details := m.renderDetails(100, 40)
	if !strings.Contains(details, "working directory: /Users/alice/Projects/tailge") {
		t.Fatalf("details omitted the process working directory: %q", details)
	}

	m.view.Items[0].Listener.WorkingDirectory = "/very-long-directory/" + strings.Repeat("nested/", 12)
	for _, line := range strings.Split(m.renderDetails(32, 80), "\n") {
		if width := lipgloss.Width(line); width > 32 {
			t.Fatalf("working-directory detail exceeded narrow pane: width=%d line=%q", width, line)
		}
	}
}

func TestListenerListShowsCPUAndResidentMemory(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Listener.Usage = &discovery.ProcessUsage{CPUPercent: 12.5, MemoryBytes: 42 * 1024 * 1024}
	row := listenerTableRow(m.view.Items[0], 51, false, false, false, "")
	if !strings.Contains(listenerTableHeader(51), "CPU%") || !strings.Contains(listenerTableHeader(51), "MEM") || !strings.Contains(row, "12.5%") || !strings.Contains(row, "42M") {
		t.Fatalf("list omitted process resource usage at normal split width: header=%q row=%q", listenerTableHeader(51), row)
	}
	if view := m.View(); !strings.Contains(view, "12.5%") || !strings.Contains(view, "42M") {
		t.Fatalf("normal workspace view hid process resource usage: %q", view)
	}
	details := m.renderDetails(100, 40)
	if !strings.Contains(details, "CPU usage: 12.5% (ps %CPU)") || !strings.Contains(details, "memory (RSS): 42.0 MiB") {
		t.Fatalf("details omitted source-labeled process usage: %q", details)
	}
}

func TestListenerDetailsLabelPhysicalFootprintAndKeepLargeMemoryPrecision(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Listener.Usage = &discovery.ProcessUsage{
		CPUPercent: 7.5, MemoryBytes: 13260138336,
		MemorySource: discovery.ProcessMemoryPhysicalFootprint,
	}
	row := listenerTableRow(m.view.Items[0], 51, false, false, false, "")
	if !strings.Contains(row, "12.3G") {
		t.Fatalf("large physical footprint was not rendered precisely: %q", row)
	}
	details := m.renderDetails(100, 40)
	if !strings.Contains(details, "memory (physical footprint): 12.35 GiB") {
		t.Fatalf("details did not name the physical-footprint metric: %q", details)
	}
}

func TestListenerListMarksUnavailableCPUAndMemory(t *testing.T) {
	item := workspaceFixture().view.Items[0]
	row := listenerTableRow(item, 100, false, false, false, "")
	if strings.Count(row, "—") < 2 {
		t.Fatalf("unavailable CPU and memory were not both marked: %q", row)
	}
}

func TestProcessMemoryCellPreservesPrecisionForLargeFootprints(t *testing.T) {
	if got := processMemoryCell(13260138336); got != "12.3G" {
		t.Fatalf("large physical footprint cell = %q, want 12.3G", got)
	}
	if got := processMemoryCell(100*(1<<30) - 1); got != "100G" {
		t.Fatalf("near-100-GiB memory cell = %q, want a compact value with its unit", got)
	}
}

func TestBatchDisableSelectsHTTPRouteByIdentity(t *testing.T) {
	item := exposure.ReconciledItem{Routes: []exposuredata.ExposureRoute{{ID: "api-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api"}}}
	providerKey, routeID := batchRouteSelection(exposuredata.ExposureDisabled, item)
	if providerKey != "serve:https=443" || routeID != "api-route" {
		t.Fatalf("batch disable did not retain exact HTTP route identity: key=%q id=%q", providerKey, routeID)
	}
}

func TestWorkspaceBatchActionStartsForSelectedItems(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.view.Items[0].Routes = nil
	second := discovery.Listener{ID: "listener-two", Name: "api", Process: "node", PID: 4343, Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.Update(keyRune('V'))
	m.Update(keyRune('j'))
	m.Update(keyRune('V'))
	m.openAction(ptrMode(exposuredata.ExposureServe))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm {
		t.Fatalf("batch action did not open confirmation: modal=%v banner=%q", m.modal, m.banner)
	}
	m.Update(keyType(tea.KeyTab))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || m.batchTotal != 2 || len(m.activeOps) != 2 {
		t.Fatalf("batch operation did not start: modal=%v total=%d active=%d banner=%q", m.modal, m.batchTotal, len(m.activeOps), m.banner)
	}
}

func TestWorkspaceWildcardBackendIsNotVerifiedNoOp(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].Target.Address = "0.0.0.0"
	m.view.Exposures.Routes[0].Target.Address = "0.0.0.0"
	item, ok := m.selectedItem()
	if !ok || workspace.SameStateForItem(m.view, item, exposuredata.ExposureServe) {
		t.Fatal("wildcard provider backend was treated as a verified no-op")
	}
}

func TestWorkspaceActionSafetyAndVerifiedNoOp(t *testing.T) {
	m := workspaceFixture()
	m.Update(keyRune(' '))
	if m.modal != modalAction || m.actionSession.index != modeIndex(exposuredata.ExposureServe) {
		t.Fatalf("space did not open selector at current mode: modal=%v index=%d", m.modal, m.actionSession.index)
	}
	m.modal = modalNone
	m.openAction(ptrMode(exposuredata.ExposureServe))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || m.transient != "Already active: serve" {
		t.Fatalf("verified same-state action was not a no-op: modal=%v status=%q", m.modal, m.transient)
	}
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.confirm {
		t.Fatalf("Funnel did not open ordinary focused confirmation: modal=%v focus=%t", m.modal, m.actionSession.confirm)
	}
	m.Update(keyType(tea.KeyTab))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || len(m.activeOps) != 1 {
		t.Fatalf("focused Funnel confirmation did not start operation: modal=%v ops=%d", m.modal, len(m.activeOps))
	}
}

func TestWorkspaceFocusedConfirmationCanBeCancelledWithoutStickyError(t *testing.T) {
	m := workspaceFixture()
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	m.Update(keyRune('n'))
	if m.modal != modalConfirm {
		t.Fatalf("ordinary confirmation closed while unfocused: modal=%v", m.modal)
	}
	m.Update(keyType(tea.KeyEsc))
	if m.modal != modalNone || m.banner != "" {
		t.Fatalf("cancelled confirmation left modal/banner state: modal=%v banner=%q", m.modal, m.banner)
	}
}

type fakeProcessObserver struct{}

func (fakeProcessObserver) List(context.Context) (discovery.ListenerSnapshot, error) {
	return discovery.ListenerSnapshot{Authoritative: true}, nil
}

type fakeProcessTerminator struct{}

func (fakeProcessTerminator) Terminate(context.Context, discovery.Listener) error { return nil }

func TestWorkspaceProcessTerminationUsesFocusedConfirmation(t *testing.T) {
	m := workspaceFixture()
	m.controller.Discoverer = fakeProcessObserver{}
	m.processTerminator = fakeProcessTerminator{}
	m.Update(keyRune('x'))
	if m.modal != modalTerminateProcess || m.confirmFocus || !strings.Contains(m.View(), "TERMINATE PROCESS") || !strings.Contains(m.View(), "PID: 4242") {
		t.Fatalf("x did not open focused non-token process confirmation: modal=%v focus=%t view=%q", m.modal, m.confirmFocus, m.View())
	}
	m.Update(keyType(tea.KeyTab))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || !m.processBusy || !strings.Contains(m.renderList(100, 20), "TERMINATING") {
		t.Fatalf("focused process confirmation did not start or render status: modal=%v busy=%t list=%q", m.modal, m.processBusy, m.renderList(100, 20))
	}
	cmd := m.startTerminateProcess()
	if cmd != nil {
		t.Fatal("duplicate process termination was not blocked")
	}
}

func TestWorkspaceProcessTerminationSupportsSelectedBatch(t *testing.T) {
	m := workspaceFixture()
	second := discovery.Listener{ID: "listener-two", Name: "api", Process: "python", ProcessStart: "test:4343", PID: 4343, Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.controller.Discoverer = fakeProcessObserver{}
	m.processTerminator = fakeProcessTerminator{}
	m.Update(keyRune('v'))
	m.Update(keyRune('j'))
	m.Update(keyRune('v'))
	m.Update(keyRune('x'))
	if m.modal != modalTerminateProcess || len(m.processBatch) != 2 || !strings.Contains(m.View(), "Selected processes: 2") {
		t.Fatalf("x did not open multi-process confirmation: modal=%v batch=%d view=%q", m.modal, len(m.processBatch), m.View())
	}
	// A periodic listener refresh must validate every captured process in the
	// batch, not compare the focused row only with the batch's first process.
	// Resource changes alone are not process-identity changes.
	m.view.Items[0].Listener.Usage = &discovery.ProcessUsage{CPUPercent: 7.5, MemoryBytes: 64 * 1024 * 1024}
	m.invalidatePreviewIfChanged()
	if m.modal != modalTerminateProcess || len(m.processBatch) != 2 {
		t.Fatalf("refresh invalidated a valid multi-process confirmation: modal=%v batch=%d banner=%q", m.modal, len(m.processBatch), m.banner)
	}
	m.Update(keyType(tea.KeyTab))
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil || !m.processBusy || !strings.Contains(m.renderList(100, 20), "TERMINATING") {
		t.Fatalf("batch process confirmation did not start or render status: busy=%t cmd=%v list=%q", m.processBusy, cmd != nil, m.renderList(100, 20))
	}
	message := cmd()
	_, next := m.Update(message)
	if next == nil || !m.processBusy || m.processBatchDone != 1 {
		t.Fatalf("batch process did not advance sequentially: busy=%t done=%d next=%v", m.processBusy, m.processBatchDone, next != nil)
	}
	message = next()
	_, _ = m.Update(message)
	if m.processBusy || len(m.processBatch) != 0 || !strings.Contains(m.transient, "Terminated 2 process(es)") {
		t.Fatalf("batch process did not finish: busy=%t batch=%d transient=%q", m.processBusy, len(m.processBatch), m.transient)
	}
}

func TestWorkspaceProcessConfirmationClosesWhenAnyBatchIdentityChanges(t *testing.T) {
	m := workspaceFixture()
	second := discovery.Listener{ID: "listener-two", Name: "api", Process: "python", ProcessStart: "test:4343", PID: 4343, Target: targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Scope: targetmodel.ScopeLoopback, Metadata: discovery.MetadataComplete}
	m.view.Items = append(m.view.Items, exposure.ReconciledItem{ID: second.ID, Listener: &second, State: exposuredata.ExposureState("disabled"), Mode: exposuredata.ExposureDisabled})
	m.Update(keyRune('v'))
	m.Update(keyRune('j'))
	m.Update(keyRune('v'))
	m.Update(keyRune('x'))
	if m.modal != modalTerminateProcess || len(m.processBatch) != 2 {
		t.Fatalf("multi-process confirmation did not open: modal=%v batch=%d", m.modal, len(m.processBatch))
	}
	m.view.Items[1].Listener.ProcessStart = "test:changed"
	m.invalidatePreviewIfChanged()
	if m.modal != modalNone || len(m.processBatch) != 0 || !strings.Contains(m.banner, "Selection changed") {
		t.Fatalf("changed batch process was not rejected: modal=%v batch=%d banner=%q", m.modal, len(m.processBatch), m.banner)
	}
}

func TestWorkspaceGuardsQuitAndDuplicateOperations(t *testing.T) {
	m := workspaceFixture()
	target, _ := itemTarget(m.view.Items[0])
	m.activeOps[target.Key()] = func() {}
	m.Update(keyRune('q'))
	if m.modal != modalQuit {
		t.Fatal("quit bypassed Applying guard")
	}
	m.modal = modalNone
	m.openAction(ptrMode(exposuredata.ExposureFunnel))
	m.Update(keyType(tea.KeyEnter))
	for _, r := range "YES" {
		m.Update(keyRune(r))
	}
	m.Update(keyType(tea.KeyTab))
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone || !strings.Contains(m.banner, "already has an operation") {
		t.Fatalf("duplicate operation was not rejected: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestWorkspaceQuitRecordsUnverifiedCancellationBeforeExit(t *testing.T) {
	m := workspaceFixture()
	m.ctx, m.cancel = context.WithCancel(context.Background())
	target, _ := itemTarget(m.view.Items[0])
	cancelled := false
	m.activeOps[target.Key()] = func() { cancelled = true }
	m.modal = modalQuit
	m.modalChoice = true
	m.Update(keyType(tea.KeyEnter))
	if !cancelled || !m.quittingAfterCancel || m.modal != modalNone {
		t.Fatalf("quit did not wait for cancellation: cancelled=%t waiting=%t modal=%v", cancelled, m.quittingAfterCancel, m.modal)
	}
	m.Update(quitAfterCancelMsg{generation: m.quitGeneration})
	if m.quittingAfterCancel {
		t.Fatal("quit remained blocked after cancellation grace period")
	}
	if m.view.Items[0].OperationState != exposuredata.ExposureUnverified {
		t.Fatalf("cancelled operation was not marked unverified: %q", m.view.Items[0].OperationState)
	}
}

func TestWorkspaceQuitShortcutConfirmsGuardedQuit(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRune('q'), keyType(tea.KeyCtrlC)} {
		m := workspaceFixture()
		cancelled := false
		m.processBusy = true
		m.processCancel = func() { cancelled = true }
		m.modal = modalQuit

		_, cmd := m.Update(key)
		if cmd == nil || !cancelled || !m.quittingAfterCancel || m.modal != modalNone {
			t.Fatalf("%s did not use guarded quit path: cmd=%t cancelled=%t waiting=%t modal=%v", key.String(), cmd != nil, cancelled, m.quittingAfterCancel, m.modal)
		}
	}
}
