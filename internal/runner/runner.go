package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const DefaultMaxOutput = 256 * 1024

type Result struct {
	Command   string
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
	Truncated bool
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

type ExecRunner struct {
	MaxOutput int
}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	max := r.MaxOutput
	if max <= 0 || max > DefaultMaxOutput {
		max = DefaultMaxOutput
	}
	started := time.Now()
	result := Result{Command: formatCommand(name, args...)}
	cmd := exec.CommandContext(ctx, name, args...)
	// A cancelled command can leave descendants holding stdout/stderr open.
	// WaitDelay bounds pipe draining after the direct child is terminated.
	cmd.WaitDelay = 2 * time.Second
	// Intentionally leave Stdin nil. Provider commands must never inherit a TTY
	// and block waiting for a login, password, or confirmation.
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = max, max
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result.Duration = time.Since(started)
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.Truncated = stdout.truncated || stderr.truncated
	if err == nil {
		result.ExitCode = 0
		return result, nil
	}
	result.ExitCode = exitCode(err)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	return result, err
}

func formatCommand(name string, args ...string) string {
	parts := append([]string{name}, args...)
	for i, p := range parts {
		parts[i] = quote(p)
	}
	return strings.Join(parts, " ")
}

func quote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\'' || r == '"'
	}) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type limitedBuffer struct {
	data      []byte
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
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

func (b *limitedBuffer) String() string { return string(b.data) }

// FuncRunner is useful for adapter tests and deterministic failure injection.
type FuncRunner func(context.Context, string, ...string) (Result, error)

func (f FuncRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if f == nil {
		return Result{}, fmt.Errorf("function runner is nil")
	}
	return f(ctx, name, args...)
}

// RecordingRunner wraps a Runner and records calls for deterministic tests. It
// does not change command execution or inherit a terminal.
type RecordingRunner struct {
	Inner Runner
	Mu    sync.Mutex
	Calls []Result
}

func (r *RecordingRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if r.Inner == nil {
		return Result{}, fmt.Errorf("recording runner has no inner runner")
	}
	result, err := r.Inner.Run(ctx, name, args...)
	r.Mu.Lock()
	r.Calls = append(r.Calls, result)
	r.Mu.Unlock()
	return result, err
}
