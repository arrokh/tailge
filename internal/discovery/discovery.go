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
	"time"

	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/target"
)

type ListenerObserver interface {
	List(context.Context) (ListenerSnapshot, error)
}

// Discoverer is retained as a compatibility alias for listener observation.
// New callers should depend on ListenerObserver or ProcessTerminator separately.
type Discoverer = ListenerObserver

type ProcessTerminator interface {
	Terminate(context.Context, Listener) error
}

type OSListenerObserver struct {
	Runner runner.Runner
	OS     string
	Now    func() time.Time
}

// OSDiscoverer is retained as a compatibility name for the listener observer.
// It also exposes Terminate as a forwarding method for existing callers; new
// consumers should receive an explicit ProcessTerminator seam instead.
type OSDiscoverer = OSListenerObserver

func New(r runner.Runner) *OSListenerObserver {
	return &OSListenerObserver{Runner: r, OS: runtime.GOOS, Now: time.Now}
}

func (d *OSListenerObserver) List(ctx context.Context) (ListenerSnapshot, error) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	snapshot := ListenerSnapshot{At: now, Source: "lsof", Authoritative: false, Listeners: []Listener{}}
	if d.Runner == nil {
		err := fault.NewError(fault.ErrDependency, "discovery", "no command runner is configured", true, "unavailable", "Retry the scan.")
		snapshot.Error = ptr(err.Safe())
		return snapshot, err
	}
	if d.OS != "darwin" && d.OS != "linux" {
		err := fault.NewError(fault.ErrUnsupported, "discovery", "local listener discovery is unsupported on "+d.OS, false, "unsupported", "Use a supported macOS or Linux build.")
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
			var listeners []Listener
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
				snapshot.Error = ptr(fault.NewError(fault.ErrUnknown, "discovery", parseErr.Error(), true, "partial", "Refresh after checking the listener provider.").Safe())
				d.enrich(ctx, &snapshot)
				return snapshot, fault.NewError(fault.ErrUnknown, "discovery", "listener output could not be parsed", true, "partial", "Retry the scan; no exposure changes were made.")
			}
			if result.Truncated {
				snapshot.Listeners = append(snapshot.Listeners, listeners...)
				snapshot.Listeners = deduplicate(snapshot.Listeners)
				d.enrich(ctx, &snapshot)
				err := fault.NewError(fault.ErrUnknown, "discovery", "listener output exceeded the capture limit", true, "partial", "Reduce the number of listeners or retry the scan.")
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

// Terminate is retained for compatibility with older callers. New callers
// should receive an explicit ProcessTerminator instead of asserting this
// capability on the listener observer.
func (d *OSListenerObserver) Terminate(ctx context.Context, requested Listener) error {
	if d == nil {
		return fault.NewError(fault.ErrDependency, "discovery", "process termination is unavailable", true, "unavailable", "Refresh listener discovery and retry.")
	}
	return NewProcessTerminator(d).Terminate(ctx, requested)
}

func (d *OSListenerObserver) enrich(ctx context.Context, snapshot *ListenerSnapshot) {
	for i := range snapshot.Listeners {
		l := &snapshot.Listeners[i]
		if l.PID == 0 {
			l.Metadata = MetadataPartial
			continue
		}
		if command := processCommand(ctx, d.OS, l.PID); command != "" {
			l.CommandLine = redactCommandLine(command)
			if strings.Contains(l.CommandLine, "[truncated]") {
				l.Metadata = MetadataPartial
			}
		}
		if start := processStartIdentity(ctx, d.OS, l.PID); start != "" {
			l.ProcessStart = start
		} else {
			l.Metadata = MetadataPartial
		}
		if l.Process == "" || l.Name == "" {
			l.Metadata = MetadataPartial
		}
		if l.CommandLine == "" {
			// Process command line is optional and may be hidden by OS policy.
			if l.Metadata == MetadataComplete {
				l.Metadata = MetadataPartial
			}
		}
	}
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

func ParseLsof(text string, now time.Time) ([]Listener, error) {
	type record struct {
		pid     int
		process string
		targets []target.Target
	}
	var records []record
	var current *record
	flush := func() {
		if current != nil {
			records = append(records, *current)
		}
	}
	build := func() []Listener {
		var listeners []Listener
		for _, record := range records {
			for _, observedTarget := range record.targets {
				process := record.process
				name := process
				metadata := MetadataComplete
				if record.pid <= 0 || process == "" {
					metadata = MetadataPartial
				}
				listeners = append(listeners, Listener{ID: target.StableID(observedTarget.Key(), strconv.Itoa(record.pid), process), Target: observedTarget, Name: name, PID: record.pid, Process: process, Scope: target.ScopeForAddress(observedTarget.Address), Metadata: metadata, FirstSeen: now, LastSeen: now})
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

func ParseSS(text string, now time.Time) ([]Listener, error) {
	var listeners []Listener
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
		observedTarget, err := parseEndpoint(local)
		if err != nil {
			return deduplicate(listeners), fmt.Errorf("invalid ss endpoint %q on line %d: %w", local, lineNo+1, err)
		}
		pid, process := parseSSProcess(strings.Join(fields[5:], " "))
		listeners = append(listeners, Listener{ID: target.StableID(observedTarget.Key(), strconv.Itoa(pid), process), Target: observedTarget, Name: process, PID: pid, Process: process, Scope: target.ScopeForAddress(observedTarget.Address), Metadata: metadata(pid, process), FirstSeen: now, LastSeen: now})
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

func parseEndpoint(value string) (target.Target, error) {
	value = strings.TrimSpace(value)
	if arrow := strings.Index(value, "->"); arrow >= 0 {
		value = value[:arrow]
	}
	if strings.HasPrefix(value, "[") {
		end := strings.LastIndex(value, "]:")
		if end < 0 {
			return target.Target{}, fmt.Errorf("invalid endpoint %q", value)
		}
		address, port := value[1:end], value[end+2:]
		p, err := strconv.Atoi(port)
		if err != nil {
			return target.Target{}, fmt.Errorf("invalid endpoint port")
		}
		t := target.Target{Address: address, Port: p, Protocol: "tcp"}.Normalized()
		if err := t.Validate(); err != nil {
			return target.Target{}, err
		}
		return t, nil
	}
	colon := strings.LastIndexByte(value, ':')
	if colon < 1 {
		return target.Target{}, fmt.Errorf("invalid endpoint %q", value)
	}
	address := value[:colon]
	port, err := strconv.Atoi(value[colon+1:])
	if err != nil {
		return target.Target{}, fmt.Errorf("invalid endpoint port")
	}
	t := target.Target{Address: address, Port: port, Protocol: "tcp"}.Normalized()
	if err := t.Validate(); err != nil {
		return target.Target{}, err
	}
	return t, nil
}

func metadata(pid int, process string) MetadataQuality {
	if pid > 0 && process != "" {
		return MetadataComplete
	}
	return MetadataPartial
}

func deduplicate(listeners []Listener) []Listener {
	seen := map[string]int{}
	result := make([]Listener, 0, len(listeners))
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

func classifyCommandError(source, command string, result runner.Result, cause error) *fault.AppError {
	code := fault.ErrDependency
	state := "unavailable"
	message := command + " is unavailable"
	if cause != nil {
		message = command + " failed"
	}
	timedOut := cause != nil && errors.Is(cause, context.DeadlineExceeded)
	cancelled := cause != nil && errors.Is(cause, context.Canceled)
	if timedOut {
		code = fault.ErrTimeout
		state = "unknown"
	} else if cancelled {
		code = fault.ErrCancelled
		state = "unknown"
	} else if (cause != nil && strings.Contains(strings.ToLower(cause.Error()), "permission")) || strings.Contains(strings.ToLower(result.Stderr), "permission") || strings.Contains(strings.ToLower(result.Stderr), "not permitted") {
		code = fault.ErrPermission
		state = "permission_denied"
	}
	if result.Truncated && !timedOut && !cancelled {
		code = fault.ErrUnknown
		state = "partial"
		message = command + " output was truncated"
	}
	if result.Stderr != "" {
		message += ": " + redactCommandLine(strings.TrimSpace(result.Stderr))
	}
	return fault.WrapError(code, source, message, true, state, "Check the dependency and retry; no exposure changes were made.", cause)
}

func ptr[T any](value T) *T { return &value }
