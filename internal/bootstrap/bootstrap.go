// Package bootstrap assembles Tailge's concrete adapters for the CLI and TUI.
package bootstrap

import (
	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/tailscale"
)

// App contains the concrete application adapters shared by the command-line
// and interactive entrypoints. Callers depend on the adapter interfaces where
// possible; the concrete provider remains explicit because the TUI needs its
// provider-specific readiness and URL capabilities.
type App struct {
	Config            config.Manager
	Discoverer        discovery.ListenerObserver
	ProcessTerminator discovery.ProcessTerminator
	Tailscale         *tailscale.Adapter
}

// New builds the default local adapters. A configuration-manager failure is
// retained as an empty manager so read-only discovery can still run and report
// its own diagnostics; commands that need persistence surface that failure.
func New() App {
	runner := runner.ExecRunner{MaxOutput: runner.DefaultMaxOutput}
	discoverer := discovery.New(runner)
	provider := tailscale.New(runner)
	manager, err := config.NewManager("")
	if err != nil {
		manager = config.Manager{}
	}
	return App{
		Config:            manager,
		Discoverer:        discoverer,
		ProcessTerminator: discovery.NewProcessTerminator(discoverer),
		Tailscale:         provider,
	}
}
