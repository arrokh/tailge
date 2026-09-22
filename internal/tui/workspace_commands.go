package tui

// Workspace cancellation, quit, retry, palette commands, and configuration commands.

import (
	"context"
	"strings"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
	tea "github.com/charmbracelet/bubbletea"
)

var paletteCommands = []string{":refresh", ":retry", ":serve selected", ":funnel selected", ":disable selected", ":sort port", ":sort name", ":config show", ":config set key value", ":config validate", ":help", ":quit"}

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
		mode := map[string]exposuredata.ExposureMode{"serve": exposuredata.ExposureServe, "funnel": exposuredata.ExposureFunnel, "disable": exposuredata.ExposureDisabled}[parts[0]]
		if len(parts) == 2 && parts[1] != "selected" {
			target, err := target.ParseTarget(parts[1], "tcp")
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
	if !ok || (item.OperationState != exposuredata.ExposureFailed && item.OperationState != exposuredata.ExposureUnverified && item.OperationState != exposuredata.ExposureCancelled) {
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
