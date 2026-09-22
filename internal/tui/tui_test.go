package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
			readiness: readiness.Readiness{At: now, Status: readiness.ReadinessReady, Modes: []readiness.ModeReadiness{{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessReady}, {Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessReady}}},
			hasView:   true, hasReadiness: true, refreshState: refreshCoordinator{seq: 1, viewDone: true, readinessDone: true},
			focus: focusList, width: 120, height: 30,
			activeOps: map[string]context.CancelFunc{},
		},
		ctx:         context.Background(),
		controller:  exposure.NewController(nil, nil),
		searchInput: newInput("/ "), paletteInput: newInput(": "),
	}
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
	funnel := exposuredata.ExposureRoute{ID: "funnel-app", ProviderKey: "funnel:https=3000", Target: m.view.Items[0].Routes[0].Target, Mode: exposuredata.ExposureFunnel, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	m.view.Exposures.Routes = append(m.view.Exposures.Routes, funnel)
	m.view.Items[0].Routes = append(m.view.Items[0].Routes, funnel)
	m.view.Items[0].State = exposuredata.ExposureAmbiguous
	m.view.Items[0].Warning = "multiple exposure routes match this listener"
	m.readiness.Modes[1].Status = readiness.ReadinessReadOnly
	view := m.renderDetails(120, 100)
	for _, want := range []string{"[! AMBIG]", "── ALERTS ──", "── EXPOSURE ROUTES (2) ──", "[MANAGED]", "[UNKNOWN]", "[! READ-ONLY]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("details missing %q: %s", want, view)
		}
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
		"C or Ctrl-l", "U                    clear all selected items", "s                    preview private Serve", "f                    preview public Funnel", "d                    preview Disable", "Ctrl-d/Page Down", "Ctrl-u/Page Up", "Home/End", "Funnel               remains public",
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
	m.view.Items[0].State = exposuredata.ExposureAmbiguous
	m.view.Items[0].Warning = "multiple exposure routes match this listener; choose an exact route before changing it"
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
	if m.modal != modalConfirm || m.actionSession.routeKey != "funnel:https=3000" || m.actionSession.confirm {
		t.Fatalf("selected route was not carried into focused confirmation: modal=%v route=%q focus=%t", m.modal, m.actionSession.routeKey, m.actionSession.confirm)
	}
	if view := m.View(); !strings.Contains(view, "funnel:https=3000") || !strings.Contains(view, "external/unknown") {
		t.Fatalf("confirmation did not show selected route and ownership warning: %q", view)
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
