package tui

// Observed/local URL actions, browser launching, and clipboard transports.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/workspace"
	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *workspaceModel) urlShortcutStatus() string {
	observed, tcpOnly, browserFallback, local := false, false, false, false
	if item, ok := m.selectedItem(); ok {
		for _, route := range item.Routes {
			if _, ok := workspace.ObservedRouteURL(route); ok {
				observed = true
				continue
			}
			if workspace.RouteTransport(route) == "tcp" {
				if route.Mode == exposuredata.ExposureServe && m.provider != nil {
					browserFallback = true
				} else {
					tcpOnly = true
				}
			}
		}
		_, local = localURL(item)
	}
	clipboard := m.clipboard != nil
	if !clipboard {
		clipboard = clipboardAvailable()
	}
	return formatURLShortcutStatus(observed, tcpOnly, browserFallback, local, browserCommandAvailable(), clipboard)
}

func formatURLShortcutStatus(observed, tcpOnly, browserFallback, local, browser, clipboard bool) string {
	observedStatus := "off"
	if (observed || browserFallback) && browser {
		observedStatus = "ok"
	} else if tcpOnly && !observed && !browserFallback {
		observedStatus = "TCP-only"
	}
	localStatus := "off"
	if local && browser {
		localStatus = "ok"
	}
	copyStatus := "off"
	if observed || browserFallback {
		if clipboard {
			copyStatus = "ok"
		} else {
			copyStatus = "URL-only"
		}
	}
	return fmt.Sprintf("o observed[%s]  O localhost[%s]  y copy[%s]", observedStatus, localStatus, copyStatus)
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
	if route, url, ok := observedURLActionRoute(item); ok {
		if url != "" {
			return m.openURL(url, "observed URL")
		}
		return m.resolveServeTCPBrowserURL(route)
	}
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no browser URL exists. Use a TCP client.", true)
		return nil
	}
	m.setBanner("No observed exposure URL is available", true)
	return nil
}

func observedURLActionRoute(item exposure.ReconciledItem) (exposuredata.ExposureRoute, string, bool) {
	for _, route := range item.Routes {
		if url, ok := workspace.ObservedRouteURL(route); ok {
			return route, url, true
		}
	}
	for _, route := range item.Routes {
		if workspace.RouteTransport(route) == "tcp" && route.Mode == exposuredata.ExposureServe {
			return route, "", true
		}
	}
	return exposuredata.ExposureRoute{}, "", false
}

func (m *workspaceModel) resolveServeTCPBrowserURL(route exposuredata.ExposureRoute) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Observed Serve route has no provider endpoint resolver", true)
		return nil
	}
	m.transient = "Resolving observed Tailscale endpoint"
	return func() tea.Msg {
		url, err := m.fetchServeTCPPreviewURL(route)
		return resolvedObservedURLMsg{url: url, err: err}
	}
}

func (m *workspaceModel) fetchServeTCPPreviewURL(route exposuredata.ExposureRoute) (string, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	status, err := m.provider.Status(ctx)
	if err != nil {
		return "", err
	}
	return workspace.ServeTCPBrowserURL(status, route)
}

func (m *workspaceModel) openSelectedLocalURL() tea.Cmd {
	item, ok := m.selectedItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return nil
	}
	url, ok := localURL(item)
	if !ok {
		m.setBanner("No current local listener is available", true)
		return nil
	}
	return m.openURL(url, "local URL")
}

func localURL(item exposure.ReconciledItem) (string, bool) {
	if item.Listener == nil || item.Listener.Target.Port < 1 || item.Listener.Target.Port > 65535 {
		return "", false
	}
	return "http://localhost:" + strconv.Itoa(item.Listener.Target.Port) + "/", true
}

func (m *workspaceModel) openURL(url, label string) tea.Cmd {
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

type resolvedObservedURLMsg struct {
	url string
	err error
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
	if route, url, ok := observedURLActionRoute(item); ok {
		if url != "" {
			return m.copyURLCommand(url)
		}
		return m.resolveServeTCPCopyURL(route)
	}
	if selector := workspace.RawTCPRouteSelector(item); selector != "" {
		m.setBanner("Observed route "+selector+" is TCP-only; no browser URL exists to copy", true)
		return nil
	}
	m.setBanner("No observed exposure URL is available", true)
	return nil
}

func (m *workspaceModel) clipboardOrOS() Clipboard {
	if m.clipboard != nil {
		return m.clipboard
	}
	return OSClipboard{}
}

func (m *workspaceModel) copyURLCommand(url string) tea.Cmd {
	clipboard := m.clipboardOrOS()
	m.transient = "Copying observed URL"
	return func() tea.Msg {
		return copyURLStatus(url, clipboard)
	}
}

func (m *workspaceModel) resolveServeTCPCopyURL(route exposuredata.ExposureRoute) tea.Cmd {
	if m.provider == nil {
		m.setBanner("Observed Serve route has no provider endpoint resolver", true)
		return nil
	}
	clipboard := m.clipboardOrOS()
	m.transient = "Resolving observed Tailscale endpoint"
	return func() tea.Msg {
		url, err := m.fetchServeTCPPreviewURL(route)
		if err != nil {
			return statusMsg{value: "Observed URL: " + safeMessage(err), sticky: true}
		}
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
	for _, route := range items[selected].Routes {
		if url, ok := workspace.ObservedRouteURL(route); ok {
			copyURLValue(out, errOut, url, clipboard)
			return
		}
	}
	fmt.Fprintln(out, "No URL is available for the selected item.")
}

func copyURLValue(out, errOut io.Writer, url string, clipboard Clipboard) {
	fmt.Fprintf(out, "URL: %s\n", url)
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
