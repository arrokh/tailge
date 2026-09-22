package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	splitMinWidth       = 100
	splitMinHeight      = 24
	minCompactRows      = 8
	minModalRows        = 12
	quitCancelGraceTime = 2 * time.Second
)

func refresh(parent context.Context, controller *exposure.Controller, timeout time.Duration) (exposure.View, error) {
	if controller == nil {
		return exposure.View{}, model.NewError(model.ErrDependency, "exposure", "exposure controller is unavailable", true, "unavailable", "Configure providers and retry.")
	}
	ctx, cancel := context.WithTimeout(parent, boundedTimeout(timeout, 15*time.Second, 10*time.Minute))
	defer cancel()
	view, err := controller.Refresh(ctx)
	if err == nil || controller == nil || controller.Provider != nil || controller.Discoverer == nil {
		return view, err
	}
	// A missing exposure provider must not erase successful local discovery.
	// Reconcile it against an explicitly non-authoritative exposure snapshot so
	// every row remains visible but every mutation stays blocked.
	listeners, discoveryErr := controller.Discoverer.List(ctx)
	snapshot := model.ExposureSnapshot{At: time.Now(), Authoritative: false, Source: "unavailable"}
	if err != nil {
		safe := model.AsAppError(err).Safe()
		snapshot.Error = &safe
	}
	if discoveryErr != nil {
		safe := model.AsAppError(discoveryErr).Safe()
		listeners.Error = &safe
		listeners.Authoritative = false
	}
	return exposure.Reconcile(listeners, snapshot, nil), err
}

func boundedTimeout(value, fallback, maximum time.Duration) time.Duration {
	if value <= 0 {
		value = fallback
	}
	if value > maximum {
		value = maximum
	}
	return value
}

func readinessFor(parent context.Context, provider *tailscale.Adapter, cfg config.Config, options tailscale.ReadinessOptions) (model.Readiness, error) {
	if provider == nil {
		return model.Readiness{At: time.Now(), Status: model.ReadinessNotReady, Modes: []model.ModeReadiness{
			{Mode: model.ExposureServe, Status: model.ReadinessNotReady},
			{Mode: model.ExposureFunnel, Status: model.ReadinessNotReady},
		}}, model.NewError(model.ErrDependency, "tui", "Tailscale provider is unavailable", true, "unavailable", "Install Tailscale and retry.")
	}
	ctx, cancel := context.WithTimeout(parent, boundedTimeout(cfg.OperationTimeout, 15*time.Second, 10*time.Minute))
	defer cancel()
	return provider.Readiness(ctx, options)
}

type paneFocus uint8

const (
	focusList paneFocus = iota
	focusDetails
)

type modalKind uint8

const (
	modalNone modalKind = iota
	modalAction
	modalDisableRoute
	modalConfirm
	modalTerminateProcess
	modalCancel
	modalQuit
	modalHelp
	modalPalette
)

type configLoadedMsg struct {
	cfg      config.Config
	warnings []string
	err      error
}

type viewLoadedMsg struct {
	seq  uint64
	view exposure.View
	err  error
}

type readinessLoadedMsg struct {
	seq  uint64
	data model.Readiness
	err  error
}

type operationDoneMsg struct {
	targetKey  string
	receipt    model.OperationReceipt
	err        error
	batchID    string
	batchIndex int
	batchTotal int
}

type batchOperation struct {
	itemID          string
	target          model.Target
	providerKey     string
	confirmExternal bool
	approval        exposure.MutationApproval
}

type batchContext struct {
	context.Context
	cancel context.CancelFunc
}

func newBatchContext(parent context.Context) batchContext {
	ctx, cancel := context.WithCancel(parent)
	return batchContext{Context: ctx, cancel: cancel}
}

type processTarget struct {
	itemID      string
	listener    model.Listener
	fingerprint string
}

type processDoneMsg struct {
	itemID     string
	pid        int
	err        error
	batchIndex int
	batchTotal int
}

type quitAfterCancelMsg struct{ generation uint64 }

type gTimeoutMsg struct{ generation uint64 }
type refreshTickMsg struct{}
type configSavedMsg struct {
	cfg config.Config
	err error
}
type configValidatedMsg struct {
	cfg config.Config
	err error
}

type workspaceModel struct {
	workspaceState

	ctx    context.Context
	cancel context.CancelFunc

	provider          *tailscale.Adapter
	clipboard         Clipboard
	controller        *exposure.Controller
	processTerminator discovery.ProcessTerminator
	manager           config.Manager

	searchInput  textinput.Model
	paletteInput textinput.Model
}

func newInput(prompt string) textinput.Model {
	input := textinput.New()
	input.Prompt = prompt
	input.CharLimit = 256
	input.Width = 32
	return input
}

func newWorkspaceModel(discoverer discovery.ListenerObserver, processTerminator discovery.ProcessTerminator, provider *tailscale.Adapter, manager config.Manager) *workspaceModel {
	ctx, cancel := context.WithCancel(context.Background())
	search := newInput("/ ")
	palette := newInput(": ")
	return &workspaceModel{
		workspaceState: newWorkspaceState(),
		ctx:            ctx,
		cancel:         cancel,
		provider:       provider,
		controller: func() *exposure.Controller {
			controller := exposure.NewController(discoverer, provider)
			controller.MutationLockPath = manager.Path + ".exposure.lock"
			return controller
		}(),
		processTerminator: processTerminator,
		manager:           manager,
		searchInput:       search,
		paletteInput:      palette,
	}
}

func (m *workspaceModel) Init() tea.Cmd {
	return loadConfigCmd(m.ctx, m.manager)
}

func loadConfigCmd(ctx context.Context, manager config.Manager) tea.Cmd {
	return func() tea.Msg {
		cfg, warnings, err := manager.Ensure(ctx)
		return configLoadedMsg{cfg: cfg, warnings: warnings, err: err}
	}
}

func refreshViewCmd(parent context.Context, controller *exposure.Controller, cfg config.Config, seq uint64) tea.Cmd {
	return func() tea.Msg {
		view, err := refresh(parent, controller, cfg.OperationTimeout)
		return viewLoadedMsg{seq: seq, view: view, err: err}
	}
}

func readinessCmd(parent context.Context, provider *tailscale.Adapter, cfg config.Config, options tailscale.ReadinessOptions, seq uint64) tea.Cmd {
	return func() tea.Msg {
		data, err := readinessFor(parent, provider, cfg, options)
		return readinessLoadedMsg{seq: seq, data: data, err: err}
	}
}

func refreshTickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(boundedTimeout(interval, 5*time.Second, 24*time.Hour), func(time.Time) tea.Msg { return refreshTickMsg{} })
}

func (m *workspaceModel) startRefresh() tea.Cmd {
	seq, started := m.refreshState.start()
	if !started {
		return nil
	}
	m.refreshActionModal()
	options := m.controller.ReadinessOptions
	return tea.Batch(refreshViewCmd(m.ctx, m.controller, m.cfg, seq), readinessCmd(m.ctx, m.provider, m.cfg, options, seq))
}

func (m *workspaceModel) finishRefreshPart(seq uint64) tea.Cmd {
	if !m.refreshState.finish(seq) {
		return nil
	}
	m.refreshState.recordResult(m.viewErr, m.readyErr)
	m.refreshActionModal()
	if m.viewErr == nil && m.readyErr == nil && m.transient == "Refreshing..." {
		m.transient = ""
	}
	if m.refreshState.consumeRetryPreview() {
		m.openAction(nil)
	}
	if m.refreshState.consumeAgain() {
		return m.startRefresh()
	}
	return nil
}

func (m *workspaceModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch message := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = message.Width, message.Height
		m.clampOffsets()
		return m, nil
	case configLoadedMsg:
		m.cfg = message.cfg
		m.configErr = message.err
		if message.err != nil && m.cfg == (config.Config{}) {
			m.cfg = config.Defaults()
		}
		m.controller.ReadinessOptions = tailscale.ReadinessOptions{ServeProbeVersion: m.cfg.ServeProbeVersion, FunnelProbeVersion: m.cfg.FunnelProbeVersion}
		if message.err != nil {
			m.setBanner("Configuration unavailable: "+safeMessage(message.err), true)
		} else if len(message.warnings) > 0 {
			m.appendBanner(strings.Join(message.warnings, " | "), true)
		}
		return m, tea.Batch(m.startRefresh(), refreshTickCmd(m.cfg.RefreshInterval))
	case refreshTickMsg:
		return m, tea.Batch(m.startRefresh(), refreshTickCmd(refreshBackoff(m.cfg.RefreshInterval, m.refreshState.failureCount())))
	case viewLoadedMsg:
		if !m.refreshState.accept(message.seq, refreshViewPart) {
			return m, nil
		}
		oldID, oldIdx := m.selectedID, m.selectedIdx
		available := viewAvailable(message.view)
		viewErr := message.err
		if !available && m.hasView && viewErr == nil {
			viewErr = model.NewError(model.ErrUnknown, "tui", "refresh returned no snapshot", true, "unknown", "Refresh again before changing exposure.")
		}
		if available || !m.hasView {
			m.view = message.view
			m.hasView = available
		}
		m.pruneSelection()
		m.viewErr = viewErr
		m.reselect(oldID, oldIdx)
		m.updateVisualSelection()
		m.invalidatePreviewIfChanged()
		m.refreshActionModal()
		if message.err != nil {
			m.setBanner("Listener/exposure refresh: "+safeMessage(message.err), true)
		} else if len(message.view.Warnings) > 0 {
			m.appendBanner(strings.Join(message.view.Warnings, " | "), true)
		}
		return m, m.finishRefreshPart(message.seq)
	case readinessLoadedMsg:
		if !m.refreshState.accept(message.seq, refreshReadinessPart) {
			return m, nil
		}
		if readinessAvailable(message.data) || !m.hasReadiness {
			m.readiness = message.data
			m.hasReadiness = readinessAvailable(message.data)
		}
		m.readyErr = message.err
		m.refreshActionModal()
		if message.err != nil {
			m.appendBanner("Tailscale readiness: "+safeMessage(message.err), true)
		}
		return m, m.finishRefreshPart(message.seq)
	case operationDoneMsg:
		delete(m.activeOps, message.targetKey)
		m.applyLocalOperationResult(message)
		if m.quittingAfterCancel && (message.err != nil || !message.receipt.Verified) {
			m.markOperationUnverified(message.targetKey)
		}
		if message.batchID != "" {
			m.batchCompleted++
			if message.err != nil {
				m.batchFailures++
			}
			if m.batchCompleted < message.batchTotal {
				m.transient = fmt.Sprintf("Applying batch (%d/%d)", m.batchCompleted, message.batchTotal)
				return m, nil
			}
			failures := m.batchFailures
			batchTotal := message.batchTotal
			if m.batchCancel != nil {
				m.batchCancel()
				m.batchCancel = nil
			}
			m.batchID, m.batchTotal, m.batchCompleted, m.batchFailures = "", 0, 0, 0
			if failures > 0 {
				m.setBanner(fmt.Sprintf("Batch completed with %d failed operation(s); refresh to inspect each result", failures), true)
			} else {
				m.transient = fmt.Sprintf("Batch verified (%d operation(s))", batchTotal)
			}
			if m.quittingAfterCancel {
				if !m.hasPendingOperations() {
					m.quittingAfterCancel = false
					m.cancel()
					return m, tea.Quit
				}
				return m, nil
			}
			return m, m.startRefresh()
		}
		if message.err != nil {
			m.setBanner("Operation: "+safeMessage(message.err), true)
		} else {
			m.transient = "Operation verified for " + message.targetKey
		}
		if m.quittingAfterCancel {
			if !m.hasPendingOperations() {
				m.quittingAfterCancel = false
				m.cancel()
				return m, tea.Quit
			}
			return m, nil
		}
		return m, m.startRefresh()
	case processDoneMsg:
		m.processBusy = false
		if m.processCancel != nil {
			m.processCancel()
			m.processCancel = nil
		}
		if message.batchTotal > 0 {
			return m, m.finishProcessBatch(message)
		}
		if message.err != nil {
			if m.quittingAfterCancel {
				m.processUnverified = true
			}
			m.setBanner("Process termination: "+safeMessage(message.err), true)
		} else {
			m.transient = fmt.Sprintf("Process %d terminated", message.pid)
		}
		if m.quittingAfterCancel {
			if !m.hasPendingOperations() {
				m.quittingAfterCancel = false
				m.cancel()
				return m, tea.Quit
			}
			return m, nil
		}
		return m, m.startRefresh()
	case quitAfterCancelMsg:
		if m.quittingAfterCancel && message.generation == m.quitGeneration {
			m.markActiveOperationsUnverified()
			m.processUnverified = m.processUnverified || m.processBusy
			m.quittingAfterCancel = false
			m.cancel()
			return m, tea.Quit
		}
		return m, nil
	case resolvedObservedURLMsg:
		if message.err != nil {
			m.setBanner("Observed URL: "+safeMessage(message.err), true)
			return m, nil
		}
		return m, m.openURL(message.url, "observed URL")
	case statusMsg:
		m.applyStatus(message)
		return m, nil
	case configSavedMsg:
		if message.err != nil {
			m.configErr = message.err
			m.setBanner("Configuration was not saved: "+safeMessage(message.err), true)
		} else {
			m.configErr = nil
			m.cfg = message.cfg
			m.transient = "Configuration saved"
		}
		return m, nil
	case configValidatedMsg:
		if message.err != nil {
			m.configErr = message.err
			m.setBanner("Config: "+safeMessage(message.err), true)
		} else {
			m.configErr = nil
			m.cfg = message.cfg
			m.transient = "Configuration is valid"
		}
		return m, nil
	case gTimeoutMsg:
		if m.gGeneration == message.generation {
			m.gGeneration = 0
		}
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(message)
	}
	return m, nil
}

func (m *workspaceModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.quittingAfterCancel {
		return m, nil
	}
	key := msg.String()
	if m.modal != modalNone {
		return m.updateModal(msg, key)
	}
	if m.searching {
		return m.updateSearch(msg, key)
	}
	if m.focus == focusDetails {
		if key == "esc" {
			m.focus = focusList
			return m, nil
		}
		if cmd, handled := m.handleGlobalNavigation(key); handled {
			return m, cmd
		}
	}
	if m.gGeneration != 0 {
		if key == "g" {
			m.gGeneration = 0
			m.goFirst()
			return m, nil
		}
		m.gGeneration = 0
	}
	if key == "g" {
		m.gGeneration++
		generation := m.gGeneration
		return m, tea.Tick(450*time.Millisecond, func(time.Time) tea.Msg { return gTimeoutMsg{generation: generation} })
	}
	if cmd, handled := m.handleGlobalNavigation(key); handled {
		return m, cmd
	}
	if effect, handled := m.effectForKey(key); handled {
		return m, m.executeWorkspaceEffect(effect)
	}
	switch key {
	case "/":
		m.previousQ, m.searching = m.query, true
		m.searchInput.SetValue(m.query)
		m.searchInput.Focus()
		return m, textinput.Blink
	case ":":
		m.openPalette()
		return m, textinput.Blink
	case "?":
		m.modal, m.helpOffset = modalHelp, 0
		return m, nil
	case "space", " ":
		m.openAction(nil)
	case "s":
		m.openAction(ptrMode(model.ExposureServe))
	case "f":
		m.openAction(ptrMode(model.ExposureFunnel))
	case "d":
		m.openAction(ptrMode(model.ExposureDisabled))
	case "v":
		m.toggleCurrentSelection()
	case "V":
		m.toggleVisualSelection()
	case "U":
		m.clearAllSelection()
	case "C", "ctrl+l":
		m.clearFilter()
	case "X":
		m.banner, m.bannerSticky, m.transient = "", false, ""
	}
	return m, nil
}

func ptrMode(mode model.ExposureMode) *model.ExposureMode { return &mode }

func (m *workspaceModel) handleGlobalNavigation(key string) (tea.Cmd, bool) {
	if key == "esc" {
		if m.visualSelection {
			m.visualSelection = false
			m.selectionAnchorID = ""
			m.visualRange = map[string]bool{}
			m.transient = "Visual selection cancelled"
			return nil, true
		}
		if m.gGeneration != 0 {
			m.gGeneration = 0
			return nil, true
		}
		m.focus = focusList
		return nil, true
	}
	if key == "tab" || key == "shift+tab" || key == "left" || key == "right" || key == "h" || key == "l" {
		switch key {
		case "tab", "shift+tab":
			if m.focus == focusList {
				m.focus = focusDetails
			} else {
				m.focus = focusList
			}
		case "left", "h":
			m.focus = focusList
		case "right", "l":
			m.focus = focusDetails
		}
		return nil, true
	}
	if key == "home" {
		m.goFirst()
		return nil, true
	}
	if key == "end" {
		m.goLast()
		return nil, true
	}
	if key == "pgdown" || key == "ctrl+d" {
		if m.focus == focusDetails {
			m.detailOffset += m.detailPage()
			m.clampOffsets()
		} else {
			m.moveSelection(maxInt(1, m.listPage()))
		}
		return nil, true
	}
	if key == "pgup" || key == "ctrl+u" {
		if m.focus == focusDetails {
			m.detailOffset -= m.detailPage()
			m.clampOffsets()
		} else {
			m.moveSelection(-maxInt(1, m.listPage()))
		}
		return nil, true
	}
	if key == "up" || key == "k" || key == "N" {
		if m.focus == focusDetails {
			m.detailOffset--
			m.clampOffsets()
		} else {
			m.moveSelection(-1)
		}
		return nil, true
	}
	if key == "down" || key == "j" || key == "n" {
		if m.focus == focusDetails {
			m.detailOffset++
			m.clampOffsets()
		} else {
			m.moveSelection(1)
		}
		return nil, true
	}
	if key == "enter" {
		m.focus = focusDetails
		m.detailOffset = 0
		return nil, true
	}
	return nil, false
}

func (m *workspaceModel) clearFilter() {
	if m.query == "" && !m.searching {
		return
	}
	m.query = ""
	m.previousQ = ""
	m.searching = false
	m.searchInput.SetValue("")
	m.searchInput.Blur()
	m.reselect(m.selectedID, m.selectedIdx)
	m.transient = "Filter cleared"
}

func (m *workspaceModel) updateSearch(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if key == "esc" {
		m.query, m.searching = m.previousQ, false
		m.searchInput.SetValue(m.query)
		m.searchInput.Blur()
		m.reselect(m.selectedID, m.selectedIdx)
		return m, nil
	}
	if key == "enter" {
		m.query, m.searching = m.searchInput.Value(), false
		m.searchInput.Blur()
		m.reselect(m.selectedID, m.selectedIdx)
		return m, nil
	}
	if key == "up" || key == "down" {
		if key == "up" {
			m.moveSelection(-1)
		} else {
			m.moveSelection(1)
		}
		return m, nil
	}
	if key == "ctrl+u" {
		m.searchInput.SetValue("")
		m.query = ""
		m.reselect(m.selectedID, m.selectedIdx)
		return m, nil
	}
	updated, cmd := m.searchInput.Update(msg)
	m.searchInput = updated
	m.query = updated.Value()
	m.reselect(m.selectedID, m.selectedIdx)
	return m, cmd
}

func (m *workspaceModel) openPalette() {
	m.modal = modalPalette
	m.paletteInput.SetValue("")
	m.paletteInput.Focus()
	m.paletteIndex = 0
}

var paletteCommands = []string{":refresh", ":retry", ":serve selected", ":funnel selected", ":disable selected", ":sort port", ":sort name", ":config show", ":config set key value", ":config validate", ":help", ":quit"}

func (m *workspaceModel) updateModal(msg tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	switch m.modal {
	case modalHelp:
		if key == "q" || key == "ctrl+c" {
			m.modal = modalNone
			return m, m.quitCommand()
		}
		if key == "esc" || key == "?" {
			m.modal = modalNone
			return m, nil
		}
		switch key {
		case "up", "k":
			m.helpOffset--
		case "down", "j":
			m.helpOffset++
		case "pgup", "ctrl+u", "b":
			m.helpOffset -= m.modalPage()
		case "pgdown", "ctrl+d", " ":
			m.helpOffset += m.modalPage()
		case "home":
			m.helpOffset = 0
		case "end":
			m.helpOffset = m.helpMaxOffset()
		}
		m.helpOffset = clamp(m.helpOffset, 0, m.helpMaxOffset())
		return m, nil
	case modalPalette:
		if key == "esc" {
			m.modal = modalNone
			m.paletteInput.Blur()
			return m, nil
		}
		if key == "up" || key == "ctrl+p" {
			m.paletteIndex = m.nextPaletteIndex(-1)
			return m, nil
		}
		if key == "down" || key == "ctrl+n" {
			m.paletteIndex = m.nextPaletteIndex(1)
			return m, nil
		}
		if key == "enter" {
			query := strings.TrimSpace(m.paletteInput.Value())
			if query == "" {
				commands := m.filteredPalette()
				if len(commands) > 0 {
					query = commands[m.paletteIndex%len(commands)]
				}
			}
			m.modal, m.paletteIndex = modalNone, 0
			m.paletteInput.Blur()
			return m, m.executePalette(query)
		}
		updated, cmd := m.paletteInput.Update(msg)
		m.paletteInput = updated
		m.paletteIndex = 0
		return m, cmd
	case modalAction:
		return m.updateActionModal(msg, key)
	case modalDisableRoute:
		return m.updateDisableRouteModal(msg, key)
	case modalConfirm:
		return m.updateConfirmModal(msg, key)
	case modalTerminateProcess:
		return m.updateTerminateProcessModal(msg, key)
	case modalCancel, modalQuit:
		return m.updateChoiceModal(msg, key)
	}
	return m, nil
}

func (m *workspaceModel) updateActionModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if key == "esc" {
		m.modal = modalNone
		return m, nil
	}
	if key == "up" || key == "k" {
		m.actionSession.index = (m.actionSession.index + 2) % 3
		return m, nil
	}
	if key == "down" || key == "j" {
		m.actionSession.index = (m.actionSession.index + 1) % 3
		return m, nil
	}
	if key != "enter" {
		return m, nil
	}
	choice, ok := m.actionSession.selectedChoice()
	if !ok {
		m.modal = modalNone
		m.setBanner("Exposure actions are unavailable", true)
		return m, nil
	}
	items := m.actionItems()
	if choice.disabled {
		// The workspace owns refresh/readiness state and supplies this result to
		// the modal. A waiting choice keeps the modal open without making the
		// modal inspect the workspace's refresh lifecycle.
		if choice.wait {
			return m, nil
		}
		m.modal = modalNone
		m.setBanner(choice.reason, true)
		return m, nil
	}
	mode := choice.mode
	allSame := len(items) > 0
	for _, item := range items {
		if !m.sameStateForItem(item, mode) {
			allSame = false
			break
		}
	}
	if allSame {
		m.modal = modalNone
		if len(items) == 1 {
			m.transient = alreadyMessage(mode)
		} else {
			m.transient = fmt.Sprintf("Already %s for %d selected service(s)", mode, len(items))
		}
		return m, nil
	}
	if mode == model.ExposureDisabled && len(items) == 1 {
		if len(items[0].Routes) > 1 {
			m.modal = modalDisableRoute
			m.disableRouteIndex = 0
			return m, nil
		}
		if len(items[0].Routes) == 1 {
			m.actionSession.routeKey = items[0].Routes[0].ProviderKey
		}
	}
	return m.beginConfirmation(mode)
}

func (m *workspaceModel) updateDisableRouteModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	item, ok := m.actionAnchorItem()
	if !ok || len(item.Routes) < 2 {
		m.modal = modalNone
		m.setBanner("Selection changed — refresh required", true)
		return m, nil
	}
	if key == "esc" {
		m.modal = modalAction
		return m, nil
	}
	if key == "up" || key == "k" {
		m.disableRouteIndex = (m.disableRouteIndex + len(item.Routes) - 1) % len(item.Routes)
		return m, nil
	}
	if key == "down" || key == "j" {
		m.disableRouteIndex = (m.disableRouteIndex + 1) % len(item.Routes)
		return m, nil
	}
	if key != "enter" {
		return m, nil
	}
	route := item.Routes[m.disableRouteIndex]
	if route.ProviderKey == "" {
		m.setBanner("Disable unavailable: exact route selector is missing", true)
		return m, nil
	}
	m.actionSession.routeKey = route.ProviderKey
	return m.beginConfirmation(model.ExposureDisabled)
}

func (m *workspaceModel) beginConfirmation(mode model.ExposureMode) (tea.Model, tea.Cmd) {
	m.actionSession.mode = mode
	m.actionSession.confirm = false
	m.modal = modalConfirm
	return m, nil
}

func (m *workspaceModel) updateConfirmModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if key == "esc" {
		m.modal = modalNone
		return m, nil
	}
	if key == "tab" || key == "shift+tab" || key == "left" || key == "right" {
		m.actionSession.confirm = !m.actionSession.confirm
		return m, nil
	}
	if key == "enter" {
		if !m.actionSession.confirm {
			m.modal = modalNone
			m.transient = "Cancelled; no changes made"
			return m, nil
		}
		m.modal = modalNone
		return m, m.executeWorkspaceEffect(workspaceEffect{kind: workspaceEffectApplyExposure})
	}
	return m, nil
}

func (m *workspaceModel) updateTerminateProcessModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if key == "esc" {
		m.modal = modalNone
		m.processBatch = nil
		return m, nil
	}
	if key == "tab" || key == "shift+tab" || key == "left" || key == "right" {
		m.confirmFocus = !m.confirmFocus
		return m, nil
	}
	if key != "enter" {
		return m, nil
	}
	if !m.confirmFocus {
		m.modal = modalNone
		m.processBatch = nil
		m.transient = "Cancelled; no process terminated"
		return m, nil
	}
	m.modal = modalNone
	return m, m.executeWorkspaceEffect(workspaceEffect{kind: workspaceEffectStartProcessTermination})
}

func (m *workspaceModel) updateChoiceModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	if key == "esc" {
		m.modal = modalNone
		return m, nil
	}
	if key == "left" || key == "right" || key == "tab" || key == "shift+tab" {
		m.modalChoice = !m.modalChoice
		return m, nil
	}
	if key != "enter" {
		return m, nil
	}
	if !m.modalChoice {
		m.modal = modalNone
		return m, nil
	}
	if m.modal == modalCancel {
		m.modal = modalNone
		return m, m.executeWorkspaceEffect(workspaceEffect{kind: workspaceEffectConfirmCancellation})
	}
	m.modal = modalNone
	return m, m.executeWorkspaceEffect(workspaceEffect{kind: workspaceEffectConfirmQuit})
}

func (m *workspaceModel) openAction(requested *model.ExposureMode) {
	item, ok := m.actionAnchorItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return
	}
	m.modal = modalAction
	target, _ := itemTarget(item)
	mode := observedMode(item)
	if requested != nil {
		mode = *requested
	}
	m.actionSession.open(item.ID, target, mode)
	m.actionSession.capturePreview(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	)
	m.actionSession.refreshChoices(m.actionAvailabilityForItems)
	m.transient = "Preview: " + string(mode)
}

func (m *workspaceModel) refreshActionModal() {
	if m.modal == modalAction {
		m.actionSession.refreshChoices(m.actionAvailabilityForItems)
	}
}

func (m *workspaceModel) invalidatePreviewIfChanged() {
	if m.modal == modalTerminateProcess {
		item, ok := m.selectedItem()
		if !ok || item.ID != m.modalItemID || item.Listener == nil || processFingerprint(*item.Listener) != m.modalProcessFingerprint {
			m.modal = modalNone
			m.setBanner("Selection changed — refresh required", true)
		}
		return
	}
	if m.modal != modalAction && m.modal != modalDisableRoute && m.modal != modalConfirm {
		return
	}
	if !m.actionSession.previewChanged(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	) {
		return
	}
	m.modal = modalNone
	m.setBanner("Selection changed — refresh required", true)
}

func observedMode(item exposure.ReconciledItem) model.ExposureMode {
	if len(item.Routes) == 1 {
		return item.Routes[0].Mode
	}
	return model.ExposureDisabled
}

func (m *workspaceModel) startOperation() tea.Cmd {
	// Refresh lifecycle belongs to the workspace, not the action modal. If a
	// refresh starts after confirmation opened, return to the selector and let
	// the workspace update its supplied choices when fresh state arrives.
	if m.refreshState.isPending() {
		m.modal = modalAction
		m.refreshActionModal()
		return nil
	}
	// Re-evaluate the complete action guard at the mutation boundary. A
	// readiness result can change while the confirmation modal is open without
	// changing route or listener fingerprints; confirmation must never bypass
	// that newer workspace-owned safety decision.
	availability := m.actionAvailabilityForItems(m.actionSession.mode)
	if availability.disabled {
		m.modal = modalAction
		m.refreshActionModal()
		if !availability.wait {
			m.setBanner(availability.reason, true)
		}
		return nil
	}
	if len(m.actionItems()) > 1 {
		return m.startBatchOperation()
	}
	return m.startSingleOperation()
}

func (m *workspaceModel) mutationApproval(item exposure.ReconciledItem, target model.Target) exposure.MutationApproval {
	approval := exposure.MutationApproval{
		Target:           target,
		RouteIDsHash:     exposure.RouteIDsHash(m.view.Exposures.Routes, target),
		TargetRoutesHash: exposure.RouteIdentityHash(m.view.Exposures.Routes, target),
		AllRoutesHash:    tailscale.RoutesHash(m.view.Exposures.Routes),
	}
	if item.Listener != nil {
		listener := *item.Listener
		approval.ListenerID = listener.ID
		approval.ListenerPID = listener.PID
		approval.ListenerProcess = listener.Process
		approval.ListenerStart = listener.ProcessStart
		approval.ListenerCommandLine = listener.CommandLine
		approval.ListenerTarget = listener.Target
	}
	return approval
}

func (m *workspaceModel) startSingleOperation() tea.Cmd {
	item, ok := m.actionAnchorItem()
	if !ok || item.ID != m.actionSession.itemID {
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	if m.actionSession.previewChanged(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	) {
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	key := m.actionSession.target.Key()
	if _, busy := m.activeOps[key]; busy {
		m.setBanner("This target already has an operation Applying", true)
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.activeOps[key] = cancel
	m.markApplying(key)
	mode, confirmExternal := m.actionSession.mode, m.externalPreview()
	confirmFunnel := mode == model.ExposureFunnel
	m.transient = "Applying " + string(mode) + " to " + m.actionSession.target.String()
	target := m.actionSession.target
	routeKey := m.actionSession.routeKey
	controller := m.controller
	timeout := m.cfg.OperationTimeout
	approval := m.mutationApproval(item, target)
	return func() tea.Msg {
		var receipt model.OperationReceipt
		var err error
		if mode == model.ExposureDisabled && routeKey != "" {
			receipt, err = controller.ApplyRouteApproved(ctx, target, routeKey, confirmExternal, timeout, approval)
		} else {
			receipt, err = controller.ApplyApproved(ctx, target, mode, confirmFunnel, confirmExternal, timeout, approval)
		}
		return operationDoneMsg{targetKey: key, receipt: receipt, err: err}
	}
}

func (m *workspaceModel) startBatchOperation() tea.Cmd {
	items := m.actionItems()
	if len(items) <= 1 {
		return m.startSingleOperation()
	}
	if m.actionSession.preview.selectionHash == "" || selectionFingerprint(items) != m.actionSession.preview.selectionHash || routeFingerprint(m.view.Exposures.Routes, nil) != m.actionSession.preview.allRoutesHash {
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	requests := make([]batchOperation, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		target, ok := itemTarget(item)
		if !ok {
			m.setBanner(item.ID+": exact target is unavailable", true)
			return nil
		}
		key := target.Normalized().Key()
		if seen[key] {
			m.setBanner("Selection contains duplicate target identities; refresh and select exact listeners", true)
			return nil
		}
		seen[key] = true
		if _, busy := m.activeOps[key]; busy {
			m.setBanner("Selection contains a target with an operation Applying", true)
			return nil
		}
		if m.sameStateForItem(item, m.actionSession.mode) {
			continue
		}
		providerKey := ""
		if m.actionSession.mode == model.ExposureDisabled && len(item.Routes) == 1 {
			providerKey = item.Routes[0].ProviderKey
		}
		approval := m.mutationApproval(item, target)
		approval.AllowOtherRouteChanges = true
		requests = append(requests, batchOperation{itemID: item.ID, target: target, providerKey: providerKey, confirmExternal: m.externalPreviewFor(item, providerKey), approval: approval})
	}
	if len(requests) == 0 {
		m.modal = modalNone
		m.transient = fmt.Sprintf("Already %s for %d selected service(s)", m.actionSession.mode, len(items))
		return nil
	}
	batchCtx := newBatchContext(m.ctx)
	batchID := model.StableID("batch", string(m.actionSession.mode), time.Now().UTC().Format(time.RFC3339Nano))
	m.batchID, m.batchTotal, m.batchCompleted, m.batchFailures = batchID, len(requests), 0, 0
	m.batchCancel = batchCtx.cancel
	for _, request := range requests {
		key := request.target.Normalized().Key()
		m.activeOps[key] = batchCtx.cancel
		m.markApplying(key)
	}
	m.transient = fmt.Sprintf("Applying %s to %d selected services", m.actionSession.mode, len(requests))
	cmds := make([]tea.Cmd, 0, len(requests))
	mode, confirmFunnel := m.actionSession.mode, m.actionSession.mode == model.ExposureFunnel
	controller := m.controller
	timeout := m.cfg.OperationTimeout
	for index, request := range requests {
		request, index := request, index
		cmds = append(cmds, func() tea.Msg {
			var receipt model.OperationReceipt
			var err error
			if mode == model.ExposureDisabled && request.providerKey != "" {
				receipt, err = controller.ApplyRouteApproved(batchCtx, request.target, request.providerKey, request.confirmExternal, timeout, request.approval)
			} else {
				receipt, err = controller.ApplyApproved(batchCtx, request.target, mode, confirmFunnel, request.confirmExternal, timeout, request.approval)
			}
			return operationDoneMsg{targetKey: request.target.Normalized().Key(), receipt: receipt, err: err, batchID: batchID, batchIndex: index, batchTotal: len(requests)}
		})
	}
	return tea.Sequence(cmds...)
}

func (m *workspaceModel) openTerminateProcess() {
	if m.processBusy {
		m.setBanner("Process termination is already in progress", true)
		return
	}
	targets, reason := m.collectProcessTargets()
	if reason != "" {
		m.setBanner("Terminate unavailable: "+reason, true)
		return
	}
	if len(targets) == 0 {
		m.setBanner("Terminate unavailable: no local listener is selected", true)
		return
	}
	m.processBatch = targets
	first := targets[0]
	m.modal = modalTerminateProcess
	m.modalItemID = first.itemID
	m.modalTarget = first.listener.Target
	m.modalProcess = first.listener
	m.modalProcessFingerprint = first.fingerprint
	m.confirmFocus = false
}

func (m *workspaceModel) startTerminateProcess() tea.Cmd {
	if m.processBusy {
		m.setBanner("Process termination is already in progress", true)
		return nil
	}
	if len(m.processBatch) > 1 {
		m.processBatchIndex = 0
		m.processBatchDone = 0
		m.processBatchFailed = 0
		return m.startNextProcessTermination()
	}
	var target processTarget
	if len(m.processBatch) == 1 {
		target = m.processBatch[0]
		m.processBatch = nil
	} else {
		item, ok := m.selectedItem()
		if !ok || item.ID != m.modalItemID || item.Listener == nil || processFingerprint(*item.Listener) != m.modalProcessFingerprint {
			m.setBanner("Selection changed — refresh required", true)
			return nil
		}
		target = processTarget{itemID: item.ID, listener: *item.Listener, fingerprint: m.modalProcessFingerprint}
	}
	return m.startProcessTarget(target, 0, 0)
}

func (m *workspaceModel) startNextProcessTermination() tea.Cmd {
	if m.processBatchIndex >= len(m.processBatch) {
		return nil
	}
	return m.startProcessTarget(m.processBatch[m.processBatchIndex], m.processBatchIndex, len(m.processBatch))
}

func (m *workspaceModel) startProcessTarget(target processTarget, batchIndex, batchTotal int) tea.Cmd {
	listener, err := m.revalidateProcessTarget(target)
	if err != nil {
		return func() tea.Msg {
			return processDoneMsg{itemID: target.itemID, pid: target.listener.PID, err: err, batchIndex: batchIndex, batchTotal: batchTotal}
		}
	}
	if m.processTerminator == nil {
		err := fmt.Errorf("process termination is unsupported on this platform")
		return func() tea.Msg {
			return processDoneMsg{itemID: target.itemID, pid: listener.PID, err: err, batchIndex: batchIndex, batchTotal: batchTotal}
		}
	}
	ctx, cancel := context.WithTimeout(m.ctx, boundedTimeout(m.cfg.OperationTimeout, 15*time.Second, 10*time.Minute))
	m.processBusy = true
	m.processCancel = cancel
	m.transient = fmt.Sprintf("Terminating process %d (%d/%d)", listener.PID, batchIndex+1, maxInt(1, batchTotal))
	return func() tea.Msg {
		defer cancel()
		return processDoneMsg{itemID: target.itemID, pid: listener.PID, err: m.processTerminator.Terminate(ctx, listener), batchIndex: batchIndex, batchTotal: batchTotal}
	}
}

func (m *workspaceModel) finishProcessBatch(message processDoneMsg) tea.Cmd {
	m.processBatchDone++
	if message.err != nil {
		m.processBatchFailed++
	}
	if message.err != nil && m.quittingAfterCancel {
		m.processUnverified = true
	}
	if m.processBatchDone < message.batchTotal && !m.quittingAfterCancel {
		m.processBatchIndex++
		m.transient = fmt.Sprintf("Terminating services (%d/%d)", m.processBatchDone, message.batchTotal)
		return m.startNextProcessTermination()
	}
	if m.quittingAfterCancel && m.processBatchDone < message.batchTotal {
		m.processUnverified = true
	}
	failed := m.processBatchFailed
	total := message.batchTotal
	m.processBatch = nil
	m.processBatchIndex, m.processBatchDone, m.processBatchFailed = 0, 0, 0
	if failed > 0 {
		m.setBanner(fmt.Sprintf("Process termination completed with %d failure(s) out of %d", failed, total), true)
	} else {
		m.transient = fmt.Sprintf("Terminated %d process(es)", total)
	}
	if m.quittingAfterCancel {
		if !m.hasPendingOperations() {
			m.quittingAfterCancel = false
			m.cancel()
			return tea.Quit
		}
		return nil
	}
	return m.startRefresh()
}

func (m *workspaceModel) openCancel() {
	item, ok := m.selectedItem()
	if !ok {
		return
	}
	target, ok := itemTarget(item)
	if !ok {
		return
	}
	if _, busy := m.activeOps[target.Key()]; !busy {
		m.transient = "No Applying operation for this target"
		return
	}
	m.modal, m.modalTarget, m.modalChoice = modalCancel, target, false
}

func (m *workspaceModel) quitCommand() tea.Cmd {
	if !m.hasPendingOperations() {
		m.cancel()
		return tea.Quit
	}
	m.modal, m.modalChoice = modalQuit, false
	return nil
}

func (m *workspaceModel) executePalette(query string) tea.Cmd {
	query = strings.TrimSpace(query)
	query = strings.TrimPrefix(query, ":")
	parts := strings.Fields(query)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "refresh":
		if len(parts) != 1 {
			m.setBanner("Usage: :refresh", true)
			return nil
		}
		return m.startRefresh()
	case "retry":
		if len(parts) != 1 {
			m.setBanner("Usage: :retry", true)
			return nil
		}
		return m.startRetry()
	case "help":
		if len(parts) != 1 {
			m.setBanner("Usage: :help", true)
			return nil
		}
		m.modal, m.helpOffset = modalHelp, 0
	case "quit":
		if len(parts) != 1 {
			m.setBanner("Usage: :quit", true)
			return nil
		}
		return m.quitCommand()
	case "serve", "funnel", "disable":
		if len(parts) > 2 {
			m.setBanner("Usage: :"+parts[0]+" selected|address:port", true)
			return nil
		}
		mode := map[string]model.ExposureMode{"serve": model.ExposureServe, "funnel": model.ExposureFunnel, "disable": model.ExposureDisabled}[parts[0]]
		if len(parts) == 2 && parts[1] != "selected" {
			target, err := model.ParseTarget(parts[1], "tcp")
			if err != nil {
				m.setBanner("Invalid palette target", true)
				return nil
			}
			matches := []exposure.ReconciledItem{}
			for _, item := range m.items() {
				candidate, ok := itemTarget(item)
				if ok && exposure.Matches(candidate, target) {
					matches = append(matches, item)
				}
			}
			if len(matches) != 1 {
				m.setBanner("Palette target is ambiguous or unavailable", true)
				return nil
			}
			m.selectedID = matches[0].ID
			m.reselect(m.selectedID, 0)
		}
		m.openAction(&mode)
	case "sort":
		if len(parts) != 2 {
			m.setBanner("Usage: :sort port|name|address|exposure", true)
			return nil
		}
		candidate := m.cfg
		if err := config.SetValue(&candidate, "sort", parts[1]); err != nil {
			m.setBanner(safeMessage(err), true)
			return nil
		}
		m.cfg = candidate
		return saveConfigCmd(m.ctx, m.manager, candidate)
	case "config":
		if len(parts) == 2 && parts[1] == "show" {
			m.setBanner(m.cfg.YAML(), true)
			return nil
		}
		if len(parts) == 2 && parts[1] == "validate" {
			return validateConfigCmd(m.ctx, m.manager)
		}
		if len(parts) == 4 && parts[1] == "set" {
			candidate := m.cfg
			if err := config.SetValue(&candidate, parts[2], parts[3]); err != nil {
				m.setBanner("Config: "+safeMessage(err), true)
				return nil
			}
			m.cfg = candidate
			return saveConfigCmd(m.ctx, m.manager, candidate)
		}
		m.setBanner("Palette config commands: :config show, :config set key value, or :config validate", true)
	default:
		m.setBanner("Unknown palette command", true)
	}
	return nil
}

func (m *workspaceModel) startRetry() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok || (item.OperationState != model.ExposureFailed && item.OperationState != model.ExposureUnverified && item.OperationState != model.ExposureCancelled) {
		m.transient = "Retry is available only after a failed, cancelled, or unverified operation"
		return nil
	}
	target, targetOK := itemTarget(item)
	if !targetOK {
		m.transient = "Retry is unavailable without an exact target"
		return nil
	}
	if _, busy := m.activeOps[target.Key()]; busy {
		m.transient = "Retry is disabled while Applying"
		return nil
	}
	m.refreshState.requestRetryPreview()
	m.transient = "Refreshing before retry preview..."
	return m.startRefresh()
}

func validateConfigCmd(ctx context.Context, manager config.Manager) tea.Cmd {
	return func() tea.Msg {
		cfg, _, _, err := manager.Load(ctx)
		return configValidatedMsg{cfg: cfg, err: err}
	}
}

func saveConfigCmd(ctx context.Context, manager config.Manager, cfg config.Config) tea.Cmd {
	return func() tea.Msg {
		return configSavedMsg{cfg: cfg, err: manager.Save(ctx, cfg)}
	}
}

func (m *workspaceModel) filteredPalette() []string {
	query := strings.ToLower(strings.TrimSpace(m.paletteInput.Value()))
	result := []string{}
	for _, command := range paletteCommands {
		if query == "" || strings.Contains(strings.ToLower(command), query) {
			result = append(result, command)
		}
	}
	return result
}
func (m *workspaceModel) nextPaletteIndex(delta int) int {
	items := m.filteredPalette()
	if len(items) == 0 {
		return 0
	}
	index := (m.paletteIndex + delta) % len(items)
	if index < 0 {
		index += len(items)
	}
	return index
}

func (m *workspaceModel) urlShortcutStatus() string {
	observed, tcpOnly, browserFallback, local := false, false, false, false
	if item, ok := m.selectedItem(); ok {
		for _, route := range item.Routes {
			if _, ok := observedRouteURL(route); ok {
				observed = true
				continue
			}
			if routeTransport(route) == "tcp" {
				if route.Mode == model.ExposureServe && m.provider != nil {
					browserFallback = true
				} else {
					tcpOnly = true
				}
			}
		}
		_, local = localURL(item)
	}
	clipboard := m.clipboard != nil
	if !clipboard {
		clipboard = clipboardAvailable()
	}
	return formatURLShortcutStatus(observed, tcpOnly, browserFallback, local, browserCommandAvailable(), clipboard)
}

func formatURLShortcutStatus(observed, tcpOnly, browserFallback, local, browser, clipboard bool) string {
	observedStatus := "off"
	if (observed || browserFallback) && browser {
		observedStatus = "ok"
	} else if tcpOnly && !observed && !browserFallback {
		observedStatus = "TCP-only"
	}
	localStatus := "off"
	if local && browser {
		localStatus = "ok"
	}
	copyStatus := "off"
	if observed || browserFallback {
		if clipboard {
			copyStatus = "ok"
		} else {
			copyStatus = "URL-only"
		}
	}
	return fmt.Sprintf("o observed[%s]  O localhost[%s]  y copy[%s]", observedStatus, localStatus, copyStatus)
}

func browserCommand() string {
	name := ""
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func browserCommandAvailable() bool {
	return browserCommand() != ""
}

func (m *workspaceModel) openSelectedURL() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return nil
	}
	for _, route := range item.Routes {
		if url, ok := observedRouteURL(route); ok {
			return m.openURL(url, "observed URL")
		}
	}
	for _, route := range item.Routes {
		if routeTransport(route) == "tcp" && route.Mode == model.ExposureServe {
			return m.resolveServeTCPBrowserURL(route)
		}
	}
	if selector := rawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no browser URL exists. Use a TCP client.", true)
		return nil
	}
	m.setBanner("No observed exposure URL is available", true)
	return nil
}

func (m *workspaceModel) resolveServeTCPBrowserURL(route model.ExposureRoute) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Observed Serve route has no provider endpoint resolver", true)
		return nil
	}
	m.transient = "Resolving observed Tailscale endpoint"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		status, err := m.provider.Status(ctx)
		if err != nil {
			return resolvedObservedURLMsg{err: err}
		}
		url, err := serveTCPBrowserURL(status, route)
		return resolvedObservedURLMsg{url: url, err: err}
	}
}

func (m *workspaceModel) openSelectedLocalURL() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return nil
	}
	url, ok := localURL(item)
	if !ok {
		m.setBanner("No current local listener is available", true)
		return nil
	}
	return m.openURL(url, "local URL")
}

func localURL(item exposure.ReconciledItem) (string, bool) {
	if item.Listener == nil || item.Listener.Target.Port < 1 || item.Listener.Target.Port > 65535 {
		return "", false
	}
	return "http://localhost:" + strconv.Itoa(item.Listener.Target.Port) + "/", true
}

func (m *workspaceModel) openURL(url, label string) tea.Cmd {
	name := browserCommand()
	if name == "" {
		m.setBanner("Opening URLs is unsupported or unavailable on "+runtime.GOOS, true)
		return nil
	}
	m.transient = "Opening " + label
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, name, url)
		command.WaitDelay = 2 * time.Second
		err := command.Run()
		if err != nil {
			return statusMsg{value: "Could not open URL: " + safeMessage(err), sticky: true}
		}
		return statusMsg{value: "Opened " + label, sticky: false}
	}
}

type resolvedObservedURLMsg struct {
	url string
	err error
}

type statusMsg struct {
	value  string
	sticky bool
}

func (m *workspaceModel) copyURL() tea.Cmd {
	items := m.items()
	if m.selectedIdx < 0 || m.selectedIdx >= len(items) {
		m.setBanner("No service is selected", true)
		return nil
	}
	item := items[m.selectedIdx]
	for _, route := range item.Routes {
		if url, ok := observedRouteURL(route); ok {
			return m.copyURLCommand(url)
		}
	}
	for _, route := range item.Routes {
		if routeTransport(route) == "tcp" && route.Mode == model.ExposureServe {
			return m.resolveServeTCPCopyURL(route)
		}
	}
	if selector := rawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no browser URL exists to copy", true)
		return nil
	}
	m.setBanner("No observed exposure URL is available", true)
	return nil
}

func (m *workspaceModel) copyURLCommand(url string) tea.Cmd {
	clipboard := m.clipboard
	if clipboard == nil {
		clipboard = OSClipboard{}
	}
	m.transient = "Copying observed URL"
	return func() tea.Msg {
		return copyURLStatus(url, clipboard)
	}
}

func (m *workspaceModel) resolveServeTCPCopyURL(route model.ExposureRoute) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Observed Serve route has no provider endpoint resolver", true)
		return nil
	}
	clipboard := m.clipboard
	if clipboard == nil {
		clipboard = OSClipboard{}
	}
	m.transient = "Resolving observed Tailscale endpoint"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		status, err := m.provider.Status(ctx)
		if err != nil {
			return statusMsg{value: "Observed URL: " + safeMessage(err), sticky: true}
		}
		url, err := serveTCPBrowserURL(status, route)
		if err != nil {
			return statusMsg{value: "Observed URL: " + safeMessage(err), sticky: true}
		}
		return copyURLStatus(url, clipboard)
	}
}

func copyURLStatus(url string, clipboard Clipboard) statusMsg {
	var out, errOut strings.Builder
	copyURLValue(&out, &errOut, url, clipboard)
	if text := strings.TrimSpace(errOut.String()); text != "" {
		value := strings.TrimSpace(out.String())
		if value != "" {
			value += " "
		}
		return statusMsg{value: value + text, sticky: true}
	}
	return statusMsg{value: strings.TrimSpace(out.String())}
}

func (m *workspaceModel) setBanner(value string, sticky bool) {
	m.banner, m.bannerSticky = sanitizeTUIText(value), sticky
}

func (m *workspaceModel) appendBanner(value string, sticky bool) {
	value = sanitizeTUIText(value)
	if value == "" || strings.Contains(m.banner, value) {
		m.bannerSticky = m.bannerSticky || sticky
		return
	}
	if m.banner == "" {
		m.banner = value
	} else {
		m.banner += " | " + value
	}
	m.bannerSticky = m.bannerSticky || sticky
}
func safeMessage(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeTUIText(model.AsAppError(err).Message)
}

func (m *workspaceModel) applyStatus(message statusMsg) {
	if message.sticky {
		m.setBanner(message.value, true)
	} else {
		m.transient = message.value
	}
}

func modeStatus(readiness model.Readiness, wanted model.ExposureMode) model.ReadinessStatus {
	for _, mode := range readiness.Modes {
		if mode.Mode == wanted {
			return mode.Status
		}
	}
	return model.ReadinessUnknown
}

func readinessActionReason(readiness model.Readiness, wanted model.ExposureMode) string {
	for _, mode := range readiness.Modes {
		if mode.Mode != wanted {
			continue
		}
		for _, check := range mode.Checks {
			if check.Status == model.ReadinessReady {
				continue
			}
			reason := fmt.Sprintf("%s — %s: %s", wanted, mode.Status, check.Message)
			if check.Remediation != "" {
				reason += " Next: " + check.Remediation
			}
			return reason
		}
		return fmt.Sprintf("%s — %s", wanted, mode.Status)
	}
	return fmt.Sprintf("%s — %s", wanted, model.ReadinessUnknown)
}

func paint(theme, code, value string) string {
	if (theme != "dark" && theme != "light" && theme != "auto") || os.Getenv("NO_COLOR") != "" {
		return value
	}
	if theme == "auto" {
		theme = "dark"
	}
	if theme == "light" {
		if code == "1;36" {
			code = "1;34"
		} else if code == "33" {
			code = "1;31"
		}
	}
	return "\x1b[" + code + "m" + value + "\x1b[0m"
}

func copySelectedURL(out, errOut io.Writer, items []exposure.ReconciledItem, selected int, clipboard Clipboard) {
	if selected < 0 || selected >= len(items) {
		return
	}
	for _, route := range items[selected].Routes {
		if url, ok := observedRouteURL(route); ok {
			copyURLValue(out, errOut, url, clipboard)
			return
		}
	}
	fmt.Fprintln(out, "No URL is available for the selected item.")
}

func copyURLValue(out, errOut io.Writer, url string, clipboard Clipboard) {
	fmt.Fprintf(out, "URL: %s\n", url)
	if clipboard == nil {
		fmt.Fprintln(errOut, "clipboard unavailable: no clipboard integration is configured.\nCopy the visible URL with normal terminal text selection.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := clipboard.Copy(ctx, url)
	cancel()
	if err != nil {
		fmt.Fprintf(errOut, "clipboard unavailable: %s\nCopy the visible URL with normal terminal text selection.\n", err)
	} else {
		fmt.Fprintln(out, "URL copied to clipboard.")
	}
}

type Clipboard interface {
	Copy(context.Context, string) error
}

// OSC52Clipboard copies through the terminal stream instead of the operating
// system clipboard on the machine running Tailge. This is what makes copying
// work for SSH, Mosh, and terminal multiplexers: the terminal client receives
// the sequence and updates its own clipboard.
type OSC52Clipboard struct {
	Output io.Writer
	Mode   osc52.Mode
	mu     sync.Mutex
}

func (c *OSC52Clipboard) Copy(ctx context.Context, value string) error {
	if c == nil || c.Output == nil {
		return fmt.Errorf("terminal output is unavailable")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := osc52.New(value).Mode(c.Mode).WriteTo(c.Output)
	return err
}

func terminalClipboard(out io.Writer) Clipboard {
	if out == nil {
		return nil
	}
	return &OSC52Clipboard{Output: out, Mode: terminalClipboardMode()}
}

func terminalClipboardMode() osc52.Mode {
	// GNU screen does not pass OSC 52 directly, so wrap it in DCS. tmux can
	// handle the normal OSC 52 sequence when set-clipboard is enabled; using
	// TmuxMode unconditionally would instead require allow-passthrough.
	term := os.Getenv("TERM")
	if os.Getenv("STY") != "" || (strings.HasPrefix(term, "screen") && os.Getenv("TMUX") == "") {
		return osc52.ScreenMode
	}
	return osc52.DefaultMode
}

type OSClipboard struct{}

func clipboardCommand() (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("pbcopy"); err == nil {
			return path, nil
		}
	case "linux":
		if path, err := exec.LookPath("wl-copy"); err == nil {
			return path, nil
		} else if path, err := exec.LookPath("xclip"); err == nil {
			return path, []string{"-selection", "clipboard"}
		}
	}
	return "", nil
}

func clipboardAvailable() bool {
	name, _ := clipboardCommand()
	return name != ""
}

func (OSClipboard) Copy(ctx context.Context, value string) error {
	name, args := clipboardCommand()
	if name == "" {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("clipboard is unsupported on %s", runtime.GOOS)
		}
		return fmt.Errorf("no supported clipboard command was found")
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(value)
	return command.Run()
}

type ClipboardFunc func(context.Context, string) error

func (f ClipboardFunc) Copy(ctx context.Context, value string) error {
	if f == nil {
		return fmt.Errorf("clipboard function is nil")
	}
	return f(ctx, value)
}

func sanitizeTUIText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}
func truncate(value string, max int) string {
	if max < 4 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-3]) + "..."
}
func valueOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// Run enters the full-screen Bubble Tea application. Bubble Tea owns the raw
// terminal and alternate-screen lifecycle, including restoration on errors,
// interrupts, and normal quit. No startup status is written outside it.
func Run(in io.Reader, out, errOut io.Writer, discoverer discovery.ListenerObserver, processTerminator discovery.ProcessTerminator, provider *tailscale.Adapter, manager config.Manager) int {
	if in == nil {
		fmt.Fprintln(errOut, "error: TUI input is unavailable")
		return model.ErrInterrupted.ExitCode()
	}
	m := newWorkspaceModel(discoverer, processTerminator, provider, manager)
	clipboardOutput := errOut
	if clipboardOutput == nil {
		clipboardOutput = out
	}
	m.clipboard = terminalClipboard(clipboardOutput)
	program := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out), tea.WithAltScreen())
	finalModel, err := program.Run()
	m.cancel()
	if err != nil {
		fmt.Fprintf(errOut, "error: TUI stopped: %s\n", sanitizeTUIText(err.Error()))
		return model.ErrInterrupted.ExitCode()
	}
	if final, ok := finalModel.(*workspaceModel); ok {
		unverified := len(final.activeOps) > 0 || final.processBusy || final.processUnverified
		if !unverified {
			for _, item := range final.view.Items {
				if item.OperationState == model.ExposureUnverified {
					unverified = true
					break
				}
			}
		}
		if unverified {
			fmt.Fprintln(errOut, "warning: operation state is unverified; inspect Tailscale after shutdown")
		}
	}
	return 0
}
