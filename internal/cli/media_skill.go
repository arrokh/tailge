package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/media"
)

const defaultMediaListenAddress = "127.0.0.1:8765"

const tailgeAgentSkill = `---
name: tailge
description: Operate Tailge to inspect local services, manage exact Tailscale exposure routes, or browse local media. Use when a task involves Tailge commands, local service exposure, or Tailge's media server.
---

# Tailge

Tailge discovers local TCP listeners, reports Tailscale exposure state, and manages exact Serve/Funnel routes. It can also serve selected local image and video directories through a read-only web browser.

## Inspect services and exposure

- Run ` + "`tailge scan`" + ` to inspect local TCP listeners; discovery does not prove that a listener speaks HTTP.
- Run ` + "`tailge exposure status`" + ` to inspect observed routes and readiness.
- Run ` + "`tailge doctor --tailscale`" + ` to inspect Tailscale readiness.
- Use ` + "`tailge exposure serve ADDRESS:PORT`" + ` for private raw-TCP exposure.
- Use ` + "`tailge exposure funnel ADDRESS:PORT --confirm-public`" + ` only when public exposure is intended.
- Use ` + "`tailge exposure http serve ADDRESS:PORT --root --https-port PORT`" + ` only when the local service is known to speak HTTP.
- Use ` + "`tailge exposure http funnel ADDRESS:PORT --path SLUG --confirm-public`" + ` only after reviewing that every handler on the shared HTTPS endpoint becomes public.
- Disable one exact route with ` + "`tailge exposure disable ADDRESS:PORT --mode serve|funnel`" + `, or select an exact HTTP path/root with ` + "`tailge exposure http disable`" + `.

## Serve local media

Run ` + "`tailge media serve DIRECTORY [DIRECTORY ...]`" + ` to start Tailge's read-only media web server. Open the printed URL in a browser. The server lists supported images and videos, allows navigation into subdirectories, renders images, and plays videos with browser-native controls. It supports range requests for video seeking.

The server binds to ` + "`127.0.0.1:8765`" + ` by default. It accepts browser Host headers for localhost, loopback, and the bound IP; this blocks DNS-rebinding access through an attacker-controlled domain. ` + "`--listen ADDRESS:PORT`" + ` changes the bind address; direct non-loopback HTTP has no authentication or TLS and makes selected files reachable to other machines on that network. ` + "`--allow-host HOST`" + ` adds one exact browser host for reverse proxies or DNS aliases. The server serves only regular supported media files inside the selected directory trees, omits dot-prefixed entries and unsupported files, and does not upload, edit, or delete files. Keep it running while the browser uses it; Ctrl-C stops it. Video playback depends on the browser's codec support.

To make a running media server available privately over Tailscale, start it with ` + "`--allow-host MACHINE-DNS-NAME`" + `, then configure a private HTTPS root for its observed local listener with ` + "`tailge exposure http serve ADDRESS:PORT --root --https-port PORT`" + `. Use Funnel only after deliberately reviewing its public scope. In the interactive workspace, enter ` + "`:media serve \"DIRECTORY\"`" + ` in the ` + "`:`" + ` command palette; Ctrl-C stops the foreground server and returns to the workspace.

Install the agent skill with ` + "`tailge skill install`" + ` or ` + "`:skill install`" + ` in the workspace. To remove it, use ` + "`tailge skill uninstall --confirm`" + ` or the Cancel-first ` + "`:skill uninstall`" + ` palette action; removal is limited to an unchanged Tailge-generated file.

## Safety

- Treat listener discovery as TCP-only. Choose HTTP routes only when the backend is known to speak HTTP.
- Review exact route identity and ownership before replacing or removing a route. Tailge fails closed on stale, ambiguous, incomplete, or unverified state.
- Funnel is public internet exposure and requires ` + "`--confirm-public`" + `.
- Tailge does not implicitly start, stop, or restart other services. Its TUI process-termination action requires confirmation, revalidates process identity, and sends only SIGTERM.
- Prefer read-only inspection before a mutation. Never use broad Tailscale reset commands to work around an exact-route refusal.
`

func (a app) mediaCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		return reportError(stderr, invalidArgs("media requires serve DIRECTORY [DIRECTORY ...]"))
	}
	parsed, err := parseFlagValues(args[1:], map[string]bool{"listen": true, "allow-host": true})
	if err != nil {
		return reportError(stderr, err)
	}
	if len(parsed.positionals) == 0 {
		return reportError(stderr, invalidArgs("media serve requires at least one directory"))
	}
	listenAddress := defaultMediaListenAddress
	if value, supplied := parsed.values["listen"]; supplied {
		listenAddress = strings.TrimSpace(value)
		if listenAddress == "" {
			return reportError(stderr, invalidArgs("--listen requires a non-empty ADDRESS:PORT"))
		}
	}
	var extraAllowedHost string
	if value, supplied := parsed.values["allow-host"]; supplied {
		extraAllowedHost, err = parseAllowedHost(value)
		if err != nil {
			return reportError(stderr, invalidArgs(err.Error()))
		}
	}
	browser, err := media.New(parsed.positionals)
	if err != nil {
		return reportError(stderr, fault.WrapError(fault.ErrInvalidInput, "media", "cannot serve the selected media directories", false, "invalid", "Choose one or more readable directories and retry.", err))
	}
	defer browser.Close()
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return reportError(stderr, fault.WrapError(fault.ErrOperation, "media", "cannot listen on "+listenAddress, false, "listen_failed", "Choose an available TCP address with `--listen ADDRESS:PORT`.", err))
	}
	defer listener.Close()

	allowedHosts := mediaListenerHosts(listener.Addr())
	if extraAllowedHost != "" {
		allowedHosts = append(allowedHosts, extraAllowedHost)
	}
	server := &http.Server{
		Handler:           browser.Handler(allowedHosts...),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	address := mediaBrowserURL(listener.Addr())
	fmt.Fprintf(stdout, "Tailge Media serving %d director%s at %s\n", len(parsed.positionals), pluralSuffix(len(parsed.positionals)), address)
	fmt.Fprintln(stdout, "Press Ctrl-C to stop.")
	if !isLoopbackAddress(listener.Addr()) {
		fmt.Fprintln(stderr, "warning: non-loopback HTTP binding has no authentication or TLS and makes selected media reachable to other machines that can access this address")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	err = server.Serve(listener)
	stop()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return 0
	}
	return reportError(stderr, fault.WrapError(fault.ErrOperation, "media", "media server stopped unexpectedly", true, "server_failed", "Check whether the listener was closed and restart the media server.", err))
}

func pluralSuffix(count int) string {
	if count == 1 {
		return "y"
	}
	return "ies"
}

func parseAllowedHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("--allow-host must be a hostname or IP address without a port")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if strings.ContainsAny(host, "/\\:@?#[] \t\r\n") {
		return "", fmt.Errorf("--allow-host must be a hostname or IP address without a port")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) > 253 {
		return "", fmt.Errorf("--allow-host hostname is too long")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("--allow-host must be a valid hostname or IP address")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", fmt.Errorf("--allow-host must be a valid hostname or IP address")
			}
		}
	}
	return host, nil
}

func mediaListenerHosts(address net.Addr) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return hosts
	}
	if !tcp.IP.IsUnspecified() {
		return append(hosts, tcp.IP.String())
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return hosts
	}
	for _, address := range interfaces {
		ipText, _, hasMask := strings.Cut(address.String(), "/")
		if !hasMask {
			continue
		}
		if ip := net.ParseIP(ipText); ip != nil {
			hosts = append(hosts, ip.String())
		}
	}
	return hosts
}

func mediaBrowserURL(address net.Addr) string {
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return "http://" + address.String() + "/"
	}
	host := tcp.IP.String()
	if tcp.IP.IsUnspecified() {
		if tcp.IP.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(tcp.Port)) + "/"
}

func isLoopbackAddress(address net.Addr) bool {
	tcp, ok := address.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

func (a app) skillCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return reportError(stderr, invalidArgs("skill requires install [--force] or uninstall --confirm"))
	}
	path := filepath.Join(".agents", "skills", "tailge", "SKILL.md")
	switch args[0] {
	case "install":
		parsed, err := parseFlagValues(args[1:], map[string]bool{"force": false})
		if err != nil {
			return reportError(stderr, err)
		}
		if len(parsed.positionals) != 0 {
			return reportError(stderr, invalidArgs("skill install does not accept positional arguments"))
		}
		if err := installSkill(path, []byte(tailgeAgentSkill), parsed.bools["force"]); err != nil {
			message := "cannot install the Tailge agent skill: " + err.Error()
			return reportError(stderr, fault.WrapError(fault.ErrOperation, "skill", message, false, "install_failed", "Run this command from the project where agents should learn Tailge, and use `--force` only to replace an existing regular skill file.", err))
		}
		fmt.Fprintf(stdout, "Tailge agent skill installed at %s\n", path)
		return 0
	case "uninstall":
		parsed, err := parseFlagValues(args[1:], map[string]bool{"confirm": false})
		if err != nil {
			return reportError(stderr, err)
		}
		if len(parsed.positionals) != 0 {
			return reportError(stderr, invalidArgs("skill uninstall does not accept positional arguments"))
		}
		if !parsed.bools["confirm"] {
			return reportError(stderr, invalidArgs("skill uninstall requires --confirm"))
		}
		removed, err := uninstallSkill(path, []byte(tailgeAgentSkill))
		if err != nil {
			message := "cannot uninstall the Tailge agent skill: " + err.Error()
			return reportError(stderr, fault.WrapError(fault.ErrOperation, "skill", message, false, "uninstall_failed", "The skill is removed only when it is an unchanged Tailge-generated regular file; inspect the project and retry.", err))
		}
		if removed {
			fmt.Fprintf(stdout, "Tailge agent skill removed from %s\n", path)
		} else {
			fmt.Fprintln(stdout, "Tailge agent skill is not installed in this project.")
		}
		return 0
	default:
		return reportError(stderr, invalidArgs("skill requires install [--force] or uninstall --confirm"))
	}
}

func ensureSkillDirectory(directory string) error {
	if filepath.IsAbs(directory) {
		return os.MkdirAll(directory, 0o755)
	}
	clean := filepath.Clean(directory)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("skill directory must stay within the current project")
	}
	current := "."
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			if err := os.Mkdir(next, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(next)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("refusing to use non-directory or symlink path %q", next)
		}
		current = next
	}
	return nil
}

func installSkill(path string, content []byte, force bool) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("skill path cannot be empty")
	}
	directory := filepath.Dir(path)
	if err := ensureSkillDirectory(directory); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	if !force {
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("skill already exists at %q (use --force to replace it)", path)
			}
			return fmt.Errorf("create skill file: %w", err)
		}
		if err := file.Chmod(0o644); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("set skill file permissions: %w", err)
		}
		if _, err := file.Write(content); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write skill file: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("sync skill file: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return fmt.Errorf("close skill file: %w", err)
		}
		return nil
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular skill path %q", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect skill path: %w", err)
	}

	file, err := os.CreateTemp(directory, ".tailge-skill-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary skill file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return fmt.Errorf("set skill file permissions: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write skill file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync skill file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close skill file: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("install skill file: %w", err)
	}
	return nil
}

func uninstallSkill(path string, expected []byte) (bool, error) {
	clean := filepath.Clean(path)
	if filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false, fmt.Errorf("skill path must stay within the current project")
	}
	root, err := os.OpenRoot(".")
	if err != nil {
		return false, fmt.Errorf("open project directory: %w", err)
	}
	defer root.Close()
	if err := validateSkillDirectoryWithinRoot(root, filepath.Dir(clean)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect skill directory: %w", err)
	}
	info, err := root.Lstat(clean)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect skill file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing to remove non-regular skill path %q", path)
	}
	contents, err := root.ReadFile(clean)
	if err != nil {
		return false, fmt.Errorf("read skill file: %w", err)
	}
	if !bytes.Equal(contents, expected) {
		return false, fmt.Errorf("refusing to remove modified skill file %q", path)
	}
	if err := root.Remove(clean); err != nil {
		return false, fmt.Errorf("remove skill file: %w", err)
	}
	return true, nil
}

func validateSkillDirectoryWithinRoot(root *os.Root, directory string) error {
	clean := filepath.Clean(directory)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("skill directory must stay within the current project")
	}
	current := "."
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		next := filepath.Join(current, component)
		info, err := root.Lstat(next)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("refusing to use non-directory or symlink path %q", next)
		}
		current = next
	}
	return nil
}
