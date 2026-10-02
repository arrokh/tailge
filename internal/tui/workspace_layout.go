package tui

// Workspace layout thresholds are kept with the rendering domain rather than
// the Bubble Tea event loop.
const (
	splitMinWidth    = 100
	splitMinHeight   = 24
	minListPaneWidth = 53
	minCompactRows   = 8
	minModalRows     = 12
)

func splitViewAvailable(width, height int) bool {
	return width >= splitMinWidth && height >= splitMinHeight
}
