package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
)

// workspaceState is the deterministic decision state for the Service workspace.
// It contains no Bubble Tea messages, terminal handles, provider adapters, or
// OS effects. The Bubble Tea model owns the adapter that translates this state
// into commands and rendering.
type workspaceState struct {
	cfg          config.Config
	configErr    error
	view         exposure.View
	readiness    readiness.Readiness
	viewErr      error
	readyErr     error
	banner       string
	bannerSticky bool
	transient    string

	width  int
	height int
	focus  paneFocus

	query             string
	searching         bool
	previousQ         string
	selectedID        string
	selectedIdx       int
	selectedItems     map[string]bool
	visualSelection   bool
	visualRange       map[string]bool
	selectionAnchorID string
	listScroll        int
	detailOffset      int

	modal                   modalKind
	disableRouteIndex       int
	modalItemID             string
	modalTarget             target.Target
	modalProcess            discovery.Listener
	modalProcessFingerprint string
	actionSession           exposureActionSession
	confirmFocus            bool
	modalChoice             bool
	helpOffset              int
	paletteIndex            int

	refreshState        refreshCoordinator
	hasView             bool
	hasReadiness        bool
	gGeneration         uint64
	activeOps           map[string]context.CancelFunc
	batchID             string
	batchTotal          int
	batchCompleted      int
	batchFailures       int
	batchCancel         context.CancelFunc
	processBusy         bool
	processCancel       context.CancelFunc
	processBatch        []processTarget
	processBatchIndex   int
	processBatchDone    int
	processBatchFailed  int
	processUnverified   bool
	quittingAfterCancel bool
	quitGeneration      uint64
}

func newWorkspaceState() workspaceState {
	now := time.Now()
	return workspaceState{
		cfg:  config.Defaults(),
		view: exposure.View{At: now},
		readiness: readiness.Readiness{
			At:     now,
			Status: readiness.ReadinessUnknown,
			Modes: []readiness.ModeReadiness{
				{Mode: exposuredata.ExposureServe, Status: readiness.ReadinessUnknown},
				{Mode: exposuredata.ExposureFunnel, Status: readiness.ReadinessUnknown},
			},
		},
		focus:         focusList,
		selectedItems: map[string]bool{},
		visualRange:   map[string]bool{},
		activeOps:     map[string]context.CancelFunc{},
		width:         120,
		height:        30,
	}
}

func (s *workspaceState) ensureSelectedItems() {
	if s.selectedItems == nil {
		s.selectedItems = map[string]bool{}
	}
}

func (s *workspaceState) clearAllSelection() {
	count := len(s.selectedItems)
	s.selectedItems = map[string]bool{}
	s.visualSelection = false
	s.visualRange = map[string]bool{}
	s.selectionAnchorID = ""
	if count == 0 {
		s.transient = "No selected services"
		return
	}
	s.transient = fmt.Sprintf("Cleared %d selected services", count)
}

func (s workspaceState) isMarked(id string) bool {
	return s.selectedItems != nil && s.selectedItems[id]
}

func (s *workspaceState) toggleCurrentSelection() {
	if s.visualSelection {
		s.visualSelection = false
		s.selectionAnchorID = ""
		s.visualRange = map[string]bool{}
	}
	item, ok := s.selectedItem()
	if !ok {
		s.banner = "No service is selected"
		s.bannerSticky = true
		return
	}
	s.ensureSelectedItems()
	if s.selectedItems[item.ID] {
		delete(s.selectedItems, item.ID)
		s.transient = "Unselected " + item.ID
	} else {
		s.selectedItems[item.ID] = true
		s.transient = "Selected " + item.ID
	}
}

func (s *workspaceState) toggleVisualSelection() {
	items := s.items()
	if len(items) == 0 {
		s.banner = "No services are visible"
		s.bannerSticky = true
		return
	}
	if s.visualSelection {
		s.visualSelection = false
		s.selectionAnchorID = ""
		s.visualRange = map[string]bool{}
		s.transient = fmt.Sprintf("Visual selection kept (%d services)", s.selectionCount())
		return
	}
	if s.selectedID == "" {
		s.reselect("", 0)
	}
	s.visualSelection = true
	s.selectionAnchorID = s.selectedID
	s.updateVisualSelection()
	s.transient = "Visual selection: move to extend, V to keep"
}

func (s *workspaceState) updateVisualSelection() {
	if !s.visualSelection {
		return
	}
	items := s.items()
	anchor, current := -1, -1
	for index, item := range items {
		if item.ID == s.selectionAnchorID {
			anchor = index
		}
		if item.ID == s.selectedID {
			current = index
		}
	}
	if anchor < 0 || current < 0 {
		s.visualSelection = false
		s.selectionAnchorID = ""
		s.visualRange = map[string]bool{}
		return
	}
	if anchor > current {
		anchor, current = current, anchor
	}
	s.ensureSelectedItems()
	s.visualRange = map[string]bool{}
	for _, item := range items[anchor : current+1] {
		s.selectedItems[item.ID] = true
		s.visualRange[item.ID] = true
	}
}

func (s workspaceState) selectionCount() int {
	count := 0
	for _, item := range s.items() {
		if s.isMarked(item.ID) {
			count++
		}
	}
	return count
}

func (s *workspaceState) pruneSelection() {
	if len(s.selectedItems) == 0 {
		return
	}
	valid := map[string]bool{}
	for _, item := range s.items() {
		valid[item.ID] = true
	}
	for id := range s.selectedItems {
		if !valid[id] {
			delete(s.selectedItems, id)
		}
	}
	for id := range s.visualRange {
		if !valid[id] {
			delete(s.visualRange, id)
		}
	}
	if s.visualSelection && !valid[s.selectionAnchorID] {
		s.visualSelection = false
		s.selectionAnchorID = ""
		s.visualRange = map[string]bool{}
	}
}

func (s *workspaceState) actionItems() []exposure.ReconciledItem {
	items := s.items()
	selected := make([]exposure.ReconciledItem, 0, len(items))
	for _, item := range items {
		if s.isMarked(item.ID) {
			selected = append(selected, item)
		}
	}
	if len(selected) > 0 {
		return selected
	}
	if item, ok := s.selectedItem(); ok {
		return []exposure.ReconciledItem{item}
	}
	return nil
}

func (s *workspaceState) actionAnchorItem() (exposure.ReconciledItem, bool) {
	items := s.actionItems()
	if len(items) == 0 {
		return exposure.ReconciledItem{}, false
	}
	return items[0], true
}

func (s *workspaceState) selectedItem() (exposure.ReconciledItem, bool) {
	items := s.items()
	if len(items) == 0 {
		return exposure.ReconciledItem{}, false
	}
	if s.selectedIdx < 0 || s.selectedIdx >= len(items) || items[s.selectedIdx].ID != s.selectedID {
		s.reselect(s.selectedID, s.selectedIdx)
	}
	if len(items) == 0 || s.selectedIdx >= len(items) {
		return exposure.ReconciledItem{}, false
	}
	return items[s.selectedIdx], true
}

func (s workspaceState) items() []exposure.ReconciledItem {
	return newWorkspaceSnapshot(s.view, s.query, s.cfg).Items()
}

func (s *workspaceState) reselect(previousID string, previousIndex int) {
	items := s.items()
	if len(items) == 0 {
		s.selectedID, s.selectedIdx = "", 0
		s.detailOffset = 0
		return
	}
	if previousID != "" {
		for i, item := range items {
			if item.ID == previousID {
				s.selectedID, s.selectedIdx = previousID, i
				return
			}
		}
	}
	s.selectedIdx = clamp(previousIndex, 0, len(items)-1)
	s.selectedID = items[s.selectedIdx].ID
	s.detailOffset = 0
}

func (s *workspaceState) moveSelection(delta int) {
	items := s.items()
	if len(items) == 0 {
		return
	}
	if s.selectedID == "" {
		s.reselect("", 0)
		return
	}
	index := s.selectedIdx
	if index < 0 || index >= len(items) || items[index].ID != s.selectedID {
		s.reselect(s.selectedID, index)
		index = s.selectedIdx
	}
	index = (index + delta) % len(items)
	if index < 0 {
		index += len(items)
	}
	s.selectedIdx, s.selectedID, s.detailOffset = index, items[index].ID, 0
	s.updateVisualSelection()
}

func (s *workspaceState) goFirst() {
	items := s.items()
	if len(items) > 0 {
		s.selectedIdx, s.selectedID, s.detailOffset = 0, items[0].ID, 0
		s.updateVisualSelection()
	}
}

func (s *workspaceState) goLast() {
	items := s.items()
	if len(items) > 0 {
		s.selectedIdx, s.selectedID, s.detailOffset = len(items)-1, items[len(items)-1].ID, 0
		s.updateVisualSelection()
	}
}

func (s workspaceState) listPage() int   { return maxInt(1, s.height/2) }
func (s workspaceState) detailPage() int { return maxInt(1, s.height-8) }

func (s *workspaceState) markApplying(key string) {
	for i := range s.view.Items {
		itemKey, ok := itemTarget(s.view.Items[i])
		if ok && itemKey.Key() == key {
			s.view.Items[i].OperationState = exposuredata.ExposureApplying
			s.view.Items[i].State = exposuredata.ExposureApplying
		}
	}
}

func (s *workspaceState) applyLocalOperationResult(message operationDoneMsg) {
	for i := range s.view.Items {
		key, ok := itemTarget(s.view.Items[i])
		if !ok || key.Key() != message.targetKey {
			continue
		}
		if message.err != nil {
			s.view.Items[i].OperationState = workspace.OperationStateForError(message.err)
		} else if message.receipt.Verified {
			s.view.Items[i].OperationState = exposuredata.ExposureSucceeded
		} else {
			s.view.Items[i].OperationState = exposuredata.ExposureUnverified
		}
		copyReceipt := message.receipt
		s.view.Items[i].LastOperation = &copyReceipt
	}
}

func (s *workspaceState) markOperationUnverified(targetKey string) {
	for i := range s.view.Items {
		target, ok := itemTarget(s.view.Items[i])
		if ok && target.Key() == targetKey {
			s.view.Items[i].OperationState = exposuredata.ExposureUnverified
		}
	}
}

func (s *workspaceState) markActiveOperationsUnverified() {
	for targetKey := range s.activeOps {
		s.markOperationUnverified(targetKey)
	}
}

func (s workspaceState) hasPendingOperations() bool {
	return len(s.activeOps) > 0 || s.processBusy
}
