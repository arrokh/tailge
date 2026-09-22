// Package workspace owns framework-independent Service workspace decisions.
// It returns typed effects and policy results; Bubble Tea, provider adapters,
// browsers, clipboards, and process handles remain in internal/tui.
package workspace

// EffectKind identifies an effect requested by workspace input. The TUI
// adapter decides how to execute each effect.
type EffectKind uint8

const (
	EffectNone EffectKind = iota
	EffectRefresh
	EffectRetry
	EffectOpenObservedURL
	EffectOpenLocalURL
	EffectCopyURL
	EffectApplyExposure
	EffectCancelOperation
	EffectTerminateProcess
	EffectStartProcessTermination
	EffectConfirmCancellation
	EffectConfirmQuit
	EffectQuit
)

// Effect is a typed request from workspace input to its adapter.
type Effect struct {
	Kind EffectKind
}

// EffectForKey maps global workspace shortcuts to typed adapter requests.
func EffectForKey(key string) (Effect, bool) {
	var kind EffectKind
	switch key {
	case "r":
		kind = EffectRefresh
	case "R":
		kind = EffectRetry
	case "o":
		kind = EffectOpenObservedURL
	case "O":
		kind = EffectOpenLocalURL
	case "y":
		kind = EffectCopyURL
	case "c":
		kind = EffectCancelOperation
	case "x":
		kind = EffectTerminateProcess
	case "q", "ctrl+c":
		kind = EffectQuit
	default:
		return Effect{}, false
	}
	return Effect{Kind: kind}, true
}
