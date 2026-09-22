package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/workspace"
)

// Process selection and identity revalidation stay in the TUI adapter because
// they consume the current process-control state. Fingerprinting itself is
// owned by the framework-independent workspace policy module.

func (m *workspaceModel) collectProcessTargets() ([]processTarget, string) {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil {
		return nil, "listener state is stale"
	}
	items := m.actionItems()
	seenProcesses := map[string]bool{}
	targets := make([]processTarget, 0, len(items))
	for _, item := range items {
		if item.Listener == nil {
			if len(item.Routes) > 0 {
				return nil, item.ID + ": exposure route is inactive; no current local process exists (use d to disable the route)"
			}
			return nil, item.ID + ": no current local listener exists"
		}
		listener := *item.Listener
		if _, busy := m.activeOps[listener.Target.Key()]; busy {
			return nil, "an exposure operation is Applying for a selected listener"
		}
		if listener.PID <= 1 || listener.PID == os.Getpid() {
			return nil, "a selected process is protected"
		}
		if strings.TrimSpace(listener.Process) == "" || strings.TrimSpace(listener.ProcessStart) == "" {
			return nil, "a selected process has incomplete identity"
		}
		group := workspace.ProcessGroupFingerprint(listener)
		if seenProcesses[group] {
			continue
		}
		seenProcesses[group] = true
		targets = append(targets, processTarget{itemID: item.ID, listener: listener, fingerprint: workspace.ProcessFingerprint(listener)})
	}
	return targets, ""
}

func (m *workspaceModel) revalidateProcessTarget(target processTarget) (discovery.Listener, error) {
	if !m.view.Listeners.Authoritative || m.view.Listeners.Stale || m.view.Listeners.Error != nil {
		return discovery.Listener{}, fmt.Errorf("listener state is stale")
	}
	for _, item := range m.items() {
		if item.ID != target.itemID || item.Listener == nil {
			continue
		}
		listener := *item.Listener
		if workspace.ProcessFingerprint(listener) != target.fingerprint {
			return discovery.Listener{}, fmt.Errorf("listener or process identity changed")
		}
		if _, busy := m.activeOps[listener.Target.Key()]; busy {
			return discovery.Listener{}, fmt.Errorf("an exposure operation is Applying")
		}
		if listener.PID <= 1 || listener.PID == os.Getpid() {
			return discovery.Listener{}, fmt.Errorf("process is protected")
		}
		if strings.TrimSpace(listener.Process) == "" || strings.TrimSpace(listener.ProcessStart) == "" {
			return discovery.Listener{}, fmt.Errorf("process identity is incomplete")
		}
		return listener, nil
	}
	return discovery.Listener{}, fmt.Errorf("selected listener is no longer available")
}
