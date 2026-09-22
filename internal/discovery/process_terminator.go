package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

const processTerminationWait = 2 * time.Second

// OSProcessTerminator owns the destructive process operation. It observes the
// current listener state through the narrow ListenerObserver seam, revalidates
// identity, sends exactly one SIGTERM, and never escalates to SIGKILL.
type OSProcessTerminator struct {
	Observer ListenerObserver
	OS       string
}

func NewProcessTerminator(observer ListenerObserver) *OSProcessTerminator {
	return &OSProcessTerminator{Observer: observer, OS: runtime.GOOS}
}

// Terminate sends SIGTERM to the exact process owning a selected listener. It
// re-discovers the listener immediately before signalling and never escalates
// to SIGKILL.
func (t *OSProcessTerminator) Terminate(ctx context.Context, requested model.Listener) error {
	if err := ctx.Err(); err != nil {
		return model.WrapError(model.ErrCancelled, "discovery", "process termination was cancelled", true, "cancelled", "Retry after reviewing the selected process.", err)
	}
	if t == nil || t.Observer == nil {
		return model.NewError(model.ErrDependency, "discovery", "process termination is unavailable", true, "unavailable", "Refresh listener discovery and retry.")
	}
	if t.OS != "darwin" && t.OS != "linux" {
		return model.NewError(model.ErrUnsupported, "discovery", "process termination is unsupported on "+t.OS, false, "unsupported", "Use a supported macOS or Linux build.")
	}
	if requested.PID <= 1 || requested.PID == os.Getpid() {
		return model.NewError(model.ErrUnsafe, "discovery", "refusing to terminate this or a protected process", false, "unsafe", "Select an application listener with a different PID.")
	}
	if strings.TrimSpace(requested.Process) == "" || strings.TrimSpace(requested.ProcessStart) == "" {
		return model.NewError(model.ErrUnknown, "discovery", "selected process identity is incomplete", true, "unknown", "Refresh until the process name and start identity are available before terminating it.")
	}
	current, err := t.Observer.List(ctx)
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
	if currentStart := processStartIdentity(ctx, t.OS, requested.PID); currentStart == "" || currentStart != requested.ProcessStart {
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
		exists, existsErr := processExistsIdentity(ctx, t.OS, requested.PID, requested.ProcessStart)
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

// processStartIdentity is deliberately kept with the termination adapter: it
// is the OS-specific identity check that prevents PID reuse from becoming a
// destructive-process authorization.
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
