package tui

// Observed HTTPS URL actions, browser launching, and clipboard transports.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/workspace"
	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
)

const unavailableURLBanner = "No observed HTTPS URL is available"
const unavailableOpenURLBanner = "No observed HTTPS URL is available. o only opens observed HTTPS routes; use p to configure one."
const unavailableCopyURLBanner = "No observed HTTPS URL is available. y only copies observed HTTPS routes; use p to configure one."
const insecureURLBanner = "Observed route URL is not HTTPS. Configure an explicit HTTPS route before opening or copying it."

func (m *workspaceModel) clearStaleURLUnavailableBanner() {
	if m.banner == "" {
		return
	}
	item, ok := m.selectedItem()
	if !ok || len(observedHTTPSURLRoutes(item)) == 0 {
		return
	}
	m.clearBannerNotice(unavailableURLBanner)
	m.clearBannerNotice(unavailableOpenURLBanner)
	m.clearBannerNotice(unavailableCopyURLBanner)
	m.clearBannerNotice(insecureURLBanner)
}

func (m *workspaceModel) urlShortcutStatus() string {
	observed, insecure, tcpOnly := false, false, false
	if item, ok := m.selectedItem(); ok {
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
	}
	clipboard := m.clipboard != nil
	if !clipboard {
		clipboard = clipboardAvailable()
	}
	return formatURLShortcutStatus(observed, insecure, tcpOnly, browserCommandAvailable(), clipboard)
}

func formatURLShortcutStatus(observed, insecure, tcpOnly, browser, clipboard bool) string {
	openStatus := "off"
	if observed && browser {
		openStatus = "ok"
	} else if !observed && insecure {
		openStatus = "HTTPS-only"
	} else if !observed && tcpOnly {
		openStatus = "TCP-only"
	}
	copyStatus := "off"
	if observed {
		if clipboard {
			copyStatus = "ok"
		} else {
			copyStatus = "URL-only"
		}
	} else if insecure {
		copyStatus = "HTTPS-only"
	}
	return fmt.Sprintf("o HTTPS[%s]  y copy[%s]", openStatus, copyStatus)
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
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no HTTPS URL exists. Use a TCP client or configure an HTTPS route.", true)
		return nil
	}
	m.setBanner(unavailableOpenURLBanner, true)
	return nil
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
	name := browserCommand()
	if name == "" {
		m.setBanner("Opening URLs is unsupported or unavailable on "+runtime.GOOS, true)
		return nil
	}
	m.transient = "Opening " + label
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, name, url)
		command.WaitDelay = 2 * time.Second
		err := command.Run()
		if err != nil {
			return statusMsg{value: "Could not open URL: " + safeMessage(err), sticky: true}
		}
		return statusMsg{value: "Opened " + label, sticky: false}
	}
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
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no HTTPS URL exists to copy", true)
		return nil
	}
	m.setBanner(unavailableCopyURLBanner, true)
	return nil
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
	if text := strings.TrimSpace(errOut.String()); text != "" {
		value := strings.TrimSpace(out.String())
		if value != "" {
			value += " "
		}
		return statusMsg{value: value + text, sticky: true}
	}
	return statusMsg{value: strings.TrimSpace(out.String())}
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
	fmt.Fprintf(out, "HTTPS URL: %s\n", url)
	if clipboard == nil {
		fmt.Fprintln(errOut, "clipboard unavailable: no clipboard integration is configured.\nCopy the visible URL with normal terminal text selection.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := clipboard.Copy(ctx, url)
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
