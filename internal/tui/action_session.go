package tui

import (
	"sort"
	"strings"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

type exposureActionAvailability struct {
	disabled bool
	reason   string
	wait     bool
}

type exposureActionChoice struct {
	mode     model.ExposureMode
	label    string
	disabled bool
	reason   string
	wait     bool
}

type exposureActionSession struct {
	index    int
	mode     model.ExposureMode
	routeKey string
	itemID   string
	target   model.Target
	choices  []exposureActionChoice
	confirm  bool
	preview  actionPreview
}

type actionPreview struct {
	routeHash     string
	allRoutesHash string
	listenersHash string
	selectionHash string
}

func modeIndex(mode model.ExposureMode) int {
	switch mode {
	case model.ExposureServe:
		return 1
	case model.ExposureFunnel:
		return 2
	default:
		return 0
	}
}

func (s *exposureActionSession) open(itemID string, target model.Target, mode model.ExposureMode) {
	s.itemID = itemID
	s.target = target
	s.mode = mode
	s.routeKey = ""
	s.index = modeIndex(mode)
	s.confirm = false
	s.choices = nil
	s.preview = actionPreview{}
}

func (s *exposureActionSession) refreshChoices(availability func(model.ExposureMode) exposureActionAvailability) {
	s.choices = []exposureActionChoice{
		{mode: model.ExposureDisabled, label: "Disabled"},
		{mode: model.ExposureServe, label: "Serve (tailnet only)"},
		{mode: model.ExposureFunnel, label: "Funnel (public internet)"},
	}
	for index := range s.choices {
		state := availability(s.choices[index].mode)
		s.choices[index].disabled = state.disabled
		s.choices[index].reason = state.reason
		s.choices[index].wait = state.wait
	}
}

func (s *exposureActionSession) selectedChoice() (exposureActionChoice, bool) {
	if s.index < 0 || s.index >= len(s.choices) {
		return exposureActionChoice{}, false
	}
	return s.choices[s.index], true
}

func (s *exposureActionSession) capturePreview(routeHash, allRoutesHash, listenersHash, selectionHash string) {
	s.preview = actionPreview{
		routeHash: routeHash, allRoutesHash: allRoutesHash,
		listenersHash: listenersHash, selectionHash: selectionHash,
	}
}

func (s exposureActionSession) previewChanged(routeHash, allRoutesHash, listenersHash, selectionHash string) bool {
	if s.preview.routeHash == "" && s.preview.allRoutesHash == "" && s.preview.listenersHash == "" && s.preview.selectionHash == "" {
		return false
	}
	return routeHash != s.preview.routeHash || allRoutesHash != s.preview.allRoutesHash || listenersHash != s.preview.listenersHash || (s.preview.selectionHash != "" && selectionHash != s.preview.selectionHash)
}

func selectionFingerprint(items []exposure.ReconciledItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		target, ok := itemTarget(item)
		if !ok {
			parts = append(parts, item.ID+":unavailable")
			continue
		}
		parts = append(parts, item.ID+":"+target.Key()+":"+routeFingerprint(item.Routes, nil))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// routeFingerprint delegates provider-independent route identity to the
// canonical Tailscale identity module. Ownership/state availability is checked
// separately by the workspace safety gate.
func routeFingerprint(routes []model.ExposureRoute, target *model.Target) string {
	matched := make([]model.ExposureRoute, 0, len(routes))
	for _, route := range routes {
		if target != nil && !model.TargetsMatch(route.Target, *target) {
			continue
		}
		matched = append(matched, route)
	}
	return tailscale.RoutesHash(matched)
}

func listenerFingerprint(snapshot model.ListenerSnapshot, target model.Target) string {
	ids := []string{}
	for _, listener := range snapshot.Listeners {
		if exposure.Matches(listener.Target, target) {
			ids = append(ids, listener.ID+":"+listener.Target.Key())
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\x00")
}
