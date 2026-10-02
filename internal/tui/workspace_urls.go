package tui

// Observed HTTPS URL actions, browser launching, and clipboard transports.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
)

const unavailableURLBanner = "No observed HTTPS URL is available"
const unavailableOpenURLBanner = "No observed browser URL or Serve TCP preview is available. Use b to preview a private HTTPS root (only if this listener speaks HTTP) or refresh Tailscale status."
const unavailableCopyURLBanner = "No observed browser URL or Serve TCP preview is available to copy. Use b to preview a private HTTPS root (only if this listener speaks HTTP) or refresh Tailscale status."
const insecureURLBanner = "Observed route URL is not HTTPS. Configure an explicit HTTPS route before opening or copying it."
const magicDNSPreviewUnavailableBanner = "Tailscale did not report a valid MagicDNS name for this Serve TCP preview. Check Tailscale status and try again."
const magicDNSStatusTimeout = 5 * time.Second
const localURLUnavailableBanner = "No active local listener is available for the O browser shortcut."

func (m *workspaceModel) clearStaleURLUnavailableBanner() {
	if m.banner == "" {
		return
	}
	item, ok := m.selectedItem()
	if !ok {
		return
	}
	available := len(observedHTTPSURLRoutes(item)) > 0
	if !available {
		_, available = m.serveTCPPreviewPort(item)
	}
	if !available {
		return
	}
	m.clearBannerNotice(unavailableURLBanner)
	m.clearBannerNotice(unavailableOpenURLBanner)
	m.clearBannerNotice(unavailableCopyURLBanner)
	m.clearBannerNotice(insecureURLBanner)
	m.clearBannerNotice(magicDNSPreviewUnavailableBanner)
	for _, part := range strings.Split(m.banner, " | ") {
		if strings.HasPrefix(part, "Could not read Tailscale MagicDNS status:") {
			m.clearBannerNotice(part)
		}
	}
}

func (m *workspaceModel) urlShortcutStatus() string {
	observed, insecure, tcpOnly := false, false, false
	servePreview, localListener := false, false
	if item, ok := m.selectedItem(); ok {
		localListener = m.currentLocalListenerPort(item) != 0
		for _, route := range item.Routes {
			if _, ok := workspace.ObservedHTTPSRouteURL(route); ok {
				observed = true
				continue
			}
			if strings.TrimSpace(route.URL) != "" {
				insecure = true
			}
			if workspace.RouteTransport(route) == "tcp" {
				tcpOnly = true
			}
		}
		if !observed && !insecure && m.provider != nil {
			_, servePreview = m.serveTCPPreviewPort(item)
		}
	}
	clipboard := m.clipboard != nil
	if !clipboard {
		clipboard = clipboardAvailable()
	}
	browser := browserCommandAvailable()
	status := formatURLShortcutStatus(observed, insecure, tcpOnly, servePreview, browser, clipboard)
	localStatus := "off"
	if localListener && browser {
		localStatus = "ok"
	}
	return status + "  O local[" + localStatus + "]"
}

func formatURLShortcutStatus(observed, insecure, tcpOnly, servePreview, browser, clipboard bool) string {
	openStatus, openKind := "off", "HTTPS"
	if observed {
		if browser {
			openStatus = "ok"
		}
	} else if servePreview {
		openKind = "HTTP-preview"
		if browser {
			openStatus = "ok"
		}
	} else if insecure {
		openStatus = "HTTPS-only"
	} else if tcpOnly {
		openStatus = "TCP-only"
	}
	copyStatus := "off"
	if observed || servePreview {
		if clipboard {
			copyStatus = "ok"
		} else {
			copyStatus = "URL-only"
		}
	} else if insecure {
		copyStatus = "HTTPS-only"
	}
	return fmt.Sprintf("o %s[%s]  y copy[%s]", openKind, openStatus, copyStatus)
}

func validLocalListenerPort(item exposure.ReconciledItem) int {
	if item.Listener == nil {
		return 0
	}
	listener := item.Listener.Target.Normalized()
	if listener.Validate() != nil {
		return 0
	}
	return listener.Port
}

func (m *workspaceModel) currentLocalListenerPort(item exposure.ReconciledItem) int {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil {
		return 0
	}
	return validLocalListenerPort(item)
}

func (m *workspaceModel) serveTCPPreviewPort(item exposure.ReconciledItem) (int, bool) {
	if item.Listener == nil || item.State != exposuredata.ExposureActive || m.viewErr != nil ||
		!m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil ||
		!m.view.Exposures.Authoritative || m.view.Exposures.Stale || m.view.Exposures.Error != nil {
		return 0, false
	}
	listener := item.Listener.Target.Normalized()
	if listener.Validate() != nil {
		return 0, false
	}
	for _, route := range item.Routes {
		if route.ID == "" || route.Kind != exposuredata.RouteKindRawTCP || route.Mode != exposuredata.ExposureServe ||
			route.State != exposuredata.ExposureActive || workspace.RouteTransport(route) != "tcp" {
			continue
		}
		selector, err := tailscale.ParseListenerSelector(route.ProviderKey, exposuredata.ExposureServe)
		routeTarget := route.Target.Normalized()
		if err != nil || selector.Transport != "tcp" || selector.Port != listener.Port || routeTarget.Validate() != nil ||
			!target.TargetsMatch(routeTarget, listener) {
			continue
		}
		return selector.Port, true
	}
	return 0, false
}

func magicDNSPreviewURL(dnsName string, port int) (string, error) {
	dnsName = strings.TrimSuffix(strings.TrimSpace(dnsName), ".")
	if port < 1 || port > 65535 || !validMagicDNSName(dnsName) {
		return "", fmt.Errorf("%s", magicDNSPreviewUnavailableBanner)
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(dnsName, strconv.Itoa(port)), Path: "/"}).String(), nil
}

func resolveMagicDNSPreviewURL(ctx context.Context, provider *tailscale.Adapter, port int) (string, string) {
	statusCtx, cancel := context.WithTimeout(ctx, magicDNSStatusTimeout)
	defer cancel()
	status, err := provider.Status(statusCtx)
	if err != nil {
		return "", "Could not read Tailscale MagicDNS status: " + safeMessage(err)
	}
	previewURL, err := magicDNSPreviewURL(status.Self.DNSName, port)
	if err != nil {
		return "", safeMessage(err)
	}
	return previewURL, ""
}

func validMagicDNSName(name string) bool {
	if name == "" || len(name) > 253 || !strings.Contains(name, ".") || net.ParseIP(name) != nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return true
}

func isMagicDNSPreviewURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || !validMagicDNSName(parsed.Hostname()) {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	return err == nil && port >= 1 && port <= 65535
}

func browserCommand() string {
	name := ""
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func browserCommandAvailable() bool {
	return browserCommand() != ""
}

func (m *workspaceModel) openSelectedURL() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return nil
	}
	urls := observedHTTPSURLRoutes(item)
	if len(urls) > 0 {
		m.clearStaleURLUnavailableBanner()
	}
	if len(urls) > 1 {
		m.urlAction, m.urlItemID, m.urlRoutesFingerprint, m.urlRouteIndex, m.modal = "open", item.ID, routeFingerprint(item.Routes, nil), 0, modalChooseURL
		return nil
	}
	if len(urls) == 1 {
		return m.openURL(urls[0].URL, "observed HTTPS URL")
	}
	if hasObservedNonHTTPSURL(item) {
		m.setBanner(insecureURLBanner, true)
		return nil
	}
	if port, ok := m.serveTCPPreviewPort(item); ok {
		m.clearStaleURLUnavailableBanner()
		return m.openServeTCPPreview(port)
	}
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; a browser preview is available only for an exact active Serve listener. Use a TCP client or configure an HTTPS route.", true)
		return nil
	}
	m.setBanner(unavailableOpenURLBanner, true)
	return nil
}

func (m *workspaceModel) openLocalURL() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return nil
	}
	port := m.currentLocalListenerPort(item)
	if port == 0 {
		m.setBanner(localURLUnavailableBanner, true)
		return nil
	}
	m.clearBannerNotice(localURLUnavailableBanner)
	return m.launchBrowserURL((&url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", strconv.Itoa(port)), Path: "/"}).String(), "local HTTP URL")
}

func (m *workspaceModel) openServeTCPPreview(port int) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Tailscale status is unavailable; cannot resolve the Serve TCP MagicDNS preview.", true)
		return nil
	}
	browser := browserCommand()
	if browser == "" {
		m.setBanner("Opening URLs is unsupported or unavailable on "+runtime.GOOS, true)
		return nil
	}
	provider, ctx := m.provider, m.ctx
	m.transient = "Resolving Serve TCP MagicDNS preview"
	return func() tea.Msg {
		previewURL, failure := resolveMagicDNSPreviewURL(ctx, provider, port)
		if failure != "" {
			return statusMsg{value: failure, sticky: true}
		}
		return openBrowserStatus(ctx, browser, previewURL, "Serve TCP HTTP preview")
	}
}

func observedHTTPSURLRoutes(item exposure.ReconciledItem) []exposuredata.ExposureRoute {
	routes := make([]exposuredata.ExposureRoute, 0, len(item.Routes))
	for _, route := range item.Routes {
		if _, ok := workspace.ObservedHTTPSRouteURL(route); ok {
			routes = append(routes, route)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].URL != routes[j].URL {
			return routes[i].URL < routes[j].URL
		}
		return routes[i].ID < routes[j].ID
	})
	return routes
}

func hasObservedNonHTTPSURL(item exposure.ReconciledItem) bool {
	for _, route := range item.Routes {
		if strings.TrimSpace(route.URL) != "" && !workspace.IsHTTPSURL(route.URL) {
			return true
		}
	}
	return false
}

func (m *workspaceModel) updateURLChoiceModal(_ tea.KeyMsg, key string) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.ID != m.urlItemID || routeFingerprint(item.Routes, nil) != m.urlRoutesFingerprint {
		m.modal, m.urlAction, m.urlItemID, m.urlRoutesFingerprint = modalNone, "", "", ""
		m.setBanner("Observed HTTPS route URLs changed — select a fresh URL", true)
		return m, nil
	}
	routes := observedHTTPSURLRoutes(item)
	if len(routes) < 2 {
		m.modal, m.urlAction, m.urlItemID, m.urlRoutesFingerprint = modalNone, "", "", ""
		return m, nil
	}
	switch key {
	case "esc":
		m.modal, m.urlAction, m.urlItemID, m.urlRoutesFingerprint = modalNone, "", "", ""
	case "up", "k":
		m.urlRouteIndex = (m.urlRouteIndex + len(routes) - 1) % len(routes)
	case "down", "j":
		m.urlRouteIndex = (m.urlRouteIndex + 1) % len(routes)
	case "enter":
		route := routes[clamp(m.urlRouteIndex, 0, len(routes)-1)]
		action := m.urlAction
		m.modal, m.urlAction, m.urlItemID, m.urlRoutesFingerprint = modalNone, "", "", ""
		if action == "copy" {
			return m, m.copyURLCommand(route.URL)
		}
		return m, m.openURL(route.URL, "observed HTTPS URL")
	}
	return m, nil
}

func (m *workspaceModel) openURL(url, label string) tea.Cmd {
	if !workspace.IsHTTPSURL(url) {
		m.setBanner("Only valid HTTPS URLs can be opened from Tailge", true)
		return nil
	}
	return m.launchBrowserURL(url, label)
}

func (m *workspaceModel) launchBrowserURL(url, label string) tea.Cmd {
	name := browserCommand()
	if name == "" {
		m.setBanner("Opening URLs is unsupported or unavailable on "+runtime.GOOS, true)
		return nil
	}
	m.transient = "Opening " + label
	return func() tea.Msg {
		return openBrowserStatus(m.ctx, name, url, label)
	}
}

func openBrowserStatus(parent context.Context, name, url, label string) statusMsg {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, url)
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		return statusMsg{value: "Could not open URL: " + safeMessage(err), sticky: true}
	}
	return statusMsg{value: "Opened " + label, sticky: false}
}

type statusMsg struct {
	value  string
	sticky bool
}

func (m *workspaceModel) copyURL() tea.Cmd {
	items := m.items()
	if m.selectedIdx < 0 || m.selectedIdx >= len(items) {
		m.setBanner("No service is selected", true)
		return nil
	}
	item := items[m.selectedIdx]
	urls := observedHTTPSURLRoutes(item)
	if len(urls) > 0 {
		m.clearStaleURLUnavailableBanner()
	}
	if len(urls) > 1 {
		m.urlAction, m.urlItemID, m.urlRoutesFingerprint, m.urlRouteIndex, m.modal = "copy", item.ID, routeFingerprint(item.Routes, nil), 0, modalChooseURL
		return nil
	}
	if len(urls) == 1 {
		return m.copyURLCommand(urls[0].URL)
	}
	if hasObservedNonHTTPSURL(item) {
		m.setBanner(insecureURLBanner, true)
		return nil
	}
	if port, ok := m.serveTCPPreviewPort(item); ok {
		m.clearStaleURLUnavailableBanner()
		return m.copyServeTCPPreview(port)
	}
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; a browser preview is available only for an exact active Serve listener.", true)
		return nil
	}
	m.setBanner(unavailableCopyURLBanner, true)
	return nil
}

func (m *workspaceModel) copyServeTCPPreview(port int) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Tailscale status is unavailable; cannot resolve the Serve TCP MagicDNS preview.", true)
		return nil
	}
	provider, ctx, clipboard := m.provider, m.ctx, m.clipboardOrOS()
	m.transient = "Resolving Serve TCP MagicDNS preview"
	return func() tea.Msg {
		previewURL, failure := resolveMagicDNSPreviewURL(ctx, provider, port)
		if failure != "" {
			return statusMsg{value: failure, sticky: true}
		}
		return copyPreviewURLStatus(previewURL, clipboard)
	}
}

func (m *workspaceModel) clipboardOrOS() Clipboard {
	if m.clipboard != nil {
		return m.clipboard
	}
	return OSClipboard{}
}

func (m *workspaceModel) copyURLCommand(url string) tea.Cmd {
	if !workspace.IsHTTPSURL(url) {
		m.setBanner("Only valid HTTPS URLs can be copied from Tailge", true)
		return nil
	}
	clipboard := m.clipboardOrOS()
	m.transient = "Copying observed HTTPS URL"
	return func() tea.Msg {
		return copyURLStatus(url, clipboard)
	}
}

func copyURLStatus(url string, clipboard Clipboard) statusMsg {
	var out, errOut strings.Builder
	copyURLValue(&out, &errOut, url, clipboard)
	return copyStatusResult(out.String(), errOut.String())
}

func copyPreviewURLStatus(url string, clipboard Clipboard) statusMsg {
	var out, errOut strings.Builder
	copyPreviewURLValue(&out, &errOut, url, clipboard)
	return copyStatusResult(out.String(), errOut.String())
}

func copyStatusResult(output, errorOutput string) statusMsg {
	if text := strings.TrimSpace(errorOutput); text != "" {
		value := strings.TrimSpace(output)
		if value != "" {
			value += " "
		}
		return statusMsg{value: value + text, sticky: true}
	}
	return statusMsg{value: strings.TrimSpace(output)}
}

func copySelectedURL(out, errOut io.Writer, items []exposure.ReconciledItem, selected int, clipboard Clipboard) {
	if selected < 0 || selected >= len(items) {
		return
	}
	routes := observedHTTPSURLRoutes(items[selected])
	if len(routes) == 1 {
		copyURLValue(out, errOut, routes[0].URL, clipboard)
		return
	}
	if len(routes) > 1 {
		fmt.Fprintln(out, "Multiple observed HTTPS URLs are available; choose one in the workspace.")
		return
	}
	fmt.Fprintln(out, "No HTTPS URL is available for the selected item.")
}

func copyURLValue(out, errOut io.Writer, url string, clipboard Clipboard) {
	if !workspace.IsHTTPSURL(url) {
		fmt.Fprintln(errOut, "only valid HTTPS URLs may be copied")
		return
	}
	copyValueToClipboard(out, errOut, "HTTPS URL", url, clipboard)
}

func copyPreviewURLValue(out, errOut io.Writer, url string, clipboard Clipboard) {
	if !isMagicDNSPreviewURL(url) {
		fmt.Fprintln(errOut, "only a valid Tailscale HTTP preview URL may be copied")
		return
	}
	copyValueToClipboard(out, errOut, "HTTP preview URL", url, clipboard)
}

func copyValueToClipboard(out, errOut io.Writer, label, value string, clipboard Clipboard) {
	fmt.Fprintf(out, "%s: %s\n", label, value)
	if clipboard == nil {
		fmt.Fprintln(errOut, "clipboard unavailable: no clipboard integration is configured.\nCopy the visible URL with normal terminal text selection.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := clipboard.Copy(ctx, value)
	cancel()
	if err != nil {
		fmt.Fprintf(errOut, "clipboard unavailable: %s\nCopy the visible URL with normal terminal text selection.\n", err)
	} else {
		fmt.Fprintln(out, "URL copied to clipboard.")
	}
}

type Clipboard interface {
	Copy(context.Context, string) error
}

// OSC52Clipboard copies through the terminal stream instead of the operating
// system clipboard on the machine running Tailge. This is what makes copying
// work for SSH, Mosh, and terminal multiplexers: the terminal client receives
// the sequence and updates its own clipboard.
type OSC52Clipboard struct {
	Output io.Writer
	Mode   osc52.Mode
	mu     sync.Mutex
}

func (c *OSC52Clipboard) Copy(ctx context.Context, value string) error {
	if c == nil || c.Output == nil {
		return fmt.Errorf("terminal output is unavailable")
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := osc52.New(value).Mode(c.Mode).WriteTo(c.Output)
	return err
}

func terminalClipboard(out io.Writer) Clipboard {
	if out == nil {
		return nil
	}
	return &OSC52Clipboard{Output: out, Mode: terminalClipboardMode()}
}

func terminalClipboardMode() osc52.Mode {
	// GNU screen does not pass OSC 52 directly, so wrap it in DCS. tmux can
	// handle the normal OSC 52 sequence when set-clipboard is enabled; using
	// TmuxMode unconditionally would instead require allow-passthrough.
	term := os.Getenv("TERM")
	if os.Getenv("STY") != "" || (strings.HasPrefix(term, "screen") && os.Getenv("TMUX") == "") {
		return osc52.ScreenMode
	}
	return osc52.DefaultMode
}

type OSClipboard struct{}

func clipboardCommand() (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("pbcopy"); err == nil {
			return path, nil
		}
	case "linux":
		if path, err := exec.LookPath("wl-copy"); err == nil {
			return path, nil
		} else if path, err := exec.LookPath("xclip"); err == nil {
			return path, []string{"-selection", "clipboard"}
		}
	}
	return "", nil
}

func clipboardAvailable() bool {
	name, _ := clipboardCommand()
	return name != ""
}

func (OSClipboard) Copy(ctx context.Context, value string) error {
	name, args := clipboardCommand()
	if name == "" {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("clipboard is unsupported on %s", runtime.GOOS)
		}
		return fmt.Errorf("no supported clipboard command was found")
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(value)
	return command.Run()
}

type ClipboardFunc func(context.Context, string) error

func (f ClipboardFunc) Copy(ctx context.Context, value string) error {
	if f == nil {
		return fmt.Errorf("clipboard function is nil")
	}
	return f(ctx, value)
}
