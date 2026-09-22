package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
	tea "github.com/charmbracelet/bubbletea"
)

func refresh(parent context.Context, controller *exposure.Controller, timeout time.Duration) (exposure.View, error) {
	if controller == nil {
		return exposure.View{}, fault.NewError(fault.ErrDependency, "exposure", "exposure controller is unavailable", true, "unavailable", "Configure providers and retry.")
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
	snapshot := exposuredata.ExposureSnapshot{At: time.Now(), Authoritative: false, Source: "unavailable"}
	if err != nil {
		safe := fault.AsAppError(err).Safe()
		snapshot.Error = &safe
	}
	if discoveryErr != nil {
		safe := fault.AsAppError(discoveryErr).Safe()
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

func readinessFor(parent context.Context, provider *tailscale.Adapter, cfg config.Config, options tailscale.ReadinessOptions) (readiness.Readiness, error) {
	if provider == nil {
		return readiness.Readiness{At: time.Now(), Status: readiness.ReadinessNotReady, Modes: []readiness.ModeReadiness{
			{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessNotReady},
			{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessNotReady},
		}}, fault.NewError(fault.ErrDependency, "tui", "Tailscale provider is unavailable", true, "unavailable", "Install Tailscale and retry.")
	}
	ctx, cancel := context.WithTimeout(parent, boundedTimeout(cfg.OperationTimeout, 15*time.Second, 10*time.Minute))
	defer cancel()
	return provider.Readiness(ctx, options)
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
			viewErr = fault.NewError(fault.ErrUnknown, "tui", "refresh returned no snapshot", true, "unknown", "Refresh again before changing exposure.")
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

// Run enters the full-screen Bubble Tea application. Bubble Tea owns the raw
// terminal and alternate-screen lifecycle, including restoration on errors,
// interrupts, and normal quit. No startup status is written outside it.
func Run(in io.Reader, out, errOut io.Writer, discoverer discovery.ListenerObserver, processTerminator discovery.ProcessTerminator, provider *tailscale.Adapter, manager config.Manager) int {
	if in == nil {
		fmt.Fprintln(errOut, "error: TUI input is unavailable")
		return fault.ErrInterrupted.ExitCode()
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
		if errors.Is(err, tea.ErrInterrupted) {
			reportUnverifiedTUIState(errOut, finalModel)
			return fault.ErrInterrupted.ExitCode()
		}
		return reportTUIStop(errOut, err)
	}
	reportUnverifiedTUIState(errOut, finalModel)
	return 0
}

func reportUnverifiedTUIState(errOut io.Writer, finalModel tea.Model) {
	final, ok := finalModel.(*workspaceModel)
	if !ok {
		return
	}
	unverified := len(final.activeOps) > 0 || final.processBusy || final.processUnverified
	if !unverified {
		for _, item := range final.view.Items {
			if item.OperationState == exposuredata.ExposureUnverified {
				unverified = true
				break
			}
		}
	}
	if unverified {
		fmt.Fprintln(errOut, "warning: operation state is unverified; inspect Tailscale after shutdown")
	}
}

func reportTUIStop(errOut io.Writer, err error) int {
	if !errors.Is(err, tea.ErrInterrupted) {
		fmt.Fprintf(errOut, "error: TUI stopped: %s\n", sanitizeTUIText(err.Error()))
	}
	return fault.ErrInterrupted.ExitCode()
}
