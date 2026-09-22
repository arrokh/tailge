package tui

import (
	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
)

// Bubble Tea messages exchanged by the workspace's asynchronous flows.
type configLoadedMsg struct {
	cfg      config.Config
	warnings []string
	err      error
}

type viewLoadedMsg struct {
	seq  uint64
	view exposure.View
	err  error
}

type readinessLoadedMsg struct {
	seq  uint64
	data readiness.Readiness
	err  error
}

type operationDoneMsg struct {
	targetKey  string
	receipt    exposuredata.OperationReceipt
	err        error
	batchID    string
	batchIndex int
	batchTotal int
}

type quitAfterCancelMsg struct{ generation uint64 }
type gTimeoutMsg struct{ generation uint64 }
type refreshTickMsg struct{}

type configSavedMsg struct {
	cfg config.Config
	err error
}

type configValidatedMsg struct {
	cfg config.Config
	err error
}
