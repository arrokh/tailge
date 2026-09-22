package tailscale

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/runner"
)

func findBinary() string {
	path, err := exec.LookPath("tailscale")
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return ""
	}
	return path
}

func (a *Adapter) binaryPath() string {
	if a.Binary != "" {
		return a.Binary
	}
	return findBinary()
}

func (a *Adapter) run(ctx context.Context, args ...string) (runner.Result, error) {
	if a.Runner == nil {
		return runner.Result{}, model.NewError(model.ErrDependency, "tailscale", "no command runner is configured", true, "unavailable", "Retry the command.")
	}
	name := a.binaryPath()
	if name == "" {
		return runner.Result{ExitCode: -1}, &exec.Error{Name: "tailscale", Err: exec.ErrNotFound}
	}
	return a.Runner.Run(ctx, name, args...)
}

func (a *Adapter) commandError(command string, result runner.Result, cause error) *model.AppError {
	code := model.ErrOperation
	state := "failed"
	retry := true
	message := command + " failed"
	var existing *model.AppError
	if errors.As(cause, &existing) && existing != nil {
		code, state, retry = existing.Code, existing.State, existing.Retryable
	}
	if result.ExitCode == -1 && errors.Is(cause, exec.ErrNotFound) {
		code, state, message = model.ErrDependency, "unavailable", "tailscale executable was not found"
	}
	timedOut := errors.Is(cause, context.DeadlineExceeded)
	cancelled := errors.Is(cause, context.Canceled)
	if timedOut {
		code, state, message = model.ErrTimeout, "unknown", command+" timed out"
	} else if cancelled {
		code, state, message = model.ErrCancelled, "unknown", command+" was cancelled"
	} else if strings.Contains(strings.ToLower(result.Stderr), "permission denied") || strings.Contains(strings.ToLower(result.Stderr), "not permitted") {
		code, state, message = model.ErrPermission, "permission_denied", command+" was denied by Tailscale"
	}
	if result.Stderr != "" {
		message += ": " + redact(strings.TrimSpace(result.Stderr))
	}
	if result.Truncated && !timedOut && !cancelled {
		code, state, message = model.ErrUnknown, "partial", command+" output was truncated"
	}
	return model.WrapError(code, "tailscale", message, retry, state, "Review Tailscale status and retry; final exposure state must be verified.", cause)
}

var urlWithQuery = regexp.MustCompile(`(?i)https?://[^\s]+`)

func redact(value string) string {
	value = sanitizeText(value)
	value = urlWithQuery.ReplaceAllStringFunc(value, redactURLQuery)
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)((?:auth(?:orization)?|token|password|secret|cookie|credential|session(?:[_-]?id)?|jwt|api[_-]?key)(?:=|:|[ \t]+))(?:bearer[ \t]*)?[^\s&]+`),
		regexp.MustCompile(`(?i)(bearer[ \t]*)[^\s&]+`),
	}
	for _, pattern := range patterns {
		value = pattern.ReplaceAllString(value, "$1[REDACTED]")
	}
	if len(value) > 4096 {
		value = value[:4096] + "…[truncated]"
	}
	return value
}

func sanitizeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
}

func redactURLQuery(raw string) string {
	raw = redactURLUserInfo(raw)
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
	return body[:question+1] + redactQueryValues(body[question+1:]) + fragment
}

func redactURLUserInfo(raw string) string {
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

func redactQueryValues(query string) string {
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
