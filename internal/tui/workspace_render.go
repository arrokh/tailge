package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
)

func (m *workspaceModel) View() string {
	if m.width < 1 {
		m.width = 120
	}
	if m.height < 1 {
		m.height = 30
	}
	base := m.workspaceView()
	if m.modal != modalNone {
		if m.width < 30 || m.height < minModalRows {
			return "Terminal too small for this dialog — resize to at least 30 columns by 12 rows."
		}
		return overlay(base, m.modalView(), m.width, m.height)
	}
	return base
}

func (m *workspaceModel) workspaceView() string {
	if m.height < minCompactRows || m.width < 30 {
		return "Terminal too small — resize to at least 30 columns and 8 rows."
	}
	top := m.renderTop()
	contentHeight := maxInt(3, m.height-6)
	var body string
	if m.width >= splitMinWidth && m.height >= splitMinHeight {
		listWidth := maxInt(36, m.width*40/100)
		if listWidth > m.width-40 {
			listWidth = m.width - 40
		}
		detailWidth := m.width - listWidth - 1
		list := paneStyle(listWidth, contentHeight, m.focus == focusList, m.cfg.ColorTheme).Render(m.renderList(listWidth-2, contentHeight-2))
		detail := paneStyle(detailWidth, contentHeight, m.focus == focusDetails, m.cfg.ColorTheme).Render(m.renderDetails(detailWidth-2, contentHeight-2))
		body = lipgloss.JoinHorizontal(lipgloss.Top, list, detail)
	} else if m.focus == focusDetails {
		body = paneStyle(m.width-2, contentHeight, true, m.cfg.ColorTheme).Render(m.renderDetails(m.width-4, contentHeight-2))
	} else {
		body = paneStyle(m.width-2, contentHeight, true, m.cfg.ColorTheme).Render(m.renderList(m.width-4, contentHeight-2))
	}
	return top + "\n" + body + "\n" + m.renderBottom()
}

func paneStyle(width, height int, focused bool, theme string) lipgloss.Style {
	style := lipgloss.NewStyle().Width(width).Height(height).Border(lipgloss.NormalBorder(), true, true, true, true)
	if focused && os.Getenv("NO_COLOR") == "" && (theme == "auto" || theme == "dark" || theme == "light") {
		color := lipgloss.Color("62")
		if theme == "light" {
			color = lipgloss.Color("25")
		}
		style = style.BorderForeground(color)
	}
	return style
}

func viewAvailable(view exposure.View) bool {
	return !view.At.IsZero() || !view.Listeners.At.IsZero() || !view.Exposures.At.IsZero() || len(view.Items) > 0
}

func readinessAvailable(readiness model.Readiness) bool {
	return !readiness.At.IsZero() || readiness.Status != model.ReadinessUnknown || len(readiness.Modes) > 0
}

func readinessStatus(m *workspaceModel, mode model.ExposureMode) model.ReadinessStatus {
	if !m.hasReadiness || m.readyErr != nil {
		return model.ReadinessUnknown
	}
	return modeStatus(m.readiness, mode)
}

func (m *workspaceModel) renderTop() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "local machine"
	}
	listenerState, exposureState := "loading", "loading"
	if m.hasView || m.refreshState.viewComplete() {
		genericRefreshFailure := m.viewErr != nil && m.view.Listeners.Error == nil && m.view.Exposures.Error == nil && !m.view.Listeners.Stale && !m.view.Exposures.Stale
		if genericRefreshFailure {
			listenerState, exposureState = "stale", "stale/unavailable"
		} else {
			if m.view.Listeners.Authoritative && !m.view.Listeners.Stale && m.view.Listeners.Error == nil {
				listenerState = "ready"
			} else if m.view.Listeners.Stale || m.view.Listeners.Error != nil {
				listenerState = "stale"
			} else {
				listenerState = "unknown"
			}
			if m.view.Exposures.Authoritative && !m.view.Exposures.Stale && m.view.Exposures.Error == nil {
				exposureState = "ready"
			} else if m.view.Exposures.Stale || m.view.Exposures.Error != nil {
				exposureState = "stale/unavailable"
			} else {
				exposureState = "unknown"
			}
		}
	}
	lines := []string{fmt.Sprintf("TAILGE  %s   listeners:%s  exposure:%s  Serve:%s  Funnel:%s", sanitizeTUIText(host), listenerState, exposureState, readinessStatus(m, model.ExposureServe), readinessStatus(m, model.ExposureFunnel))}
	if m.banner != "" {
		lines = append(lines, "! "+sanitizeTUIText(m.banner))
	} else {
		// Keep the top bar a fixed two-line region so a warning arriving after
		// the first frame cannot shift the entire renderer and hide the title.
		lines = append(lines, " ")
	}
	for i := range lines {
		lines[i] = truncate(lines[i], m.width)
	}
	return strings.Join(lines, "\n")
}

func (m *workspaceModel) renderBottom() string {
	focus := "List"
	if m.focus == focusDetails {
		focus = "Details"
	}
	search := ""
	if m.searching {
		search = "  Search: /" + sanitizeTUIText(m.query) + "▌"
	} else if m.query != "" {
		search = "  Search: /" + sanitizeTUIText(m.query)
	}
	progress := sanitizeTUIText(m.transient)
	if m.refreshState.isPending() {
		if progress == "" {
			progress = "↻ Refreshing..."
		} else {
			progress += " · ↻ Refreshing..."
		}
	}
	if progress == "" && len(m.activeOps) > 0 {
		progress = "Applying"
	}
	if progress == "" && m.processBusy {
		progress = "Terminating process"
	}
	keys := m.urlShortcutStatus()
	selection := ""
	if count := m.selectionCount(); count > 0 {
		selection = fmt.Sprintf("  selected:%d", count)
	}
	if m.focus == focusDetails {
		keys += "  v toggle  V visual  U clear selection  s/f/d batch  Enter inspect  Esc cancel visual/list  C clear filter  c cancel  x terminate selected"
	} else if m.query != "" {
		keys = "j/k navigate  v toggle  V visual  U clear selection  C clear filter  / edit filter  Tab focus  " + keys + "  ? help  q quit"
	} else {
		keys = "j/k or ↑/↓ navigate  v toggle  V visual  U clear selection  Tab/h/l focus  " + keys + "  ? help  : palette  q quit"
	}
	return truncate(fmt.Sprintf("Focus: %s%s%s  %s", focus, search, selection, progress), m.width) + "\n" + truncate(keys, m.width)
}

func (m *workspaceModel) renderList(width, height int) string {
	items := m.items()
	title := paint(m.cfg.ColorTheme, "1;36", "SERVICE LIST")
	if !m.hasView && !m.refreshState.viewComplete() {
		return title + "\n\n  ◌ Loading listeners..."
	}
	if len(items) == 0 {
		if m.query != "" {
			return title + "\n\n  NO MATCHING SERVICES\n\n  Clear search with Ctrl-u or edit with /."
		}
		return title + "\n\n  No local listeners or configured routes."
	}
	lines := []string{title}
	section := ""
	for i, item := range items {
		currentSection := itemSection(item)
		if currentSection != section {
			section = currentSection
			lines = append(lines, "", paint(m.cfg.ColorTheme, "1;34", section), listenerTableHeader(width), listenerTableDivider(width))
		}
		active := i == m.selectedIdx && item.ID == m.selectedID
		marked := m.isMarked(item.ID)
		visual := m.visualRange[item.ID]
		status := ""
		if m.processTerminating(item) {
			status = "TERMINATING"
		}
		lines = append(lines, listenerTableRowWithStatus(item, width, active, marked, visual, m.cfg.ColorTheme, status))
	}
	return clipLines(lines, m.listOffset(lines, height), height)
}

func listenerTableWidths(width int) (service, target, mode, state int) {
	// The fixed six-cell selection/focus marker plus three separators leave
	// the remaining width for the service table columns.
	available := maxInt(20, width-9)
	if available < 33 {
		service = maxInt(7, available*30/100)
		target = maxInt(7, available*27/100)
		mode = maxInt(4, available*14/100)
		state = available - service - target - mode
		for state < 5 && mode > 4 {
			mode--
			state++
		}
		for state < 5 && target > 7 {
			target--
			state++
		}
		for state < 5 && service > 5 {
			service--
			state++
		}
		return service, target, mode, maxInt(5, state)
	}
	service = clamp(available*34/100, 10, 26)
	target = clamp(available*29/100, 10, 22)
	mode = clamp(available*16/100, 6, 12)
	state = available - service - target - mode
	for state < 9 && mode > 6 {
		mode--
		state++
	}
	for state < 9 && target > 10 {
		target--
		state++
	}
	for state < 9 && service > 8 {
		service--
		state++
	}
	return service, target, mode, maxInt(9, state)
}

func listenerTableHeader(width int) string {
	service, target, mode, state := listenerTableWidths(width)
	return paint("auto", "1;37", fmt.Sprintf("SELECT %-*s %-*s %-*s %-*s", service, truncate("SERVICE", service), target, truncate("TARGET", target), mode, truncate("MODE", mode), state, truncate("STATUS", state)))
}

func listenerTableDivider(width int) string {
	service, target, mode, state := listenerTableWidths(width)
	return paint("auto", "2;36", "      "+strings.Repeat("─", service+target+mode+state+3))
}

func listenerTableRow(item exposure.ReconciledItem, width int, active, marked, visual bool, theme string) string {
	return listenerTableRowWithStatus(item, width, active, marked, visual, theme, "")
}

func listenerTableRowWithStatus(item exposure.ReconciledItem, width int, active, marked, visual bool, theme, statusOverride string) string {
	serviceWidth, targetWidth, modeWidth, stateWidth := listenerTableWidths(width)
	name := item.ID
	if item.Listener != nil {
		name = valueOr(item.Listener.Name, item.Listener.Target.String())
	}
	target := "—"
	if targetValue, ok := itemTarget(item); ok {
		target = targetValue.String()
	}
	statusState := item.State
	status := stateBadgeText(item.State)
	if statusOverride != "" {
		statusState = model.ExposureApplying
		status = stateGlyph(statusState) + " " + statusOverride
	}
	status = truncate(status, stateWidth)
	columns := fmt.Sprintf("%-*s %-*s %-*s ", serviceWidth, truncate(sanitizeTUIText(name), serviceWidth), targetWidth, truncate(sanitizeTUIText(target), targetWidth), modeWidth, truncate(sanitizeTUIText(displayModeLabel(item)), modeWidth))
	statusCell := fmt.Sprintf("%-*s", stateWidth, status)
	rawPrefix := itemMarker(active, marked, visual)
	plainBody := columns + statusCell
	if width < 38 {
		row := truncate(rawPrefix+plainBody, width)
		if visual && marked {
			return paint(theme, "1;35", row)
		}
		if active {
			return rawPrefix + paint(theme, "1;36", strings.TrimPrefix(row, rawPrefix))
		}
		return row
	}
	if visual && marked {
		return paint(theme, "1;35", rawPrefix+plainBody)
	}
	if active {
		return rawPrefix + paint(theme, "1;36", plainBody)
	}
	return rawPrefix + columns + paint(theme, stateColor(statusState), statusCell)
}

func (m *workspaceModel) processTerminating(item exposure.ReconciledItem) bool {
	if !m.processBusy {
		return false
	}
	if len(m.processBatch) > 0 {
		return m.processBatchIndex < len(m.processBatch) && m.processBatch[m.processBatchIndex].itemID == item.ID
	}
	return item.ID == m.modalItemID
}

func itemMarker(active, marked, visual bool) string {
	selection := "[ ]"
	if marked {
		if visual {
			selection = "[V]"
		} else {
			selection = "[✓]"
		}
	}
	cursor := "  "
	if active {
		cursor = "> "
	}
	return cursor + selection + " "
}

func displayMode(item exposure.ReconciledItem) string {
	if len(item.Routes) > 1 {
		return "multiple (choose exact route)"
	}
	if len(item.Routes) == 1 {
		return string(item.Routes[0].Mode)
	}
	return string(item.Mode)
}

func displayModeLabel(item exposure.ReconciledItem) string {
	if len(item.Routes) > 1 {
		return "MULTI"
	}
	if len(item.Routes) == 1 {
		return strings.ToUpper(string(item.Routes[0].Mode))
	}
	if item.Mode == model.ExposureDisabled {
		return "OFF"
	}
	return strings.ToUpper(string(item.Mode))
}

func stateGlyph(state model.ExposureState) string {
	switch state {
	case model.ExposureActive, model.ExposureSucceeded:
		return "●"
	case model.ExposureApplying:
		return "◌"
	case model.ExposureUnknown, model.ExposureUnavailable, model.ExposureUnverified:
		return "?"
	case model.ExposureFailed, model.ExposureAmbiguous:
		return "!"
	default:
		return "–"
	}
}

func stateColor(state model.ExposureState) string {
	switch state {
	case model.ExposureActive, model.ExposureSucceeded:
		return "32"
	case model.ExposureApplying:
		return "33"
	case model.ExposureUnknown, model.ExposureUnavailable, model.ExposureUnverified, model.ExposureFailed, model.ExposureAmbiguous:
		return "31"
	default:
		return "2;37"
	}
}

func stateBadgeText(state model.ExposureState) string {
	labels := map[model.ExposureState]string{
		model.ExposureActive:            "ACTIVE",
		model.ExposureInactive:          "INACTIVE",
		model.ExposureUnsupported:       "UNSUP",
		model.ExposureAmbiguous:         "AMBIG",
		model.ExposureUnknown:           "UNKNOWN",
		model.ExposureUnavailable:       "UNAVAIL",
		model.ExposureApplying:          "APPLYING",
		model.ExposureSucceeded:         "DONE",
		model.ExposureFailed:            "FAILED",
		model.ExposureCancelled:         "CANCEL",
		model.ExposureUnverified:        "VERIFY",
		model.ExposureState("disabled"): "OFF",
	}
	label, ok := labels[state]
	if !ok {
		label = strings.ToUpper(string(state))
	}
	return stateGlyph(state) + " " + label
}

func readinessBadgeText(status model.ReadinessStatus) string {
	label := strings.ToUpper(strings.ReplaceAll(string(status), "_", "-"))
	glyph := "?"
	if status == model.ReadinessReady {
		glyph = "●"
	} else if status == model.ReadinessReadOnly || status == model.ReadinessNotReady {
		glyph = "!"
	}
	return glyph + " " + label
}

func ownershipBadgeText(ownership model.Ownership) string {
	switch ownership {
	case model.OwnershipManaged:
		return "MANAGED"
	case model.OwnershipExternal:
		return "EXTERNAL"
	case model.OwnershipUnknown:
		return "UNKNOWN"
	default:
		return strings.ToUpper(string(ownership))
	}
}

func detailGroup(title string) string {
	return "── " + title + " ──"
}
func (m *workspaceModel) listOffset(lines []string, height int) int {
	if height <= 0 || len(lines) <= height {
		m.listScroll = 0
		return 0
	}
	selectedLine := 0
	for lineIndex, line := range lines {
		if strings.HasPrefix(line, "> ") {
			selectedLine = lineIndex
			break
		}
	}
	if selectedLine < m.listScroll {
		m.listScroll = selectedLine
	}
	if selectedLine >= m.listScroll+height {
		m.listScroll = selectedLine - height + 1
	}
	m.listScroll = clamp(m.listScroll, 0, maxInt(0, len(lines)-height))
	return m.listScroll
}

func (m *workspaceModel) renderDetails(width, height int) string {
	item, ok := m.selectedItem()
	if !m.hasView && !m.refreshState.viewComplete() {
		return "DETAILS\n\n◌ Loading selected service..."
	}
	if !ok {
		return "DETAILS\n\nNo selected service.\nSearch returned no matches or data is unavailable."
	}
	title := "DETAILS"
	lines := []string{
		title,
		"  " + valueOr(item.ID, "selected service"),
		fmt.Sprintf("  state: %s %s   mode: %s %s", item.State, "["+stateBadgeText(item.State)+"]", displayMode(item), "["+displayModeLabel(item)+"]"),
	}
	if item.Warning != "" {
		lines = append(lines, "", detailGroup("ALERTS"))
		for _, warning := range detailWarnings(item.Warning) {
			lines = append(lines, "  "+detailOwner(warning)+" issue: "+warning)
		}
	}
	if actionItems := detailActionItems(item); len(actionItems) > 0 {
		lines = append(lines, "", detailGroup("ACTION ITEMS"))
		lines = append(lines, actionItems...)
	}

	lines = append(lines, "", detailGroup("LISTENER"))
	if item.Listener == nil {
		lines = append(lines, "  no current local listener")
	} else {
		l := item.Listener
		lines = append(lines,
			"  target: "+l.Target.String(),
			"  protocol: "+l.Target.Protocol+"  scope: "+string(l.Scope),
			fmt.Sprintf("  process: %s  pid: %s", valueOr(l.Process, "unavailable"), pidText(l.PID)),
			"  metadata: "+string(l.Metadata),
			"  discovered: "+timeText(l.LastSeen),
		)
		if l.CommandLine != "" {
			lines = append(lines, "  command: "+l.CommandLine)
		}
	}

	lines = append(lines, "", detailGroup(fmt.Sprintf("EXPOSURE ROUTES (%d)", len(item.Routes))))
	if len(item.Routes) == 0 {
		lines = append(lines, "  route: none")
	}
	for index, route := range item.Routes {
		lines = append(lines, fmt.Sprintf("  %d. %s  [%s]  [%s]", index+1, strings.ToUpper(string(route.Mode)), stateBadgeText(route.State), ownershipBadgeText(route.Ownership)))
		lines = append(lines, "     target: "+route.Target.String())
		if route.ProviderKey != "" {
			lines = append(lines, "     selector: "+route.ProviderKey)
		}
		if route.URL != "" {
			lines = append(lines, "     url: "+route.URL)
		}
		lines = append(lines, "     verified: "+timeText(route.LastVerifiedAt))
	}

	lines = append(lines, "", detailGroup("READINESS"))
	if !m.hasReadiness {
		lines = append(lines, "  status: not observed")
	} else if m.readyErr != nil {
		lines = append(lines, "  status: stale", "  error: "+safeMessage(m.readyErr))
	} else if len(m.readiness.Modes) == 0 {
		lines = append(lines, "  status: unknown")
	} else {
		for _, mode := range m.readiness.Modes {
			owner := strings.ToUpper(string(mode.Mode))
			lines = append(lines, fmt.Sprintf("  %s: %s  [%s]  [owner: %s]", mode.Mode, mode.Status, readinessBadgeText(mode.Status), owner))
			hasIssue := false
			for _, check := range mode.Checks {
				if check.Status == model.ReadinessReady {
					continue
				}
				hasIssue = true
				lines = append(lines, "  ["+owner+"] reason: "+check.Message)
				if check.Remediation != "" {
					lines = append(lines, "  ["+owner+"] next: "+check.Remediation)
				}
			}
			if !hasIssue {
				lines = append(lines, "  ["+owner+"] action: none; readiness evidence is available")
			}
		}
	}

	lines = append(lines, "", detailGroup("OPERATION"))
	operationMode := detailOperationMode(m.view, item)
	for _, mode := range []model.ExposureMode{model.ExposureServe, model.ExposureFunnel} {
		state := "idle"
		if item.OperationState != "" && operationMode == mode {
			state = string(item.OperationState) + " [" + stateBadgeText(item.OperationState) + "]"
		}
		lines = append(lines, "  "+strings.ToUpper(string(mode))+" operation: "+state)
	}
	if operationMode == model.ExposureDisabled && item.OperationState != "" {
		lines = append(lines, "  DISABLE operation: "+string(item.OperationState)+" ["+stateBadgeText(item.OperationState)+"]")
	}
	if item.DesiredMode != "" {
		lines = append(lines, "  requested: "+strings.ToUpper(string(item.DesiredMode)))
	}
	if item.LastOperation != nil {
		owner := strings.ToUpper(string(operationMode))
		lines = append(lines, fmt.Sprintf("  %s result: verified=%t exit=%d", owner, item.LastOperation.Verified, item.LastOperation.ExitCode))
		if item.LastOperation.Error != nil {
			lines = append(lines, "  "+owner+" issue: "+item.LastOperation.Error.Message, "  "+owner+" next: "+item.LastOperation.Error.Remediation)
		}
	}
	if item.OperationState == model.ExposureFailed || item.OperationState == model.ExposureUnverified || item.OperationState == model.ExposureCancelled {
		lines = append(lines, "  "+strings.ToUpper(string(operationMode))+" next: press R for a fresh refresh and preview")
	}

	lines = append(lines, "", detailGroup("SAFETY"), "  network scope and public exposure warnings are shown above.", "  s/f/d and Space open an exposure preview; no action occurs from Enter.", "  x opens guarded process termination; only confirmed SIGTERM is sent.")
	lines = wrapTextLines(lines, width)
	for i := range lines {
		line := sanitizeTUIText(lines[i])
		trimmed := strings.TrimSpace(line)
		switch {
		case i == 0:
			line = paint(m.cfg.ColorTheme, "1;36", line)
		case strings.HasPrefix(trimmed, "── "):
			line = paint(m.cfg.ColorTheme, "1;34", line)
		case strings.HasPrefix(trimmed, "warning:") || strings.HasPrefix(trimmed, "WARNING:") || strings.Contains(trimmed, "reason:") || strings.Contains(trimmed, "issue:") || strings.Contains(trimmed, "next:") || strings.HasPrefix(trimmed, "error:"):
			line = paint(m.cfg.ColorTheme, "1;33", line)
		case strings.Contains(trimmed, "READ-ONLY") || strings.Contains(trimmed, "[UNKNOWN]"):
			line = paint(m.cfg.ColorTheme, "1;33", line)
		case strings.Contains(trimmed, "[EXTERNAL]") || strings.Contains(trimmed, "[! ") || strings.Contains(trimmed, "[? "):
			line = paint(m.cfg.ColorTheme, "1;31", line)
		case strings.Contains(trimmed, "[● ") || strings.Contains(trimmed, "[✓ "):
			line = paint(m.cfg.ColorTheme, "1;32", line)
		case strings.Contains(trimmed, "[◌ "):
			line = paint(m.cfg.ColorTheme, "1;33", line)
		}
		lines[i] = line
	}
	return clipLines(lines, m.detailOffset, height)
}

func pidText(pid int) string {
	if pid <= 0 {
		return "unavailable"
	}
	return strconv.Itoa(pid)
}

func timeText(value time.Time) string {
	if value.IsZero() {
		return "not verified"
	}
	return value.Format(time.RFC3339)
}

func detailWarnings(value string) []string {
	parts := strings.Split(value, "; ")
	warnings := make([]string, 0, len(parts))
	for _, part := range parts {
		if text := strings.TrimSpace(part); text != "" {
			warnings = append(warnings, text)
		}
	}
	return warnings
}

func detailOwner(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "funnel"):
		return "FUNNEL"
	case strings.Contains(lower, "serve"):
		return "SERVE"
	case strings.Contains(lower, "listener"), strings.Contains(lower, "process"), strings.Contains(lower, "network"), strings.Contains(lower, "port"):
		return "LISTENER"
	default:
		return "EXPOSURE"
	}
}

func detailWarningNext(owner, message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "multiple"), strings.Contains(lower, "exact"):
		return "Refresh and choose one exact route before changing exposure."
	case strings.Contains(lower, "stale"), strings.Contains(lower, "incomplete"), strings.Contains(lower, "unknown"):
		return "Refresh and wait for authoritative state before changing exposure."
	case owner == "FUNNEL":
		return "Review public-internet reachability and confirm the exact Funnel action."
	case owner == "LISTENER":
		return "Inspect the listener scope and process before exposing or terminating it."
	default:
		return "Review the observed route and refresh before changing it."
	}
}

func detailActionItems(item exposure.ReconciledItem) []string {
	items := []string{}
	for _, warning := range detailWarnings(item.Warning) {
		owner := detailOwner(warning)
		items = append(items, "  "+owner+" next: "+detailWarningNext(owner, warning))
	}
	if item.Recommendation != "" {
		owner := "EXPOSURE"
		if len(item.Routes) == 1 {
			owner = strings.ToUpper(string(item.Routes[0].Mode))
		}
		items = append(items, "  "+owner+" next: "+item.Recommendation)
	}
	for _, route := range item.Routes {
		owner := strings.ToUpper(string(route.Mode))
		if route.ProviderKey == "" {
			items = append(items, "  "+owner+" next: wait for an exact provider selector; mutation is blocked.")
		}
	}
	return items
}

func detailOperationMode(view exposure.View, item exposure.ReconciledItem) model.ExposureMode {
	target, ok := itemTarget(item)
	if ok {
		for index := len(view.Events) - 1; index >= 0; index-- {
			event := view.Events[index]
			if event.Target.Key() == target.Key() && event.Mode.Valid() {
				return event.Mode
			}
		}
	}
	if item.DesiredMode.Valid() {
		return item.DesiredMode
	}
	if len(item.Routes) == 1 && item.Routes[0].Mode.Valid() {
		return item.Routes[0].Mode
	}
	return model.ExposureDisabled
}
