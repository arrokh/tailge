package tui

// Keyboard scope, search, command-palette input, and modal transitions for the Service workspace.

import (
	"fmt"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/workspace"
	textinput "github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

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
	if effect, handled := workspace.EffectForKey(key); handled {
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
		m.openAction(ptrMode(exposuredata.ExposureServe))
	case "f":
		m.openAction(ptrMode(exposuredata.ExposureFunnel))
	case "d":
		m.openAction(ptrMode(exposuredata.ExposureDisabled))
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

func ptrMode(mode exposuredata.ExposureMode) *exposuredata.ExposureMode { return &mode }

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
		if !workspace.SameStateForItem(m.view, item, mode) {
			allSame = false
			break
		}
	}
	if allSame {
		m.modal = modalNone
		if len(items) == 1 {
			m.transient = workspace.AlreadyMessage(mode)
		} else {
			m.transient = fmt.Sprintf("Already %s for %d selected service(s)", mode, len(items))
		}
		return m, nil
	}
	if mode == exposuredata.ExposureDisabled && len(items) == 1 {
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
	return m.beginConfirmation(exposuredata.ExposureDisabled)
}

func (m *workspaceModel) beginConfirmation(mode exposuredata.ExposureMode) (tea.Model, tea.Cmd) {
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
		return m, m.executeWorkspaceEffect(workspace.Effect{Kind: workspace.EffectApplyExposure})
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
	return m, m.executeWorkspaceEffect(workspace.Effect{Kind: workspace.EffectStartProcessTermination})
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
		return m, m.executeWorkspaceEffect(workspace.Effect{Kind: workspace.EffectConfirmCancellation})
	}
	m.modal = modalNone
	return m, m.executeWorkspaceEffect(workspace.Effect{Kind: workspace.EffectConfirmQuit})
}
