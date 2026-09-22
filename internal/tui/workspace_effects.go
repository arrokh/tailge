package tui

import (
	"time"

	"github.com/arrokh/tailge/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
)

const quitCancelGraceTime = 2 * time.Second

// executeWorkspaceEffect is the only adapter from pure workspace decisions to
// Bubble Tea commands. Keeping this mapping explicit makes it possible to test
// key decisions without starting a terminal or invoking the OS.
func (m *workspaceModel) executeWorkspaceEffect(effect workspace.Effect) tea.Cmd {
	switch effect.Kind {
	case workspace.EffectRefresh:
		return m.startRefresh()
	case workspace.EffectRetry:
		return m.startRetry()
	case workspace.EffectOpenObservedURL:
		return m.openSelectedURL()
	case workspace.EffectOpenLocalURL:
		return m.openSelectedLocalURL()
	case workspace.EffectCopyURL:
		return m.copyURL()
	case workspace.EffectApplyExposure:
		return m.startOperation()
	case workspace.EffectCancelOperation:
		m.openCancel()
		return nil
	case workspace.EffectTerminateProcess:
		m.openTerminateProcess()
		return nil
	case workspace.EffectStartProcessTermination:
		return m.startTerminateProcess()
	case workspace.EffectConfirmCancellation:
		return m.confirmOperationCancellation()
	case workspace.EffectConfirmQuit:
		return m.confirmQuit()
	case workspace.EffectQuit:
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
