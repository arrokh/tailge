package tui

// paneFocus identifies the workspace pane receiving navigation input.
type paneFocus uint8

const (
	focusList paneFocus = iota
	focusDetails
)

// modalKind identifies the active modal interaction.
type modalKind uint8

const (
	modalNone modalKind = iota
	modalAction
	modalDisableRoute
	modalConfirm
	modalTerminateProcess
	modalCancel
	modalQuit
	modalHelp
	modalPalette
)
