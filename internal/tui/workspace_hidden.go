package tui

import (
	"strings"

	"github.com/arrokh/tailge/internal/exposure"
)

type processVisibility uint8

const (
	hideListedProcesses processVisibility = iota
	showAllProcesses
	showHiddenProcesses
)

func (v processVisibility) label() string {
	switch v {
	case showAllProcesses:
		return "ALL"
	case showHiddenProcesses:
		return "HIDDEN ONLY"
	default:
		return "VISIBLE"
	}
}

func processHideKey(item exposure.ReconciledItem) string {
	if item.Listener == nil {
		return ""
	}
	return strings.TrimSpace(item.Listener.Process)
}

func (m *workspaceModel) toggleHiddenProcess() {
	item, ok := m.selectedItem()
	name := processHideKey(item)
	if !ok || name == "" {
		m.setBanner("Hide unavailable: select a listener with a known process name", true)
		return
	}
	if m.hiddenProcesses[name] {
		delete(m.hiddenProcesses, name)
		m.transient = "Unhidden: " + sanitizeTUIText(name)
	} else {
		if m.hiddenProcesses == nil {
			m.hiddenProcesses = make(map[string]bool)
		}
		m.hiddenProcesses[name] = true
		m.transient = "Hidden: " + sanitizeTUIText(name) + " · I to show"
	}
	m.updateProcessVisibility()
}

func (m *workspaceModel) cycleProcessVisibility() {
	m.hiddenView = (m.hiddenView + 1) % (showHiddenProcesses + 1)
	m.updateProcessVisibility()
}

func (m *workspaceModel) updateProcessVisibility() {
	m.reselect(m.selectedID, m.selectedIdx)
	m.pruneSelection()
	m.updateVisualSelection()
	m.clampOffsets()
}
