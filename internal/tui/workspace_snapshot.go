package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
)

// workspaceSnapshot is the presentation seam between reconciled observations
// and the list, details, and action flows. It applies the same filtering,
// ordering, sectioning, and port identity rules for every consumer.
type workspaceSnapshot struct {
	view  exposure.View
	query string
	cfg   config.Config
}

func newWorkspaceSnapshot(view exposure.View, query string, cfg config.Config) workspaceSnapshot {
	return workspaceSnapshot{view: view, query: query, cfg: cfg}
}

func itemTarget(item exposure.ReconciledItem) (target.Target, bool) {
	if item.Listener != nil {
		return item.Listener.Target, true
	}
	if len(item.Routes) > 0 {
		return item.Routes[0].Target, true
	}
	return target.Target{}, false
}

func itemSection(item exposure.ReconciledItem) string {
	if item.Listener != nil {
		return "LOCAL LISTENERS"
	}
	if item.State == exposuredata.ExposureInactive {
		return "INACTIVE CONFIGURED ROUTES"
	}
	return "UNKNOWN / UNAVAILABLE"
}

func (s workspaceSnapshot) Items() []exposure.ReconciledItem {
	items := deduplicatePortItems(sectioned(ordered(displayed(s.view, "", s.cfg), s.cfg.Sort)))
	return visible(exposure.View{Items: items}, s.query)
}

func sectioned(items []exposure.ReconciledItem) []exposure.ReconciledItem {
	groups := map[string][]exposure.ReconciledItem{}
	for _, item := range items {
		section := itemSection(item)
		groups[section] = append(groups[section], item)
	}
	result := make([]exposure.ReconciledItem, 0, len(items))
	for _, section := range []string{"LOCAL LISTENERS", "INACTIVE CONFIGURED ROUTES", "UNKNOWN / UNAVAILABLE"} {
		result = append(result, groups[section]...)
	}
	return result
}

func deduplicatePortItems(items []exposure.ReconciledItem) []exposure.ReconciledItem {
	result := make([]exposure.ReconciledItem, 0, len(items))
	byPort := map[int]int{}
	for _, item := range items {
		port, ok := itemPort(item)
		if !ok {
			result = append(result, snapshotItemCopy(item))
			continue
		}
		index, exists := byPort[port]
		if !exists {
			byPort[port] = len(result)
			result = append(result, snapshotItemCopy(item))
			continue
		}
		mergePortItem(&result[index], item)
	}
	return result
}

func snapshotItemCopy(item exposure.ReconciledItem) exposure.ReconciledItem {
	item.Routes = append([]exposuredata.ExposureRoute(nil), item.Routes...)
	return item
}

func itemPort(item exposure.ReconciledItem) (int, bool) {
	target, ok := itemTarget(item)
	if !ok || target.Port < 1 || target.Port > 65535 {
		return 0, false
	}
	return target.Port, true
}

func mergePortItem(primary *exposure.ReconciledItem, duplicate exposure.ReconciledItem) {
	primaryHadRoutes := len(primary.Routes) > 0
	for _, route := range duplicate.Routes {
		duplicateRoute := false
		for _, existing := range primary.Routes {
			if route.ID != "" && route.ID == existing.ID || route.ProviderKey != "" && route.ProviderKey == existing.ProviderKey {
				duplicateRoute = true
				break
			}
		}
		if !duplicateRoute {
			primary.Routes = append(primary.Routes, route)
		}
	}
	if !primaryHadRoutes && len(duplicate.Routes) > 0 {
		primary.Mode = duplicate.Mode
		primary.State = duplicate.State
	}
	if primary.Recommendation == "" {
		primary.Recommendation = duplicate.Recommendation
	}
}

func ordered(items []exposure.ReconciledItem, key string) []exposure.ReconciledItem {
	result := append([]exposure.ReconciledItem(nil), items...)
	field := func(item exposure.ReconciledItem) string {
		target, _ := itemTarget(item)
		name := item.ID
		if item.Listener != nil {
			name = valueOr(item.Listener.Name, item.ID)
		}
		address, port := target.Normalized().Address, target.Port
		exposureName := string(item.Mode)
		if len(item.Routes) > 0 {
			exposureName = string(item.Routes[0].Mode)
		}
		switch key {
		case "name":
			return fmt.Sprintf("%s\x00%s\x00%06d\x00%s", name, address, port, item.ID)
		case "address":
			return fmt.Sprintf("%s\x00%06d\x00%s", address, port, item.ID)
		case "exposure":
			return fmt.Sprintf("%s\x00%06d\x00%s\x00%s", exposureName, port, address, item.ID)
		default:
			return fmt.Sprintf("%06d\x00%s\x00%s", port, address, item.ID)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return field(result[i]) < field(result[j]) })
	return result
}

func displayed(view exposure.View, filter string, cfg config.Config) []exposure.ReconciledItem {
	items := visible(view, filter)
	result := make([]exposure.ReconciledItem, 0, len(items))
	for _, item := range items {
		if !cfg.ShowInactiveConfiguredPorts && item.Listener == nil && item.State == exposuredata.ExposureInactive {
			continue
		}
		if !cfg.ShowSystemListeners && item.Listener != nil && len(item.Routes) == 0 && knownSystemProcess(item.Listener.Process) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func knownSystemProcess(process string) bool {
	switch strings.ToLower(process) {
	case "launchd", "systemd", "systemd-resolved", "kernel_task":
		return true
	default:
		return false
	}
}

func visible(view exposure.View, filter string) []exposure.ReconciledItem {
	if filter == "" {
		return view.Items
	}
	needle := strings.ToLower(filter)
	items := make([]exposure.ReconciledItem, 0, len(view.Items))
	for _, item := range view.Items {
		text := item.ID + " " + string(item.State) + " " + string(item.Mode) + " " + item.Warning + " " + item.Recommendation
		if item.Listener != nil {
			text += " " + item.Listener.Name + " " + item.Listener.Process + " " + item.Listener.Target.String() + " " + strconv.Itoa(item.Listener.Target.Port)
		}
		for _, route := range item.Routes {
			text += " " + route.ID + " " + route.ProviderKey + " " + string(route.Mode) + " " + string(route.State) + " " + route.Target.String() + " " + strconv.Itoa(route.Target.Port) + " " + string(route.Ownership) + " " + route.URL
		}
		if strings.Contains(strings.ToLower(text), needle) {
			items = append(items, item)
		}
	}
	return items
}
