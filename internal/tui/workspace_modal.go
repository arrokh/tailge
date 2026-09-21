package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/arrokh/tailge/internal/model"
)

func (m *workspaceModel) modalView() string {
	modalWidth := maxInt(20, minInt(m.width-8, 86))
	var title string
	var lines []string
	switch m.modal {
	case modalHelp:
		title = "HELP"
		all := helpLines()
		page := m.helpViewport()
		start := clamp(m.helpOffset, 0, m.helpMaxOffset())
		end := minInt(len(all), start+page)
		lines = append(lines, all[start:end]...)
		lines = append(lines, "", fmt.Sprintf("Lines %d-%d/%d · ↑/↓ or j/k scroll · PgUp/PgDn · Home/End · Esc or ? close", start+1, end, len(all)))
	case modalPalette:
		title = "COMMAND PALETTE"
		lines = append(lines, "Type a command or select one:", "", "  "+m.paletteInput.View())
		for i, command := range m.filteredPalette() {
			marker := "  "
			if i == m.paletteIndex {
				marker = "> "
			}
			lines = append(lines, marker+command)
		}
		lines = append(lines, "", "↑/↓ select · Enter run · Esc cancel")
	case modalAction:
		title = "EXPOSURE ACTION"
		items := m.actionItems()
		if len(items) > 1 {
			lines = append(lines, fmt.Sprintf("Selected: %d services", len(items)))
		}
		lines = append(lines, "Target: "+m.actionSession.target.String())
		warning := ""
		if item, ok := m.actionAnchorItem(); ok {
			if item.Warning != "" {
				warning = "Warning: " + sanitizeTUIText(item.Warning)
			}
		}
		// Reserve one bounded warning row even when no warning is present. This
		// keeps the action dialog's height stable as refresh results arrive.
		lines = append(lines, truncate(warning, maxInt(16, modalWidth-4)))
		if item, ok := m.actionAnchorItem(); ok {
			if len(item.Routes) == 0 {
				lines = append(lines, "Observed route: none")
			} else {
				for _, route := range item.Routes {
					lines = append(lines, fmt.Sprintf("Observed route: %s %s selector=%s", route.Mode, route.Target.String(), valueOr(route.ProviderKey, "unavailable")))
				}
			}
		}
		lines = append(lines, "", "Choose one action:")
		choices := m.actionSession.choices
		for i, choice := range choices {
			marker := "  "
			if i == m.actionSession.index {
				marker = "> "
			}
			label := choice.label
			if choice.mode == model.ExposureDisabled {
				if item, ok := m.actionAnchorItem(); ok && len(item.Routes) > 1 {
					label += " (choose exact route)"
				}
			}
			if choice.disabled {
				// Detailed reasons are reported when the choice is activated. A
				// fixed-width marker here prevents refresh/readiness changes from
				// resizing and recentering the modal.
				label += " [unavailable]"
			}
			lines = append(lines, marker+label)
		}
		lines = append(lines, "", "↑/↓ or j/k select · Enter preview · Esc cancel")
	case modalDisableRoute:
		title = "CHOOSE ROUTE TO DISABLE"
		lines = append(lines, "Target: "+m.actionSession.target.String(), "Select exactly one observed route:")
		if item, ok := m.actionAnchorItem(); ok {
			for i, route := range item.Routes {
				marker := "  "
				if i == m.disableRouteIndex {
					marker = "> "
				}
				ownership := string(route.Ownership)
				if ownership == "" {
					ownership = "unknown"
				}
				lines = append(lines, marker+string(route.Mode)+" selector="+valueOr(route.ProviderKey, "unavailable")+" ownership="+ownership)
			}
		}
		lines = append(lines, "", "This removes only the selected exact route.", "↑/↓ or j/k select · Enter preview · Esc back")
	case modalConfirm:
		title = "CONFIRM EXPOSURE"
		if count := len(m.actionItems()); count > 1 {
			lines = append(lines, fmt.Sprintf("Selected: %d services", count))
		}
		lines = append(lines, "Target: "+m.actionSession.target.String(), "Requested: "+string(m.actionSession.mode))
		if item, ok := m.actionAnchorItem(); ok {
			if item.Warning != "" {
				lines = append(lines, "Warning: "+sanitizeTUIText(item.Warning))
			}
			if route := m.previewRoute(item); route != nil {
				lines = append(lines, "Current route: "+string(route.Mode)+" "+route.Target.String()+" selector="+valueOr(route.ProviderKey, "unavailable"))
			}
		}
		if m.actionSession.mode == model.ExposureFunnel {
			lines = append(lines, "WARNING: public internet exposure")
		}
		if m.externalPreview() {
			lines = append(lines, "WARNING: existing route is external/unknown")
		}
		cancelLabel, confirmLabel := "Cancel", "Confirm"
		if !m.actionSession.confirm {
			cancelLabel = "[Cancel]"
		} else {
			confirmLabel = "[Confirm]"
		}
		lines = append(lines, "", cancelLabel+"    "+confirmLabel, "Tab/←/→ choose · Enter select · Esc cancel")
	case modalTerminateProcess:
		title = "TERMINATE PROCESS"
		lines = append(lines,
			"This sends one SIGTERM to each selected process.",
			"Each process and listener is revalidated before signalling.",
			"No SIGKILL will be attempted.",
		)
		if len(m.processBatch) > 1 {
			lines = append(lines, "", fmt.Sprintf("Selected processes: %d", len(m.processBatch)))
			for _, target := range m.processBatch {
				lines = append(lines, fmt.Sprintf("  %s  PID %d  %s", valueOr(target.listener.Process, "unknown"), target.listener.PID, target.listener.Target.String()))
			}
		} else {
			lines = append(lines,
				"",
				"Process: "+valueOr(m.modalProcess.Process, "unknown"),
				fmt.Sprintf("PID: %d", m.modalProcess.PID),
				"Listener: "+m.modalProcess.Target.String(),
			)
			if m.modalProcess.CommandLine != "" {
				lines = append(lines, "Command: "+m.modalProcess.CommandLine)
			}
		}
		lines = append(lines,
			"",
			"WARNING: this terminates the application and all of its listeners.",
			"",
			"[Cancel]    Confirm",
			"Tab/←/→ choose · Enter select · Esc cancel",
		)
		if m.confirmFocus {
			lines[len(lines)-2] = "Cancel    [Confirm]"
		}
	case modalCancel:
		title = "CANCEL OPERATION"
		lines = append(lines, "Cancel Applying operation for:", "  "+m.modalTarget.String(), "", "The final provider state will be reread.", "", choiceLabels(m.modalChoice), "Tab/←/→ choose · Enter select · Esc back")
	case modalQuit:
		title = "OPERATION IN PROGRESS"
		lines = append(lines, "An operation is still in progress.", "Quitting can leave final provider state uncertain.", "", choiceLabels(m.modalChoice), "Tab/←/→ choose · Enter select · Esc stay")
	}
	lines = wrapTextLines(lines, maxInt(16, modalWidth-4))
	cleanLines := make([]string, 0, len(lines))
	for _, raw := range lines {
		line := sanitizeTUIText(raw)
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "WARNING:") || strings.HasPrefix(trimmed, "Warning:"):
			line = paint(m.cfg.ColorTheme, "1;33", line)
		case strings.Contains(line, " — Action unavailable"):
			line = paint(m.cfg.ColorTheme, "2;31", line)
		case strings.HasPrefix(trimmed, "> "):
			line = paint(m.cfg.ColorTheme, "1;36", line)
		}
		cleanLines = append(cleanLines, line)
	}
	modalTitle := paint(m.cfg.ColorTheme, "1;36", sanitizeTUIText(title))
	borderColor := "62"
	if m.modal == modalConfirm || m.modal == modalTerminateProcess || m.modal == modalQuit {
		borderColor = "33"
	}
	modalStyle := lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).Padding(0, 1).Width(modalWidth)
	if os.Getenv("NO_COLOR") == "" && (m.cfg.ColorTheme == "auto" || m.cfg.ColorTheme == "dark" || m.cfg.ColorTheme == "light") {
		if m.cfg.ColorTheme == "light" && borderColor == "33" {
			borderColor = "124"
		}
		modalStyle = modalStyle.BorderForeground(lipgloss.Color(borderColor))
	}
	return modalStyle.Render(modalTitle + "\n" + strings.Join(cleanLines, "\n"))
}
func choiceLabels(choice bool) string {
	if choice {
		return "Stay    [Cancel operation and quit]"
	}
	return "[Stay]    Cancel operation and quit"
}

func helpLines() []string {
	return []string{
		"tailge full-screen workspace",
		"",
		"WORKSPACE / NAVIGATION",
		"  j/k or Up/Down       move or scroll the focused pane",
		"  n/N                  next/previous item in normal workspace mode",
		"  h/l or Left/Right    focus list/details; Tab toggles; Shift-Tab reverses",
		"  Enter                focus/expand details; never mutates",
		"  Home/End             first/last item",
		"  gg/G                 first/last item; lone g waits briefly",
		"  Ctrl-d/Page Down     page down (list or details)",
		"  Ctrl-u/Page Up       page up (list or details)",
		"  Esc                  cancel pending g or return focus to the list",
		"  r                    refresh without stealing focus",
		"  R                    fresh retry preview after a failed operation",
		"  C or Ctrl-l          clear an accepted filter",
		"  x                    preview termination of the selected process(es)",
		"  v                    toggle only the current item",
		"  Shift-v (V)          enter/exit Vim-style visual-line selection; move to extend",
		"  U                    clear all selected items",
		"  X                    dismiss visible banner/transient feedback",
		"  q or Ctrl-C          quit; guarded while an operation is in progress",
		"",
		"SEARCH / FILTER",
		"  /                    start or edit incremental search",
		"  Up/Down              navigate filtered results while editing",
		"  Enter                accept the query",
		"  Esc                  cancel edit and restore the previous query",
		"  Ctrl-u               clear query text while editing",
		"  C or Ctrl-l          clear the accepted query in normal mode",
		"",
		"ACTION PREVIEW",
		"  Space                open the exposure action selector",
		"  s                    preview private Serve for the selection",
		"  f                    preview public Funnel for the selection",
		"  d                    preview Disable for the selection",
		"  v/V selection        action previews apply sequentially to selected items; each target is verified independently",
		"  j/k or Up/Down       choose an action",
		"  Enter                continue to route selection/confirmation or show a no-op",
		"  Esc                  cancel the preview",
		"  o                    open observed URL; Serve TCP uses HTTP preview",
		"  O                    open the selected listener at localhost",
		"  y                    copy the observed URL or Serve TCP HTTP preview",
		"  c                    open confirmed cancellation for Applying",
		"  x                    terminate selected process(es) sequentially (SIGTERM only)",
		"",
		"CONFIRMATION / APPLYING",
		"  Tab/Shift-Tab        move focus between Confirm/Cancel",
		"  Left/Right           switch Confirm/Cancel",
		"  Enter                activate the focused choice",
		"  Esc                  cancel confirmation or stay in the operation",
		"  Funnel               remains public; review the warning and explicitly focus Confirm",
		"  Disable               uses exact-route selection plus focused Confirm",
		"  Disable with multiple routes opens an exact-route chooser; only the selected route is removed",
		"  Serve                remains private, but exact readiness,",
		"                       target identity, confirmation, and verification remain",
		"                       required so a stale or ambiguous route is never changed",
		"",
		"COMMAND PALETTE",
		"  :                    open searchable commands",
		"  Up/Down or Ctrl-p/n  select a command",
		"  Enter                run the selected command",
		"  Esc                  close without running a command",
		"",
		"HELP",
		"  ?                    open or close this help",
		"  j/k, Up/Down, PgUp/PgDown, Ctrl-u/d  scroll help",
		"  Esc or ?             close help",
		"  q or Ctrl-C          quit from help",
		"",
		"SAFETY / STATE",
		"  Confirm is never the default; Cancel is focused first.",
		"  Unknown, stale, unavailable, or read-only state blocks mutation; exact-route choice resolves route multiplicity.",
		"  Operations are single-target, Applying, verified, cancelled, failed, or Unverified.",
	}
}

func overlay(background, box string, width, height int) string {
	if os.Getenv("NO_COLOR") == "" {
		background = lipgloss.NewStyle().Faint(true).Render(background)
	}
	bg := strings.Split(background, "\n")
	bl := strings.Split(box, "\n")
	startY := maxInt(0, (height-len(bl))/2)
	boxWidth := 0
	for _, line := range bl {
		if w := lipgloss.Width(line); w > boxWidth {
			boxWidth = w
		}
	}
	startX := maxInt(0, (width-boxWidth)/2)
	for i, line := range bl {
		y := startY + i
		if y >= len(bg) {
			break
		}
		if lineWidth := lipgloss.Width(line); lineWidth < boxWidth {
			line += strings.Repeat(" ", boxWidth-lineWidth)
		}
		before := ansi.Cut(bg[y], 0, startX)
		after := ansi.Cut(bg[y], startX+boxWidth, width)
		bg[y] = before + line + after
	}
	return strings.Join(bg, "\n")
}

func wrapTextLines(lines []string, width int) []string {
	if width <= 0 {
		return lines
	}
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" || lipgloss.Width(line) <= width {
			wrapped = append(wrapped, line)
			continue
		}
		indentEnd := 0
		for indentEnd < len(line) && (line[indentEnd] == ' ' || line[indentEnd] == '\t') {
			indentEnd++
		}
		indent := line[:indentEnd]
		words := strings.Fields(line[indentEnd:])
		if len(words) == 0 {
			wrapped = append(wrapped, indent)
			continue
		}
		current := indent
		for _, word := range words {
			candidate := current + word
			if current != indent {
				candidate = current + " " + word
			}
			if lipgloss.Width(candidate) <= width {
				current = candidate
				continue
			}
			if current != indent {
				wrapped = append(wrapped, current)
			}
			for lipgloss.Width(indent+word) > width && len([]rune(word)) > 1 {
				runes := []rune(word)
				cut := maxInt(1, width-lipgloss.Width(indent))
				if cut >= len(runes) {
					break
				}
				wrapped = append(wrapped, indent+string(runes[:cut]))
				word = string(runes[cut:])
			}
			current = indent + word
		}
		if current != indent {
			wrapped = append(wrapped, current)
		}
	}
	return wrapped
}

func clipLines(lines []string, offset, height int) string {
	if height <= 0 {
		return ""
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		offset = maxInt(0, len(lines)-1)
	}
	end := minInt(len(lines), offset+height)
	return strings.Join(lines[offset:end], "\n")
}

func (m *workspaceModel) clampOffsets() {
	m.detailOffset = maxInt(0, m.detailOffset)
	m.helpOffset = clamp(m.helpOffset, 0, m.helpMaxOffset())
}
func (m *workspaceModel) helpViewport() int { return maxInt(1, m.height-8) }
func (m *workspaceModel) modalPage() int    { return maxInt(1, m.helpViewport()) }
func (m *workspaceModel) helpMaxOffset() int {
	return maxInt(0, len(helpLines())-m.helpViewport())
}
