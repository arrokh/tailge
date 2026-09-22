package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// executeWorkspaceEffect is the only adapter from pure workspace decisions to
// Bubble Tea commands. Keeping this mapping explicit makes it possible to test
// key decisions without starting a terminal or invoking the OS.
func (m *workspaceModel) executeWorkspaceEffect(effect workspaceEffect) tea.Cmd {
	switch effect.kind {
	case workspaceEffectRefresh:
		return m.startRefresh()
	case workspaceEffectRetry:
		return m.startRetry()
	case workspaceEffectOpenObservedURL:
		return m.openSelectedURL()
	case workspaceEffectOpenLocalURL:
		return m.openSelectedLocalURL()
	case workspaceEffectCopyURL:
		return m.copyURL()
	case workspaceEffectApplyExposure:
		return m.startOperation()
	case workspaceEffectCancelOperation:
		m.openCancel()
		return nil
	case workspaceEffectTerminateProcess:
		m.openTerminateProcess()
		return nil
	case workspaceEffectStartProcessTermination:
		return m.startTerminateProcess()
	case workspaceEffectConfirmCancellation:
		return m.confirmOperationCancellation()
	case workspaceEffectConfirmQuit:
		return m.confirmQuit()
	case workspaceEffectQuit:
		return m.quitCommand()
	default:
		return nil
	}
}

func (m *workspaceModel) confirmOperationCancellation() tea.Cmd {
	if cancel, ok := m.activeOps[m.modalTarget.Key()]; ok {
		if cancel != nil {
			cancel()
		}
		m.setBanner("Cancellation requested; final state will be verified", false)
	}
	return nil
}

func (m *workspaceModel) confirmQuit() tea.Cmd {
	// Quitting with an active operation waits briefly for each cancelled
	// provider command to report its terminal state. This keeps the workspace
	// alive long enough to record cancellation as Unverified instead of
	// discarding the operation when Bubble Tea exits immediately.
	for _, cancel := range m.activeOps {
		if cancel != nil {
			cancel()
		}
	}
	if m.processCancel != nil {
		m.processCancel()
	}
	m.quittingAfterCancel = true
	m.quitGeneration++
	generation := m.quitGeneration
	m.setBanner("Operation cancelled; verifying final provider state before quit", true)
	return tea.Tick(quitCancelGraceTime, func(time.Time) tea.Msg { return quitAfterCancelMsg{generation: generation} })
}
