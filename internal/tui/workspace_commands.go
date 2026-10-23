package tui

// Workspace cancellation, quit, retry, palette commands, and configuration commands.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
	tea "github.com/charmbracelet/bubbletea"
)

var paletteCommands = []string{":refresh", ":retry", ":serve selected", ":funnel selected", ":disable selected", ":media serve DIRECTORY...", ":skill install", ":skill uninstall", ":sort name", ":sort name-desc", ":sort none", ":sort port", ":sort address", ":sort exposure", ":config show", ":config set key value", ":config validate", ":help", ":quit"}

func nextWorkspaceSort(current string) string {
	switch current {
	case "name":
		return "name-desc"
	case "name-desc":
		return "none"
	default:
		return "name"
	}
}

func sortDescription(sortKey string) string {
	switch sortKey {
	case "name":
		return "Name ascending"
	case "name-desc":
		return "Name descending"
	case "none":
		return "Unsorted"
	case "address":
		return "Address ascending"
	case "exposure":
		return "Exposure ascending"
	default:
		return "Port ascending"
	}
}

func (m *workspaceModel) cycleSort() tea.Cmd {
	previousID, previousIndex := m.selectedID, m.selectedIdx
	candidate := m.cfg
	candidate.Sort = nextWorkspaceSort(candidate.Sort)
	m.cfg = candidate
	m.reselect(previousID, previousIndex)
	if m.visualSelection {
		m.updateVisualSelection()
	}
	m.transient = "Sort: " + sortDescription(candidate.Sort)
	return saveConfigCmd(m.ctx, m.manager, candidate)
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
	parts, err := splitPaletteWords(query)
	if err != nil {
		m.setBanner("Invalid command quoting: "+err.Error(), true)
		return nil
	}
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
	case "media":
		if len(parts) < 3 || parts[1] != "serve" {
			m.setBanner("Usage: :media serve DIRECTORY... (Ctrl-C stops the server and returns to the workspace)", true)
			return nil
		}
		args := append([]string{"media", "serve"}, parts[2:]...)
		return m.runTailgeSubcommand("Media server stopped; returned to the workspace", args...)
	case "skill":
		if len(parts) != 2 {
			m.setBanner("Usage: :skill install|uninstall", true)
			return nil
		}
		switch parts[1] {
		case "install":
			return m.runTailgeSubcommand("Tailge agent skill installation finished", "skill", "install")
		case "uninstall":
			m.modal, m.modalChoice = modalSkillUninstall, false
		default:
			m.setBanner("Usage: :skill install|uninstall", true)
		}
	case "sort":
		if len(parts) != 2 {
			m.setBanner("Usage: :sort name|name-desc|none|port|address|exposure", true)
			return nil
		}
		candidate := m.cfg
		if err := config.SetValue(&candidate, "sort", parts[1]); err != nil {
			m.setBanner(safeMessage(err), true)
			return nil
		}
		previousID, previousIndex := m.selectedID, m.selectedIdx
		m.cfg = candidate
		m.reselect(previousID, previousIndex)
		if m.visualSelection {
			m.updateVisualSelection()
		}
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

func splitPaletteWords(input string) ([]string, error) {
	words := make([]string, 0)
	var current strings.Builder
	var quote rune
	escaped, started := false, false
	for _, character := range input {
		if escaped {
			current.WriteRune(character)
			escaped, started = false, true
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped, started = true, true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			continue
		}
		switch {
		case character == '\'' || character == '"':
			quote, started = character, true
		case unicode.IsSpace(character):
			if started {
				words = append(words, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(character)
			started = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("command ends with an escape character")
	}
	if quote != 0 {
		return nil, fmt.Errorf("command has an unclosed quote")
	}
	if started {
		words = append(words, current.String())
	}
	return words, nil
}

func tailgeExecCommand(args ...string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.Command(executable, args...), nil
}

func (m *workspaceModel) runTailgeSubcommand(success string, args ...string) tea.Cmd {
	command, err := tailgeExecCommand(args...)
	if err != nil {
		return func() tea.Msg { return paletteSubcommandDoneMsg{success: success, err: err} }
	}
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return paletteSubcommandDoneMsg{success: success, err: err}
	})
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
