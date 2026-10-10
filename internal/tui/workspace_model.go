package tui

import (
	"context"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/tailscale"
	textinput "github.com/charmbracelet/bubbles/textinput"
)

// workspaceModel is the Bubble Tea adapter around the deterministic workspace
// state. External adapters live here; domain transitions remain on the focused
// workspace modules.
type workspaceModel struct {
	workspaceState

	ctx    context.Context
	cancel context.CancelFunc

	provider          *tailscale.Adapter
	clipboard         Clipboard
	controller        *exposure.Controller
	processTerminator discovery.ProcessTerminator
	manager           config.Manager

	searchInput  textinput.Model
	paletteInput textinput.Model
}

func newInput(prompt string) textinput.Model {
	input := textinput.New()
	input.Prompt = prompt
	input.CharLimit = 256
	input.Width = 32
	return input
}

func newWorkspaceModel(discoverer discovery.ListenerObserver, processTerminator discovery.ProcessTerminator, provider *tailscale.Adapter, manager config.Manager) *workspaceModel {
	ctx, cancel := context.WithCancel(context.Background())
	search := newInput("/ ")
	palette := newInput(": ")
	return &workspaceModel{
		workspaceState: newWorkspaceState(),
		ctx:            ctx,
		cancel:         cancel,
		provider:       provider,
		controller: func() *exposure.Controller {
			controller := exposure.NewController(discoverer, provider)
			controller.MutationLockPath = manager.Path + ".exposure.lock"
			return controller
		}(),
		processTerminator: processTerminator,
		manager:           manager,
		searchInput:       search,
		paletteInput:      palette,
	}
}
