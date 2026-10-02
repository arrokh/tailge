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
		searchInput: newInput("/ "), paletteInput: newInput(": "), pathInput: newInput("Path slug: "),
	}
}

type pathRouteTestListener struct{ snapshot discovery.ListenerSnapshot }

func (observer *pathRouteTestListener) List(context.Context) (discovery.ListenerSnapshot, error) {
	return observer.snapshot, nil
}

type pathRouteTestProvider struct {
	snapshot  exposuredata.ExposureSnapshot
	caps      tailscale.Capabilities
	readiness readiness.Readiness
	sets      []tailscale.ExposureChange
	removes   []tailscale.RouteSelector
}

func (provider *pathRouteTestProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return provider.caps, nil
}

func (provider *pathRouteTestProvider) Readiness(context.Context, tailscale.ReadinessOptions) (readiness.Readiness, error) {
	return provider.readiness, nil
}

func (provider *pathRouteTestProvider) List(context.Context) (exposuredata.ExposureSnapshot, error) {
	copySnapshot := provider.snapshot
	copySnapshot.Routes = append([]exposuredata.ExposureRoute(nil), provider.snapshot.Routes...)
	return copySnapshot, nil
}

func (provider *pathRouteTestProvider) Set(_ context.Context, change tailscale.ExposureChange) (exposuredata.OperationReceipt, error) {
	provider.sets = append(provider.sets, change)
	routeTarget := change.Target.Normalized()
	if parsed, err := targetmodel.ParseTarget(change.Backend, "tcp"); err == nil {
		routeTarget = parsed
	}
	route := exposuredata.ExposureRoute{
		ID:          targetmodel.StableID(string(change.Mode), change.Target.Key(), change.ProviderKey, change.Path),
		ProviderKey: change.ProviderKey, Kind: exposuredata.RouteKindHTTPPath,
		Path: change.Path, Backend: change.Backend, Target: routeTarget,
		Mode: change.Mode, URL: "https://dev.example.ts.net" + change.Path,
		Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive,
	}
	provider.snapshot.Routes = append(provider.snapshot.Routes, route)
	return exposuredata.OperationReceipt{ID: route.ID}, nil
}

func (provider *pathRouteTestProvider) Remove(_ context.Context, selector tailscale.RouteSelector, _ string) (exposuredata.OperationReceipt, error) {
	provider.removes = append(provider.removes, selector)
	for index, route := range provider.snapshot.Routes {
		if route.ProviderKey == selector.ID && route.Path == selector.Path && route.Backend == selector.Backend {
			provider.snapshot.Routes = append(provider.snapshot.Routes[:index], provider.snapshot.Routes[index+1:]...)
			break
		}
	}
	return exposuredata.OperationReceipt{ID: "remove-" + selector.ID}, nil
}

func pathRouteCapabilities() tailscale.Capabilities {
	return tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true, ServeHTTPS: true, FunnelHTTPS: true, ServePath: true, FunnelPath: true}
}

func pathRouteReadiness() readiness.Readiness {
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
		{120, []string{"↑↓ Move", "Tab Focus", "v/V Select", "s/f/d Routes", "x Term", "p Path", "e/S Sort", "/ Find", "z Zoom", "? Help", "q Quit"}},
		{100, []string{"↑↓ Move", "Tab Focus", "v Mark", "s/f/d Routes", "e/S Sort", "/ Find", "z Zoom", "? Help", "q Quit"}},
		{72, []string{"↑↓ Move", "Tab Focus", "e/S Sort", "/ Find", "? Help", "q Quit"}},
		{30, []string{"o:off", "O:ok", "y:off", "? Help"}},
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
	for _, hint := range []string{"Enter/Esc", "o:off", "O:ok", "y:off"} {
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
	if !strings.Contains(visible, "List p[ok]") {
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
	if !strings.Contains(line, "3d16efb GitHub") || !strings.Contains(line, "List p[ok]") {
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
		"C or Ctrl-l", "U                    clear all selected items", "s                    preview private Serve", "f                    preview public Funnel", "d                    preview Disable", "p                    add one named HTTP path", "choose among paths", "Ctrl-d/Page Down", "Ctrl-u/Page Up", "Home/End", "Funnel               remains public", "z                    zoom the focused pane", "e/S                  cycle Name ↑, Name ↓, and unsorted order", "CPU% / MEM",
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
	if strings.Contains(view, "Serve (tailnet only) [unavailable]") || strings.Contains(view, "Funnel (public internet) [unavailable]") {
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
	if view := m.View(); !strings.Contains(view, "Funnel (public internet) [unavailable]") {
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

func TestHTTPPathConflictAllowsOnlyExactSameRouteIdentity(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	route := exposuredata.ExposureRoute{ID: "api", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:3000", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	if reason := httpPathRouteConflict([]exposuredata.ExposureRoute{route}, "/api", exposuredata.ExposureServe, target, false); reason != "" {
		t.Fatalf("exact same route was treated as collision: %s", reason)
	}
	otherTarget := targetmodel.Target{Address: "127.0.0.1", Port: 3001, Protocol: "tcp"}.Normalized()
	if reason := httpPathRouteConflict([]exposuredata.ExposureRoute{route}, "/api", exposuredata.ExposureServe, otherTarget, false); reason == "" || !strings.Contains(reason, "disable the exact handler") {
		t.Fatalf("same path on a different backend did not explain exact replacement: %q", reason)
	}
}

func TestWorkspaceNamedHTTPPathPreviewsAndAppliesThroughSharedController(t *testing.T) {
	m := workspaceFixture()
	listener := *m.view.Items[0].Listener
	listener.Target = targetmodel.Target{Address: "::1", Port: 3000, Protocol: "tcp"}.Normalized()
	m.view.Listeners.Listeners = []discovery.Listener{listener}
	m.view.Items[0].Listener = &listener
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.view.Items[0].Routes = nil
	sibling := exposuredata.ExposureRoute{ID: "sibling-docs", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Backend: "http://127.0.0.1:3001", Target: targetmodel.Target{Address: "127.0.0.1", Port: 3001, Protocol: "tcp"}, Mode: exposuredata.ExposureServe, URL: "https://dev.example.ts.net/docs", Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{sibling}
	m.controller = exposure.NewController(&pathRouteTestListener{snapshot: m.view.Listeners}, &pathRouteTestProvider{snapshot: m.view.Exposures, caps: pathRouteCapabilities(), readiness: pathRouteReadiness()})
	m.pathInput = newInput("Path slug: ")
	m.Update(keyRune('p'))
	if m.modal != modalHTTPPath || !m.httpPathAction {
		t.Fatalf("p did not open named path input: modal=%v pathAction=%t", m.modal, m.httpPathAction)
	}
	m.pathInput.SetValue("api-v2")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.httpPath != "/api-v2" {
		t.Fatalf("named path did not produce a confirmation preview: modal=%v path=%q", m.modal, m.httpPath)
	}
	preview := m.View()
	for _, want := range []string{"Named HTTP path: /api-v2", "Provider endpoint: standard HTTPS port 443", "http://[::1]:3000", "Serve remains private"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("confirmation preview missing %q: %s", want, preview)
		}
	}
	m.Update(keyType(tea.KeyTab))
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("confirmed named path did not invoke the shared exposure controller")
	}
	result, ok := cmd().(operationDoneMsg)
	if !ok || result.err != nil || !result.receipt.Verified {
		t.Fatalf("shared path operation was not verified: message=%#v", result)
	}
	provider := m.controller.Provider.(*pathRouteTestProvider)
	if len(provider.snapshot.Routes) != 2 || provider.snapshot.Routes[0].ID != sibling.ID || provider.snapshot.Routes[1].Path != "/api-v2" || provider.snapshot.Routes[1].Backend != "http://[::1]:3000" {
		t.Fatalf("path apply replaced its sibling or retargeted IPv6: %#v", provider.snapshot.Routes)
	}
}

func TestWorkspaceHTTPPathConfirmationReturnsToInputDuringRefresh(t *testing.T) {
	m := workspaceFixture()
	m.view.Exposures.Routes = nil
	m.view.Items[0].Routes = nil
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.pathInput = newInput("Path slug: ")
	m.Update(keyRune('p'))
	m.pathInput.SetValue("api")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || !m.httpPathAction {
		t.Fatalf("named path did not reach confirmation: modal=%v pathAction=%t", m.modal, m.httpPathAction)
	}
	m.Update(keyType(tea.KeyTab))
	m.refreshState.pending = true
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalHTTPPath || !m.httpPathAction || m.httpPath != "/api" {
		t.Fatalf("refresh changed path confirmation into a raw exposure action: modal=%v pathAction=%t path=%q", m.modal, m.httpPathAction, m.httpPath)
	}
	if !strings.Contains(m.banner, "refresh is in progress") {
		t.Fatalf("paused path preview did not explain the refresh: %q", m.banner)
	}
}

func TestInvalidatedHTTPPathPreviewClearsMutationIntent(t *testing.T) {
	m := workspaceFixture()
	m.view.Exposures.Routes = nil
	m.view.Items[0].Routes = nil
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.pathInput = newInput("Path slug: ")
	m.Update(keyRune('p'))
	m.pathInput.SetValue("api")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || !m.httpPathAction {
		t.Fatalf("named path did not reach confirmation: modal=%v pathAction=%t", m.modal, m.httpPathAction)
	}
	m.view.Listeners.Listeners[0].ID = "renamed-listener"
	m.invalidatePreviewIfChanged()
	if m.modal != modalNone || m.httpPathAction || m.httpPath != "" || m.httpPathLocalhostBackend {
		t.Fatalf("stale named path intent survived invalidation: modal=%v action=%t path=%q alias=%t", m.modal, m.httpPathAction, m.httpPath, m.httpPathLocalhostBackend)
	}
	m.Update(keyRune('s'))
	if m.modal != modalAction || m.httpPathAction {
		t.Fatalf("next raw action inherited stale named path intent: modal=%v pathAction=%t", m.modal, m.httpPathAction)
	}
}

func TestWorkspaceNamedHTTPPathAllowsExplicitLocalhostBackendForIPv6(t *testing.T) {
	m := workspaceFixture()
	listener := *m.view.Items[0].Listener
	listener.Target = targetmodel.Target{Address: "::1", Port: 3000, Protocol: "tcp"}.Normalized()
	m.view.Listeners.Listeners = []discovery.Listener{listener}
	m.view.Items[0].Listener = &listener
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.view.Items[0].Routes = nil
	m.view.Exposures.Routes = nil
	provider := &pathRouteTestProvider{snapshot: m.view.Exposures, caps: pathRouteCapabilities(), readiness: pathRouteReadiness()}
	m.controller = exposure.NewController(&pathRouteTestListener{snapshot: m.view.Listeners}, provider)
	m.Update(keyRune('p'))
	m.Update(keyType(tea.KeyCtrlB))
	m.pathInput.SetValue("api")
	m.Update(keyType(tea.KeyEnter))
	preview := m.View()
	for _, want := range []string{"Named HTTP path: /api", "Local HTTP backend: http://localhost:3000", "hostname resolution", "weakened"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("explicit localhost alias preview omitted %q: %s", want, preview)
		}
	}
	m.Update(keyType(tea.KeyTab))
	_, command := m.Update(keyType(tea.KeyEnter))
	if command == nil {
		t.Fatal("confirmed IPv6 named path did not start its operation")
	}
	result := command()
	if _, ok := result.(operationDoneMsg); !ok {
		t.Fatalf("named path operation returned unexpected result: %#v", result)
	}
	if len(provider.sets) != 1 || provider.sets[0].Backend != "http://localhost:3000" {
		t.Fatalf("TUI did not pass explicit localhost backend to the controller: %#v", provider.sets)
	}
	m.view.Items[0].Routes = append([]exposuredata.ExposureRoute(nil), provider.snapshot.Routes...)
	capture := installTestBrowserLauncher(t)
	_, openCommand := m.Update(keyRune('o'))
	if openCommand == nil {
		t.Fatalf("o did not open the observed HTTPS URL after p configured it: banner=%q", m.banner)
	}
	message, ok := openCommand().(statusMsg)
	if !ok || message.value != "Opened observed URL" {
		t.Fatalf("o returned an unexpected browser result: %#v", message)
	}
	opened, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != "https://dev.example.ts.net/api" || len(provider.sets) != 1 || len(provider.removes) != 0 {
		t.Fatalf("p→o flow did not open the observed route without another mutation: url=%q sets=%#v removes=%#v", opened, provider.sets, provider.removes)
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

func TestWorkspaceNamedHTTPPathFunnelWarnsAboutSharedEndpoint(t *testing.T) {
	m := workspaceFixture()
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{}
	m.view.Items[0].Routes = nil
	m.view.Items[0].State = exposuredata.ExposureState("disabled")
	m.view.Items[0].Mode = exposuredata.ExposureDisabled
	m.pathInput = newInput("Path slug: ")
	m.Update(keyRune('p'))
	m.Update(keyType(tea.KeyTab))
	m.pathInput.SetValue("public-site")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalConfirm || m.actionSession.mode != exposuredata.ExposureFunnel || m.actionSession.confirm {
		t.Fatalf("Funnel preview did not require explicit focus: modal=%v mode=%s confirm=%t", m.modal, m.actionSession.mode, m.actionSession.confirm)
	}
	if view := m.View(); !strings.Contains(view, "Funnel makes EVERY path on this shared HTTPS endpoint public") {
		t.Fatalf("shared endpoint public consequence was omitted: %q", view)
	}
}

func TestWorkspaceHTTPPathShortcutRejectsAmbiguousRouteIdentity(t *testing.T) {
	m := workspaceFixture()
	first := exposuredata.ExposureRoute{ID: "route-one", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: m.view.Items[0].Listener.Target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Backend = "route-two", "http://localhost:4321"
	m.view.Items[0].Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Exposures.Routes = []exposuredata.ExposureRoute{first, second}
	m.view.Items[0].State = exposuredata.ExposureAmbiguous
	if m.httpPathShortcutAvailable(m.view.Items[0]) {
		t.Fatal("p was shown as available for an ambiguous route identity")
	}
	m.Update(keyRune('p'))
	if m.modal != modalNone || !strings.Contains(m.banner, "route identity is ambiguous") {
		t.Fatalf("ambiguous route set reached the HTTP path preview: modal=%v banner=%q", m.modal, m.banner)
	}
}

func TestWorkspaceHTTPPathShortcutShowsUnsupportedProviderReason(t *testing.T) {
	m := workspaceFixture()
	m.readiness.Modes[0].HTTPPathStatus = readiness.ReadinessReadOnly
	m.readiness.Modes[0].HTTPPathMessage = "--set-path is unavailable"
	m.readiness.Modes[0].HTTPPathRemediation = "Upgrade Tailscale"
	m.readiness.Modes[1].HTTPPathStatus = readiness.ReadinessReadOnly
	if !strings.Contains(m.renderBottom(), "p HTTP path[off]") {
		t.Fatalf("unavailable path capability was not shown in shortcut status: %q", m.renderBottom())
	}
	m.Update(keyRune('p'))
	m.pathInput.SetValue("api")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalHTTPPath || !strings.Contains(m.banner, "--set-path is unavailable") || !strings.Contains(m.banner, "Upgrade Tailscale") {
		t.Fatalf("unsupported provider reason was hidden: modal=%v banner=%q", m.modal, m.banner)
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

func TestURLShortcutStatusMarksUnavailableActions(t *testing.T) {
	item := workspaceFixture().view.Items[0]
	if got := formatURLShortcutStatus(false, false, false, true, false, false); got != "o observed[off]  O localhost[off]  y copy[off]" {
		t.Fatalf("unavailable shortcut status = %q", got)
	}
	item.Routes[0].URL = "https://dev.example.ts.net:3000"
	if got := formatURLShortcutStatus(true, false, false, true, true, false); got != "o observed[ok]  O localhost[ok]  y copy[URL-only]" {
		t.Fatalf("URL-only shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(true, false, false, true, true, true); got != "o observed[ok]  O localhost[ok]  y copy[ok]" {
		t.Fatalf("available shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(false, true, false, true, true, false); got != "o observed[TCP-only]  O localhost[ok]  y copy[off]" {
		t.Fatalf("TCP-only shortcut status = %q", got)
	}
	if got := formatURLShortcutStatus(false, false, true, true, true, true); got != "o observed[ok]  O localhost[ok]  y copy[ok]" {
		t.Fatalf("Serve TCP preview shortcut status = %q", got)
	}
}

func TestCopyURLResolvesServeTCPPreview(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].ProviderKey = "serve:tcp=4321"
	m.view.Items[0].Routes[0].URL = ""
	m.provider = &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		if strings.Join(args, " ") != "status --json" {
			t.Fatalf("unexpected provider command: %v", args)
		}
		return runner.Result{Stdout: `{"Self":{"DNSName":"dev.tailnet.ts.net."}}`}, nil
	})}
	var copied string
	m.clipboard = ClipboardFunc(func(_ context.Context, value string) error {
		copied = value
		return nil
	})
	message := m.copyURL()()
	status, ok := message.(statusMsg)
	if !ok || !strings.Contains(status.value, "URL copied to clipboard") || copied != "http://dev.tailnet.ts.net:4321/" {
		t.Fatalf("Serve TCP copy result=%#v copied=%q", message, copied)
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
	if !ok || message.value != "Opened observed URL" {
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

func TestOpenShortcutExplainsPathSetupWithoutCreatingRoutes(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes = nil
	m.view.Exposures.Routes = nil
	if command := m.openSelectedURL(); command != nil {
		t.Fatal("o unexpectedly created or opened a route without an observed URL")
	}
	if !strings.Contains(m.banner, "o only opens observed URLs") || !strings.Contains(m.banner, "p to configure") {
		t.Fatalf("missing observed URL feedback did not explain the open-only contract and setup action: %q", m.banner)
	}
	m.banner = ""
	if command := m.copyURL(); command != nil {
		t.Fatal("y unexpectedly copied a URL without an observed route")
	}
	if !strings.Contains(m.banner, "y only copies observed URLs") || !strings.Contains(m.banner, "p to configure") {
		t.Fatalf("missing observed URL feedback did not explain the copy-only contract and setup action: %q", m.banner)
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

func TestOpenSelectedURLExplainsTCPOnlyRoute(t *testing.T) {
	m := workspaceFixture()
	m.view.Items[0].Routes[0].ProviderKey = "funnel:tcp=10000"
	m.view.Items[0].Routes[0].Mode = exposuredata.ExposureFunnel
	m.selectedID = m.view.Items[0].ID
	m.openSelectedURL()
	if !strings.Contains(m.banner, "TCP-only") || !strings.Contains(m.banner, "funnel:tcp=10000") {
		t.Fatalf("TCP-only observed URL action was unclear: %q", m.banner)
	}
}

func TestObservedURLWinsWhenTCPSelectorHasExplicitURL(t *testing.T) {
	route := exposuredata.ExposureRoute{ProviderKey: "serve:tcp=4321", URL: "https://dev.example.ts.net:4321"}
	if got, ok := workspace.ObservedRouteURL(route); !ok || got != route.URL {
		t.Fatalf("explicit URL was rejected for TCP selector: got=%q ok=%t", got, ok)
	}
	if got := formatURLShortcutStatus(false, false, true, true, true, false); got != "o observed[ok]  O localhost[ok]  y copy[URL-only]" {
		t.Fatalf("Serve TCP fallback shortcut status = %q", got)
	}
}

func TestServeTCPBrowserURLUsesReportedDNSName(t *testing.T) {
	status := tailscale.Status{}
	status.Self.DNSName = "noors-macbook-pro.tail85727d.ts.net."
	route := exposuredata.ExposureRoute{ProviderKey: "serve:tcp=4321", Mode: exposuredata.ExposureServe}
	if got, err := workspace.ServeTCPBrowserURL(status, route); err != nil || got != "http://noors-macbook-pro.tail85727d.ts.net:4321/" {
		t.Fatalf("Serve TCP browser URL=%q err=%v", got, err)
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

func TestServeTCPBrowserURLRejectsUnsafeDNSName(t *testing.T) {
	status := tailscale.Status{}
	status.Self.DNSName = "bad/host"
	route := exposuredata.ExposureRoute{ProviderKey: "serve:tcp=4321", Mode: exposuredata.ExposureServe}
	if _, err := workspace.ServeTCPBrowserURL(status, route); err == nil {
		t.Fatal("unsafe Tailscale DNS name was accepted")
	}
}

func TestLocalURLUsesSelectedListenerPort(t *testing.T) {
	m := workspaceFixture()
	url, ok := localURL(m.view.Items[0])
	if !ok || url != "http://localhost:3000/" {
		t.Fatalf("localURL = %q, %t; want http://localhost:3000/, true", url, ok)
	}
	m.view.Items[0].Listener = nil
	if url, ok := localURL(m.view.Items[0]); ok || url != "" {
		t.Fatalf("localURL without listener = %q, %t; want empty, false", url, ok)
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
