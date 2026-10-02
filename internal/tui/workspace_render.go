package tui

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/buildinfo"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/workspace"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *workspaceModel) View() string {
	// Rendering is a projection, not a state transition. Work on a shallow copy
	// so layout defaults and computed list scrolling cannot mutate the decision
	// state while Bubble Tea is asking for a frame.
	render := *m
	if render.width < 1 {
		render.width = 120
	}
	if render.height < 1 {
		render.height = 30
	}
	base := render.workspaceView()
	if render.modal != modalNone {
		if render.width < 30 || render.height < minModalRows {
			return "Terminal too small for this dialog — resize to at least 30 columns by 12 rows."
		}
		return overlay(base, render.modalView(), render.width, render.height)
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
	if splitViewAvailable(m.width, m.height) && !m.zoomed {
		listWidth := maxInt(minListPaneWidth, m.width*40/100)
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

func readinessAvailable(readiness readinessmodel.Readiness) bool {
	return !readiness.At.IsZero() || readiness.Status != readinessmodel.ReadinessUnknown || len(readiness.Modes) > 0
}

func readinessStatus(m *workspaceModel, mode exposuredata.ExposureMode) readinessmodel.ReadinessStatus {
	if !m.hasReadiness || m.readyErr != nil {
		return readinessmodel.ReadinessUnknown
	}
	return workspace.ModeStatus(m.readiness, mode)
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
	lines := []string{fmt.Sprintf("TAILGE  %s   listeners:%s  exposure:%s  Serve:%s  Funnel:%s", sanitizeTUIText(host), listenerState, exposureState, readinessStatus(m, exposuredata.ExposureServe), readinessStatus(m, exposuredata.ExposureFunnel))}
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

const (
	repositoryURL            = buildinfo.RepositoryURL
	repositoryLabel          = "github.com/arrokh/tailge"
	compactRepositoryLabel   = "GitHub"
	footerMinimumStatusWidth = 18
)

func footerBuildIdentity(width int, status, theme string) (plain, linked string) {
	commit := sanitizeTUIText(buildinfo.ShortCommit())
	label := compactRepositoryLabel
	full := commit + " " + repositoryLabel
	fullStatusWidth := width - lipgloss.Width(full) - 2
	if fullStatusWidth >= footerMinimumStatusWidth && fullStatusWidth >= lipgloss.Width(status) {
		label = repositoryLabel
	} else if width-lipgloss.Width(commit+" "+label)-2 < footerMinimumStatusWidth {
		if runes := []rune(commit); len(runes) > 7 {
			commit = string(runes[:7])
		}
	}
	plain = commit + " " + label
	linked = paint(theme, "1;36", commit) + " " + ansi.SetHyperlink(repositoryURL) + paint(theme, "4;34", label) + ansi.ResetHyperlink()
	return plain, linked
}

func compactURLShortcutStatus(status string) string {
	return strings.NewReplacer(" HTTPS[", ":", " HTTP-preview[", ":", " copy[", ":", " local[", ":", "]", "").Replace(status)
}

func compactSortLabel(sortKey string) string {
	switch sortKey {
	case "name":
		return "Name↑"
	case "name-desc":
		return "Name↓"
	case "none":
		return "Unsorted"
	case "address":
		return "Address↑"
	case "exposure":
		return "Exposure↑"
	default:
		return "Port↑"
	}
}

func footerSortLabel(sortKey string) string {
	switch sortKey {
	case "name":
		return "Name ↑"
	case "name-desc":
		return "Name ↓"
	case "none":
		return "Unsorted"
	default:
		return sortDescription(sortKey)
	}
}

func compactFooterStatus(width int, focus, sortKey, httpsRootStatus, progress string) string {
	if progress != "" {
		return progress
	}
	parts := []string{focus}
	if width >= 40 {
		parts = append(parts, compactSortLabel(sortKey))
	}
	parts = append(parts, strings.Replace(httpsRootStatus, "b HTTPS root", "b", 1))
	return strings.Join(parts, " ")
}

func compactFooterStatusWithShortcuts(width int, focus, sortKey, httpsRootStatus, urlStatus, progress string) string {
	return compactFooterStatus(width, focus, sortKey, httpsRootStatus, progress) + " " + compactURLShortcutStatus(urlStatus)
}

func footerKeyHints(width int, focus paneFocus, searching, zoomed, canZoom bool, urlStatus string) string {
	if searching {
		switch {
		case width >= 68:
			return "↑↓ Results · Enter Accept · Esc Cancel · Ctrl+u Clear"
		case width >= 40:
			return "Enter/Esc · " + compactURLShortcutStatus(urlStatus)
		default:
			hints := "Enter/Esc · " + compactURLShortcutStatus(urlStatus)
			if lipgloss.Width(hints) <= width {
				return hints
			}
			return compactURLShortcutStatus(urlStatus)
		}
	}
	if width < 40 {
		return compactURLShortcutStatus(urlStatus) + " · ? Help"
	}
	if width < 60 {
		return "↑↓ Move · " + compactURLShortcutStatus(urlStatus)
	}
	zoomHint := ""
	if canZoom {
		zoomHint = " · z Zoom"
		if zoomed {
			zoomHint = " · z Restore"
		}
	}
	if focus == focusDetails {
		switch {
		case width >= 120:
			return "↑↓ Scroll · Tab List · v/V Select · s/f/d Routes · b HTTPS · x Term · e/S Sort · / Find" + zoomHint + " · ? Help · q Quit"
		case width >= 100:
			return "↑↓ Scroll · Tab List · e/S Sort · / Find · ? Help · q Quit" + zoomHint
		case width >= 72:
			return "↑↓ Scroll · Tab List · e/S Sort · ? Help · q Quit" + zoomHint
		case width >= 48:
			return "↑↓ Scroll · Tab List · ? Help"
		default:
			return "↑↓ Scroll · ? Help"
		}
	}
	switch {
	case width >= 120:
		return "↑↓ Move · Tab Focus · v/V Select · s/f/d Routes · b HTTPS · x Term · e/S Sort · / Find" + zoomHint + " · ? Help · q Quit"
	case width >= 100:
		return "↑↓ Move · Tab Focus · v Mark · s/f/d Routes · e/S Sort · / Find · ? Help · q Quit" + zoomHint
	case width >= 72:
		return "↑↓ Move · Tab Focus · e/S Sort · / Find · ? Help · q Quit"
	case width >= 48:
		return "↑↓ Move · Tab Focus · e/S Sort · ? Help"
	default:
		return "↑↓ Move · ? Help"
	}
}

func (m *workspaceModel) renderBottom() string {
	focus := "List"
	if m.focus == focusDetails {
		focus = "Details"
	}
	if m.zoomed {
		focus += " (zoomed)"
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
	selection := ""
	if count := m.selectionCount(); count > 0 {
		selection = fmt.Sprintf("  selected:%d", count)
	}
	urlStatus := m.urlShortcutStatus()
	status := fmt.Sprintf("Focus: %s  Sort: %s  %s  %s%s%s", focus, footerSortLabel(m.cfg.Sort), m.httpsRootStatusLabel(), compactURLShortcutStatus(urlStatus), search, selection)
	if progress != "" {
		status += "  " + progress
	}
	identityText, identityLink := footerBuildIdentity(m.width, status, m.cfg.ColorTheme)
	statusWidth := maxInt(0, m.width-lipgloss.Width(identityText)-2)
	if lipgloss.Width(status) > statusWidth {
		if m.width < 60 {
			status = compactFooterStatus(m.width, focus, m.cfg.Sort, m.httpsRootStatusLabel(), progress)
		} else {
			status = compactFooterStatusWithShortcuts(m.width, focus, m.cfg.Sort, m.httpsRootStatusLabel(), urlStatus, progress)
		}
	}
	status = ansi.Truncate(status, statusWidth, "...")
	padding := strings.Repeat(" ", maxInt(0, statusWidth-lipgloss.Width(status)))
	keyHints := footerKeyHints(m.width, m.focus, m.searching, m.zoomed, splitViewAvailable(m.width, m.height), urlStatus)
	return status + padding + "  " + identityLink + "\n" + ansi.Truncate(keyHints, m.width, "…")
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

type listenerTableLayout struct {
	service int
	target  int
	mode    int
	state   int
	cpu     int
	memory  int
}

type listenerTableColumn struct {
	name  string
	width int
}

func (layout listenerTableLayout) columns() []listenerTableColumn {
	columns := []listenerTableColumn{
		{name: "SERVICE", width: layout.service},
		{name: "TARGET", width: layout.target},
		{name: "MODE", width: layout.mode},
		{name: "STATUS", width: layout.state},
		{name: "CPU%", width: layout.cpu},
		{name: "MEM", width: layout.memory},
	}
	visible := columns[:0]
	for _, column := range columns {
		if column.width > 0 {
			visible = append(visible, column)
		}
	}
	return visible
}

func listenerTableWidths(width int) listenerTableLayout {
	// Keep CPU and RSS visible even in compact list views. At very narrow
	// widths, target and status columns are progressively omitted rather than
	// truncating the resource columns off the right edge.
	switch {
	case width >= 51:
		layout := listenerTableLayout{service: 7, target: 7, mode: 5, state: 11, cpu: 5, memory: 5}
		extra := width - 51
		serviceExtra := minInt(extra, 19)
		layout.service += serviceExtra
		extra -= serviceExtra
		targetExtra := minInt(extra, 16)
		layout.target += targetExtra
		extra -= targetExtra
		layout.state += extra
		return layout
	case width >= 42:
		return listenerTableLayout{service: 7, mode: 4, state: 11, cpu: 5, memory: 5}
	case width >= 36:
		return listenerTableLayout{service: 6, mode: 4, state: 6, cpu: 5, memory: 5}
	case width >= 29:
		return listenerTableLayout{service: 6, mode: 4, cpu: 5, memory: 5}
	default:
		return listenerTableLayout{service: maxInt(1, width-18), cpu: 5, memory: 5}
	}
}

func listenerTableHeader(width int) string {
	columns := listenerTableWidths(width).columns()
	cells := make([]string, 0, len(columns))
	for _, column := range columns {
		label := column.name
		if column.name == "SERVICE" && column.width < 7 {
			label = "SVC"
		} else if column.name == "TARGET" && column.width < 7 {
			label = "TGT"
		}
		cells = append(cells, fmt.Sprintf("%-*s", column.width, truncate(label, column.width)))
	}
	return paint("auto", "1;37", "SELECT "+strings.Join(cells, " "))
}

func listenerTableDivider(width int) string {
	columns := listenerTableWidths(width).columns()
	columnWidth := maxInt(0, len(columns)-1)
	for _, column := range columns {
		columnWidth += column.width
	}
	return paint("auto", "2;36", "      "+strings.Repeat("─", columnWidth))
}

func listenerTableRow(item exposure.ReconciledItem, width int, active, marked, visual bool, theme string) string {
	return listenerTableRowWithStatus(item, width, active, marked, visual, theme, "")
}

func listenerTableRowWithStatus(item exposure.ReconciledItem, width int, active, marked, visual bool, theme, statusOverride string) string {
	layout := listenerTableWidths(width)
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
		statusState = exposuredata.ExposureApplying
		status = statusOverride
	}
	cpu, memory := "—", "—"
	if item.Listener != nil && item.Listener.Usage != nil {
		cpu = processCPUCell(item.Listener.Usage.CPUPercent)
		memory = processMemoryCell(item.Listener.Usage.MemoryBytes)
	}
	values := map[string]string{
		"SERVICE": sanitizeTUIText(name),
		"TARGET":  sanitizeTUIText(target),
		"MODE":    sanitizeTUIText(displayModeLabel(item)),
		"STATUS":  sanitizeTUIText(status),
		"CPU%":    cpu,
		"MEM":     memory,
	}
	columns := layout.columns()
	plainCells := make([]string, 0, len(columns))
	renderedCells := make([]string, 0, len(columns))
	for _, column := range columns {
		cell := fmt.Sprintf("%-*s", column.width, truncate(values[column.name], column.width))
		plainCells = append(plainCells, cell)
		if column.name == "STATUS" && !active && !(visual && marked) {
			cell = paint(theme, stateColor(statusState), cell)
		}
		renderedCells = append(renderedCells, cell)
	}
	rawPrefix := itemMarker(active, marked, visual)
	plainBody := strings.Join(plainCells, " ")
	if visual && marked {
		return paint(theme, "1;35", rawPrefix+plainBody)
	}
	if active {
		return rawPrefix + paint(theme, "1;36", plainBody)
	}
	return rawPrefix + strings.Join(renderedCells, " ")
}

func processCPUCell(percent float64) string {
	if percent < 0 || math.IsNaN(percent) || math.IsInf(percent, 0) {
		return "—"
	}
	if percent >= 1000 {
		return ">999%"
	}
	if percent >= 99.95 {
		return fmt.Sprintf("%.0f%%", percent)
	}
	return fmt.Sprintf("%.1f%%", percent)
}

func processMemoryCell(bytes uint64) string {
	const (
		kib = uint64(1024)
		mib = kib * 1024
		gib = mib * 1024
		tib = gib * 1024
	)
	switch {
	case bytes < kib:
		return "<1K"
	case bytes < mib:
		return fmt.Sprintf("%dK", bytes/kib)
	case bytes < gib:
		return fmt.Sprintf("%dM", bytes/mib)
	case bytes < tib:
		value := float64(bytes) / float64(gib)
		if value < 99.95 {
			return fmt.Sprintf("%.1fG", value)
		}
		return fmt.Sprintf("%.0fG", value)
	default:
		value := float64(bytes) / float64(tib)
		if value >= 1000 {
			return ">999T"
		}
		if value < 10 {
			return fmt.Sprintf("%.1fT", value)
		}
		return fmt.Sprintf("%.0fT", value)
	}
}

func processMemorySourceLabel(source discovery.ProcessMemorySource) string {
	if source == discovery.ProcessMemoryPhysicalFootprint {
		return "physical footprint"
	}
	return "RSS"
}

func processMemoryDetail(bytes uint64) string {
	const (
		mib = uint64(1024 * 1024)
		gib = mib * 1024
		tib = gib * 1024
	)
	switch {
	case bytes < mib:
		return fmt.Sprintf("%d KiB", bytes/1024)
	case bytes < gib:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	case bytes < tib:
		return fmt.Sprintf("%.2f GiB", float64(bytes)/float64(gib))
	default:
		return fmt.Sprintf("%.2f TiB", float64(bytes)/float64(tib))
	}
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
	if item.Mode == exposuredata.ExposureDisabled {
		return "OFF"
	}
	return strings.ToUpper(string(item.Mode))
}

func stateGlyph(state exposuredata.ExposureState) string {
	switch state {
	case exposuredata.ExposureActive, exposuredata.ExposureSucceeded:
		return "●"
	case exposuredata.ExposureApplying:
		return "◌"
	case exposuredata.ExposureUnknown, exposuredata.ExposureUnavailable, exposuredata.ExposureUnverified:
		return "?"
	case exposuredata.ExposureFailed, exposuredata.ExposureAmbiguous:
		return "!"
	default:
		return "–"
	}
}

func stateColor(state exposuredata.ExposureState) string {
	switch state {
	case exposuredata.ExposureActive, exposuredata.ExposureSucceeded:
		return "32"
	case exposuredata.ExposureApplying:
		return "33"
	case exposuredata.ExposureUnknown, exposuredata.ExposureUnavailable, exposuredata.ExposureUnverified, exposuredata.ExposureFailed, exposuredata.ExposureAmbiguous:
		return "31"
	default:
		return "2;37"
	}
}

func stateBadgeText(state exposuredata.ExposureState) string {
	labels := map[exposuredata.ExposureState]string{
		exposuredata.ExposureActive:            "ACTIVE",
		exposuredata.ExposureInactive:          "INACTIVE",
		exposuredata.ExposureUnsupported:       "UNSUP",
		exposuredata.ExposureAmbiguous:         "AMBIG",
		exposuredata.ExposureUnknown:           "UNKNOWN",
		exposuredata.ExposureUnavailable:       "UNAVAIL",
		exposuredata.ExposureApplying:          "APPLYING",
		exposuredata.ExposureSucceeded:         "DONE",
		exposuredata.ExposureFailed:            "FAILED",
		exposuredata.ExposureCancelled:         "CANCEL",
		exposuredata.ExposureUnverified:        "VERIFY",
		exposuredata.ExposureState("disabled"): "OFF",
	}
	label, ok := labels[state]
	if !ok {
		label = strings.ToUpper(string(state))
	}
	return stateGlyph(state) + " " + label
}

func readinessBadgeText(status readinessmodel.ReadinessStatus) string {
	label := strings.ToUpper(strings.ReplaceAll(string(status), "_", "-"))
	glyph := "?"
	if status == readinessmodel.ReadinessReady {
		glyph = "●"
	} else if status == readinessmodel.ReadinessReadOnly || status == readinessmodel.ReadinessNotReady {
		glyph = "!"
	}
	return glyph + " " + label
}

func ownershipBadgeText(ownership exposuredata.Ownership) string {
	switch ownership {
	case exposuredata.OwnershipManaged:
		return "MANAGED"
	case exposuredata.OwnershipExternal:
		return "EXTERNAL"
	case exposuredata.OwnershipUnknown:
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
		if l.Usage != nil {
			lines = append(lines,
				fmt.Sprintf("  CPU usage: %s (ps %%CPU)", processCPUCell(l.Usage.CPUPercent)),
				fmt.Sprintf("  memory (%s): %s", processMemorySourceLabel(l.Usage.MemorySource), processMemoryDetail(l.Usage.MemoryBytes)),
			)
		}
		if l.WorkingDirectory != "" {
			lines = append(lines, "  working directory: "+l.WorkingDirectory)
		}
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
		if route.Kind == exposuredata.RouteKindHTTPPath {
			lines = append(lines, "     kind: named HTTP path (configured outside the TUI)", "     mount path: "+valueOr(route.Path, "/"))
		} else if route.Kind == exposuredata.RouteKindHTTPSRoot {
			lines = append(lines, "     kind: private HTTPS root handler", "     mount path: /")
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
			handlerStatus := mode.HTTPPathStatus
			if handlerStatus != "" || mode.HTTPPathMessage != "" {
				if handlerStatus == "" {
					handlerStatus = readinessmodel.ReadinessUnknown
				}
				lines = append(lines, fmt.Sprintf("  %s HTTPS handlers: %s", mode.Mode, handlerStatus))
				if mode.HTTPPathMessage != "" && handlerStatus != readinessmodel.ReadinessReady {
					lines = append(lines, "  ["+owner+"] HTTPS handler reason: "+mode.HTTPPathMessage)
					if mode.HTTPPathRemediation != "" {
						lines = append(lines, "  ["+owner+"] HTTPS handler next: "+mode.HTTPPathRemediation)
					}
				}
			}
			hasIssue := false
			for _, check := range mode.Checks {
				if check.Status == readinessmodel.ReadinessReady {
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
	for _, mode := range []exposuredata.ExposureMode{exposuredata.ExposureServe, exposuredata.ExposureFunnel} {
		state := "idle"
		if item.OperationState != "" && operationMode == mode {
			state = string(item.OperationState) + " [" + stateBadgeText(item.OperationState) + "]"
		}
		lines = append(lines, "  "+strings.ToUpper(string(mode))+" operation: "+state)
	}
	if operationMode == exposuredata.ExposureDisabled && item.OperationState != "" {
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
	if item.OperationState == exposuredata.ExposureFailed || item.OperationState == exposuredata.ExposureUnverified || item.OperationState == exposuredata.ExposureCancelled {
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

func detailOperationMode(view exposure.View, item exposure.ReconciledItem) exposuredata.ExposureMode {
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
	return exposuredata.ExposureDisabled
}
