package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/runner"
)

type Discoverer interface {
	List(context.Context) (model.ListenerSnapshot, error)
}

type OSDiscoverer struct {
	Runner runner.Runner
	OS     string
	Now    func() time.Time
}

func New(r runner.Runner) *OSDiscoverer {
	return &OSDiscoverer{Runner: r, OS: runtime.GOOS, Now: time.Now}
}

func (d *OSDiscoverer) List(ctx context.Context) (model.ListenerSnapshot, error) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	snapshot := model.ListenerSnapshot{At: now, Source: "lsof", Authoritative: false, Listeners: []model.Listener{}}
	if d.Runner == nil {
		err := model.NewError(model.ErrDependency, "discovery", "no command runner is configured", true, "unavailable", "Retry the scan.")
		snapshot.Error = ptr(err.Safe())
		return snapshot, err
	}
	if d.OS != "darwin" && d.OS != "linux" {
		err := model.NewError(model.ErrUnsupported, "discovery", "local listener discovery is unsupported on "+d.OS, false, "unsupported", "Use a supported macOS or Linux build.")
		snapshot.Error = ptr(err.Safe())
		return snapshot, err
	}
	commands := []struct {
		name string
		args []string
	}{
		{"lsof", []string{"-nP", "-iTCP", "-sTCP:LISTEN", "-FpcnPT"}},
	}
	if d.OS == "linux" {
		commands = append(commands, struct {
			name string
			args []string
		}{"ss", []string{"-Hlnpt4"}}, struct {
			name string
			args []string
		}{"ss", []string{"-Hlnpt6"}})
	}
	var lastErr error
	for i, command := range commands {
		result, err := d.Runner.Run(ctx, command.name, command.args...)
		if err == nil || (result.ExitCode == 1 && strings.TrimSpace(result.Stdout) == "" && strings.TrimSpace(result.Stderr) == "") || strings.TrimSpace(result.Stdout) != "" {
			var listeners []model.Listener
			var parseErr error
			if command.name == "lsof" {
				listeners, parseErr = ParseLsof(result.Stdout, now)
			} else {
				listeners, parseErr = ParseSS(result.Stdout, now)
			}
			if parseErr != nil {
				snapshot.Listeners = append(snapshot.Listeners, listeners...)
				snapshot.Listeners = deduplicate(snapshot.Listeners)
				snapshot.Warnings = append(snapshot.Warnings, parseErr.Error())
				snapshot.Error = ptr(model.NewError(model.ErrUnknown, "discovery", parseErr.Error(), true, "partial", "Refresh after checking the listener provider.").Safe())
				d.enrich(ctx, &snapshot)
				return snapshot, model.NewError(model.ErrUnknown, "discovery", "listener output could not be parsed", true, "partial", "Retry the scan; no exposure changes were made.")
			}
			if result.Truncated {
				snapshot.Listeners = append(snapshot.Listeners, listeners...)
				snapshot.Listeners = deduplicate(snapshot.Listeners)
				d.enrich(ctx, &snapshot)
				err := model.NewError(model.ErrUnknown, "discovery", "listener output exceeded the capture limit", true, "partial", "Reduce the number of listeners or retry the scan.")
				snapshot.Error = ptr(err.Safe())
				return snapshot, err
			}
			snapshot.Listeners = append(snapshot.Listeners, listeners...)
			if strings.TrimSpace(result.Stderr) != "" {
				snapshot.Authoritative = false
				snapshot.Warnings = append(snapshot.Warnings, redactCommandLine(strings.TrimSpace(result.Stderr)))
				sourceErr := classifyCommandError("discovery", command.name, result, errors.New("listener provider emitted diagnostics"))
				snapshot.Error = ptr(sourceErr.Safe())
				d.enrich(ctx, &snapshot)
				return snapshot, sourceErr
			}
			if err != nil && !(result.ExitCode == 1 && strings.TrimSpace(result.Stdout) == "") {
				snapshot.Authoritative = false
				sourceErr := classifyCommandError("discovery", command.name, result, err)
				snapshot.Error = ptr(sourceErr.Safe())
				d.enrich(ctx, &snapshot)
				return snapshot, sourceErr
			}
			snapshot.Authoritative = err == nil || (result.ExitCode == 1 && strings.TrimSpace(result.Stdout) == "")
			snapshot.Source = command.name
			if err != nil {
				snapshot.Warnings = append(snapshot.Warnings, command.name+" returned no listeners")
			}
			// lsof is the preferred complete source. On Linux, ss fallback output
			// from multiple address families is combined and de-duplicated below.
			if command.name == "lsof" || d.OS != "linux" || i == len(commands)-1 {
				snapshot.Listeners = deduplicate(snapshot.Listeners)
				d.enrich(ctx, &snapshot)
				return snapshot, nil
			}
			continue
		}
		lastErr = err
		if !isMissingCommand(err) {
			// A real lsof failure is not made authoritative; try Linux fallback
			// where applicable, otherwise return the useful error immediately.
			if command.name != "lsof" || d.OS != "linux" {
				appErr := classifyCommandError("discovery", command.name, result, err)
				snapshot.Error = ptr(appErr.Safe())
				return snapshot, appErr
			}
		}
	}
	appErr := classifyCommandError("discovery", "lsof/ss", runner.Result{}, lastErr)
	snapshot.Error = ptr(appErr.Safe())
	return snapshot, appErr
}

const processTerminationWait = 2 * time.Second

// Terminate sends SIGTERM to the exact process owning a selected listener. It
// re-discovers the listener immediately before signalling and never escalates
// to SIGKILL.
func (d *OSDiscoverer) Terminate(ctx context.Context, requested model.Listener) error {
	if err := ctx.Err(); err != nil {
		return model.WrapError(model.ErrCancelled, "discovery", "process termination was cancelled", true, "cancelled", "Retry after reviewing the selected process.", err)
	}
	if d == nil || d.Runner == nil {
		return model.NewError(model.ErrDependency, "discovery", "process termination is unavailable", true, "unavailable", "Refresh listener discovery and retry.")
	}
	if d.OS != "darwin" && d.OS != "linux" {
		return model.NewError(model.ErrUnsupported, "discovery", "process termination is unsupported on "+d.OS, false, "unsupported", "Use a supported macOS or Linux build.")
	}
	if requested.PID <= 1 || requested.PID == os.Getpid() {
		return model.NewError(model.ErrUnsafe, "discovery", "refusing to terminate this or a protected process", false, "unsafe", "Select an application listener with a different PID.")
	}
	if strings.TrimSpace(requested.Process) == "" || strings.TrimSpace(requested.ProcessStart) == "" {
		return model.NewError(model.ErrUnknown, "discovery", "selected process identity is incomplete", true, "unknown", "Refresh until the process name and start identity are available before terminating it.")
	}
	current, err := d.List(ctx)
	if err != nil {
		return err
	}
	if !current.Authoritative || current.Error != nil {
		return model.NewError(model.ErrUnknown, "discovery", "current listener state is not authoritative", true, "unknown", "Refresh listener discovery before terminating a process.")
	}
	matches := exactProcessListeners(current.Listeners, requested)
	if len(matches) != 1 {
		return model.NewError(model.ErrUnsafe, "discovery", "selected process or listener changed", true, "changed", "Refresh and select the current process before terminating it.")
	}
	// Re-check the stable process-start identity immediately before signalling;
	// PID equality alone is unsafe because the OS may have reused the PID.
	if currentStart := processStartIdentity(ctx, d.OS, requested.PID); currentStart == "" || currentStart != requested.ProcessStart {
		return model.NewError(model.ErrUnsafe, "discovery", "selected process identity changed", true, "changed", "Refresh and select the current process before terminating it.")
	}
	if err := syscall.Kill(requested.PID, syscall.SIGTERM); err != nil {
		return model.WrapError(model.ErrOperation, "discovery", "could not send SIGTERM to "+strconv.Itoa(requested.PID), true, "failed", "Check process permissions and retry; no SIGKILL was attempted.", err)
	}
	deadline := time.NewTimer(processTerminationWait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		exists, existsErr := processExistsIdentity(ctx, d.OS, requested.PID, requested.ProcessStart)
		if existsErr != nil {
			return model.WrapError(model.ErrUnknown, "discovery", "could not verify process termination", true, "unknown", "Inspect the process manually; no SIGKILL was attempted.", existsErr)
		}
		if !exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return model.WrapError(model.ErrCancelled, "discovery", "process termination verification was cancelled", true, "cancelled", "Inspect the process manually; no SIGKILL was attempted.", ctx.Err())
		case <-deadline.C:
			return model.NewError(model.ErrTimeout, "discovery", "process did not exit after SIGTERM", true, "unverified", "The process may ignore SIGTERM; inspect it manually. Tailge did not send SIGKILL.")
		case <-tick.C:
		}
	}
}

func exactProcessListeners(listeners []model.Listener, requested model.Listener) []model.Listener {
	matches := make([]model.Listener, 0, 1)
	for _, listener := range listeners {
		if listener.PID != requested.PID || listener.Target.Normalized().Key() != requested.Target.Normalized().Key() || listener.Process != requested.Process {
			continue
		}
		if requested.CommandLine != "" && listener.CommandLine != requested.CommandLine {
			continue
		}
		if requested.ProcessStart != "" && listener.ProcessStart != requested.ProcessStart {
			continue
		}
		matches = append(matches, listener)
	}
	return matches
}

func processExists(pid int) (bool, error) {
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func processExistsIdentity(ctx context.Context, goos string, pid int, expectedStart string) (bool, error) {
	start := processStartIdentity(ctx, goos, pid)
	if start == "" {
		exists, err := processExists(pid)
		if err != nil || !exists {
			return exists, err
		}
		return false, fmt.Errorf("process identity is unavailable for live PID %d", pid)
	}
	return start == expectedStart, nil
}

func (d *OSDiscoverer) enrich(ctx context.Context, snapshot *model.ListenerSnapshot) {
	for i := range snapshot.Listeners {
		l := &snapshot.Listeners[i]
		if l.PID == 0 {
			l.Metadata = model.MetadataPartial
			continue
		}
		if command := processCommand(ctx, d.OS, l.PID); command != "" {
			l.CommandLine = redactCommandLine(command)
			if strings.Contains(l.CommandLine, "[truncated]") {
				l.Metadata = model.MetadataPartial
			}
		}
		if start := processStartIdentity(ctx, d.OS, l.PID); start != "" {
			l.ProcessStart = start
		} else {
			l.Metadata = model.MetadataPartial
		}
		if l.Process == "" || l.Name == "" {
			l.Metadata = model.MetadataPartial
		}
		if l.CommandLine == "" {
			// Process command line is optional and may be hidden by OS policy.
			if l.Metadata == model.MetadataComplete {
				l.Metadata = model.MetadataPartial
			}
		}
	}
}

func processStartIdentity(ctx context.Context, goos string, pid int) string {
	if pid <= 0 {
		return ""
	}
	if goos == "linux" {
		file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return ""
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 4096))
		if err != nil {
			return ""
		}
		// The comm field may contain spaces and parentheses. The last closing
		// parenthesis is the only safe delimiter before the remaining fields.
		closeParen := strings.LastIndexByte(string(data), ')')
		if closeParen < 0 {
			return ""
		}
		fields := strings.Fields(string(data)[closeParen+1:])
		// fields[0] is state (field 3); starttime is field 22.
		if len(fields) <= 19 || fields[19] == "" {
			return ""
		}
		return "linux:" + fields[19]
	}
	if goos == "darwin" {
		command := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
		command.WaitDelay = 2 * time.Second
		var output boundedMetadataBuffer
		output.max = 256
		command.Stdout = &output
		if err := command.Run(); err != nil {
			return ""
		}
		value := strings.TrimSpace(sanitizeText(output.String()))
		if value == "" {
			return ""
		}
		return "darwin:" + value
	}
	return ""
}

func processCommand(ctx context.Context, goos string, pid int) string {
	const maxMetadata = 16 * 1024
	if goos == "linux" {
		file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return ""
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxMetadata+1))
		if err != nil {
			return ""
		}
		truncated := len(data) > maxMetadata
		if truncated {
			data = data[:maxMetadata]
		}
		value := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
		if truncated {
			value += "…[truncated]"
		}
		return value
	}
	command := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "command=")
	command.WaitDelay = 2 * time.Second
	var output, diagnostics boundedMetadataBuffer
	output.max, diagnostics.max = maxMetadata, maxMetadata
	command.Stdout, command.Stderr = &output, &diagnostics
	if err := command.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(output.String())
}

type boundedMetadataBuffer struct {
	data      []byte
	max       int
	truncated bool
}

func (b *boundedMetadataBuffer) Write(p []byte) (int, error) {
	remaining := b.max - len(b.data)
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.data = append(b.data, p[:remaining]...)
		b.truncated = true
		return len(p), nil
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *boundedMetadataBuffer) String() string {
	value := string(b.data)
	if b.truncated {
		value += "…[truncated]"
	}
	return value
}

var sensitiveText = regexp.MustCompile(`(?i)((?:token|password|secret|authorization|bearer|cookie|credential|session(?:[_-]?id)?|jwt|api[_-]?key)(?:[=:]|[ \t]+))(?:bearer[ \t]*)?[^ \t]+`)
var commandURLWithQuery = regexp.MustCompile(`(?i)https?://[^\s]+`)

func redactCommandLine(value string) string {
	value = sanitizeText(value)
	value = commandURLWithQuery.ReplaceAllStringFunc(value, redactCommandURLQuery)
	value = sensitiveText.ReplaceAllString(value, "$1[REDACTED]")
	if len(value) > 16*1024 {
		return value[:16*1024] + "…[truncated]"
	}
	return value
}

func redactCommandURLQuery(raw string) string {
	raw = redactCommandURLUserInfo(raw)
	fragment := ""
	bodyEnd := len(raw)
	if hash := strings.IndexByte(raw, '#'); hash >= 0 {
		bodyEnd = hash
		fragment = "#[REDACTED]"
	}
	body := raw[:bodyEnd]
	question := strings.IndexByte(body, '?')
	if question < 0 {
		return body + fragment
	}
	return body[:question+1] + redactCommandQueryValues(body[question+1:]) + fragment
}

func redactCommandURLUserInfo(raw string) string {
	scheme := strings.Index(strings.ToLower(raw), "://")
	if scheme < 0 {
		return raw
	}
	start := scheme + 3
	end := len(raw)
	for _, separator := range []byte{'/', '?', '#'} {
		if index := strings.IndexByte(raw[start:], separator); index >= 0 && start+index < end {
			end = start + index
		}
	}
	authority := raw[start:end]
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return raw
	}
	return raw[:start] + "[REDACTED]@" + raw[start+at+1:]
}

func redactCommandQueryValues(query string) string {
	if query == "" {
		return query
	}
	var result strings.Builder
	result.Grow(len(query) + len("[REDACTED]"))
	start := 0
	for index := 0; index <= len(query); index++ {
		if index != len(query) && query[index] != '&' && query[index] != ';' {
			continue
		}
		part := query[start:index]
		if equal := strings.IndexByte(part, '='); equal >= 0 {
			result.WriteString(part[:equal])
			result.WriteString("=[REDACTED]")
		} else if part != "" {
			result.WriteString("[REDACTED]")
		}
		if index < len(query) {
			result.WriteByte(query[index])
		}
		start = index + 1
	}
	return result.String()
}

func ParseLsof(text string, now time.Time) ([]model.Listener, error) {
	type record struct {
		pid     int
		process string
		targets []model.Target
	}
	var records []record
	var current *record
	flush := func() {
		if current != nil {
			records = append(records, *current)
		}
	}
	build := func() []model.Listener {
		var listeners []model.Listener
		for _, record := range records {
			for _, target := range record.targets {
				process := record.process
				name := process
				metadata := model.MetadataComplete
				if record.pid <= 0 || process == "" {
					metadata = model.MetadataPartial
				}
				listeners = append(listeners, model.Listener{ID: model.StableID(target.Key(), strconv.Itoa(record.pid), process), Target: target, Name: name, PID: record.pid, Process: process, Scope: model.ScopeForAddress(target.Address), Metadata: metadata, FirstSeen: now, LastSeen: now})
			}
		}
		return deduplicate(listeners)
	}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		code, value := line[:1], line[1:]
		switch code {
		case "p":
			flush()
			pid, err := strconv.Atoi(value)
			if err != nil {
				flush()
				return build(), fmt.Errorf("invalid lsof pid %q", value)
			}
			current = &record{pid: pid}
		case "c":
			if current != nil {
				current.process = sanitizeText(value)
			}
		case "n":
			if current == nil {
				return build(), fmt.Errorf("lsof endpoint appeared before process record")
			}
			target, err := parseEndpoint(value)
			if err != nil {
				flush()
				return build(), fmt.Errorf("invalid lsof endpoint %q: %w", value, err)
			}
			current.targets = append(current.targets, target)
		case "P", "T":
			// Protocol/state fields are validated by the lsof selector; TCP is
			// the only discovery protocol in this MVP.
		}
	}
	flush()
	return build(), nil
}

func ParseSS(text string, now time.Time) ([]model.Listener, error) {
	var listeners []model.Listener
	for lineNo, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			if strings.TrimSpace(line) != "" {
				return deduplicate(listeners), fmt.Errorf("invalid ss output on line %d", lineNo+1)
			}
			continue
		}
		// `ss -Hlnpt` output is: LISTEN 0 128 local-peer ... users:(...).
		// Keep accepting the older protocol-prefixed fixture shape as well.
		localIndex := 3
		switch strings.ToLower(fields[0]) {
		case "listen":
			// current iproute2 output
		case "tcp":
			if len(fields) < 5 || strings.ToLower(fields[1]) != "listen" {
				return deduplicate(listeners), fmt.Errorf("unsupported protocol/state on line %d", lineNo+1)
			}
			localIndex = 4
		default:
			return deduplicate(listeners), fmt.Errorf("unsupported protocol/state %q on line %d", fields[0], lineNo+1)
		}
		local := fields[localIndex]
		target, err := parseEndpoint(local)
		if err != nil {
			return deduplicate(listeners), fmt.Errorf("invalid ss endpoint %q on line %d: %w", local, lineNo+1, err)
		}
		pid, process := parseSSProcess(strings.Join(fields[5:], " "))
		listeners = append(listeners, model.Listener{ID: model.StableID(target.Key(), strconv.Itoa(pid), process), Target: target, Name: process, PID: pid, Process: process, Scope: model.ScopeForAddress(target.Address), Metadata: metadata(pid, process), FirstSeen: now, LastSeen: now})
	}
	return deduplicate(listeners), nil
}

func parseSSProcess(value string) (int, string) {
	// users:(("name",pid=123,fd=7))
	start := strings.Index(value, "pid=")
	if start < 0 {
		return 0, ""
	}
	start += len("pid=")
	end := start
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	pid, _ := strconv.Atoi(value[start:end])
	name := ""
	prefix := value[:start]
	if closeQuote := strings.LastIndex(prefix, "\""); closeQuote >= 0 {
		if openQuote := strings.LastIndex(prefix[:closeQuote], "\""); openQuote >= 0 {
			name = prefix[openQuote+1 : closeQuote]
		}
	}
	return pid, sanitizeText(name)
}

func sanitizeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}

func parseEndpoint(value string) (model.Target, error) {
	value = strings.TrimSpace(value)
	if arrow := strings.Index(value, "->"); arrow >= 0 {
		value = value[:arrow]
	}
	if strings.HasPrefix(value, "[") {
		end := strings.LastIndex(value, "]:")
		if end < 0 {
			return model.Target{}, fmt.Errorf("invalid endpoint %q", value)
		}
		address, port := value[1:end], value[end+2:]
		p, err := strconv.Atoi(port)
		if err != nil {
			return model.Target{}, fmt.Errorf("invalid endpoint port")
		}
		t := model.Target{Address: address, Port: p, Protocol: "tcp"}.Normalized()
		if err := t.Validate(); err != nil {
			return model.Target{}, err
		}
		return t, nil
	}
	colon := strings.LastIndexByte(value, ':')
	if colon < 1 {
		return model.Target{}, fmt.Errorf("invalid endpoint %q", value)
	}
	address := value[:colon]
	port, err := strconv.Atoi(value[colon+1:])
	if err != nil {
		return model.Target{}, fmt.Errorf("invalid endpoint port")
	}
	t := model.Target{Address: address, Port: port, Protocol: "tcp"}.Normalized()
	if err := t.Validate(); err != nil {
		return model.Target{}, err
	}
	return t, nil
}

func metadata(pid int, process string) model.MetadataQuality {
	if pid > 0 && process != "" {
		return model.MetadataComplete
	}
	return model.MetadataPartial
}

func deduplicate(listeners []model.Listener) []model.Listener {
	seen := map[string]int{}
	result := make([]model.Listener, 0, len(listeners))
	for _, listener := range listeners {
		key := listener.Target.Key() + ":" + strconv.Itoa(listener.PID) + ":" + listener.Process
		if index, ok := seen[key]; ok {
			// Preserve duplicates from separate socket records by assigning a
			// deterministic ID, but avoid duplicate fallback rows from ss -4/-6.
			if listener.Target.Key() == result[index].Target.Key() {
				continue
			}
		}
		seen[key] = len(result)
		result = append(result, listener)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Target.Port != result[j].Target.Port {
			return result[i].Target.Port < result[j].Target.Port
		}
		return result[i].Target.String() < result[j].Target.String()
	})
	return result
}

func isMissingCommand(err error) bool {
	var execErr *exec.Error
	return errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound)
}

func classifyCommandError(source, command string, result runner.Result, cause error) *model.AppError {
	code := model.ErrDependency
	state := "unavailable"
	message := command + " is unavailable"
	if cause != nil {
		message = command + " failed"
	}
	timedOut := cause != nil && errors.Is(cause, context.DeadlineExceeded)
	cancelled := cause != nil && errors.Is(cause, context.Canceled)
	if timedOut {
		code = model.ErrTimeout
		state = "unknown"
	} else if cancelled {
		code = model.ErrCancelled
		state = "unknown"
	} else if (cause != nil && strings.Contains(strings.ToLower(cause.Error()), "permission")) || strings.Contains(strings.ToLower(result.Stderr), "permission") || strings.Contains(strings.ToLower(result.Stderr), "not permitted") {
		code = model.ErrPermission
		state = "permission_denied"
	}
	if result.Truncated && !timedOut && !cancelled {
		code = model.ErrUnknown
		state = "partial"
		message = command + " output was truncated"
	}
	if result.Stderr != "" {
		message += ": " + redactCommandLine(strings.TrimSpace(result.Stderr))
	}
	return model.WrapError(code, source, message, true, state, "Check the dependency and retry; no exposure changes were made.", cause)
}

func ptr[T any](value T) *T { return &value }
