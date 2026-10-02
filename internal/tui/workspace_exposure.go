package tui

// Exposure action previews, confirmation guards, mutation approvals, and operation orchestration.

import (
	"context"
	"fmt"
	"time"

	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
)

type batchOperation struct {
	itemID          string
	target          target.Target
	providerKey     string
	routeID         string
	confirmExternal bool
	approval        exposure.MutationApproval
}

type batchContext struct {
	context.Context
	cancel context.CancelFunc
}

func newBatchContext(parent context.Context) batchContext {
	ctx, cancel := context.WithCancel(parent)
	return batchContext{Context: ctx, cancel: cancel}
}

func batchRouteSelection(mode exposuredata.ExposureMode, item exposure.ReconciledItem) (providerKey, routeID string) {
	if mode != exposuredata.ExposureDisabled || len(item.Routes) != 1 {
		return "", ""
	}
	route := item.Routes[0]
	return route.ProviderKey, route.ID
}

func (m *workspaceModel) openAction(requested *exposuredata.ExposureMode) {
	item, ok := m.actionAnchorItem()
	if !ok {
		m.setBanner("No service is selected", true)
		return
	}
	m.discardHTTPPathAction()
	m.modal = modalAction
	target, _ := itemTarget(item)
	mode := observedMode(item)
	if requested != nil {
		mode = *requested
	}
	m.actionSession.open(item.ID, target, mode)
	m.actionSession.capturePreview(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	)
	m.actionSession.refreshChoices(m.actionAvailability)
	m.transient = "Preview: " + string(mode)
}

func (m *workspaceModel) refreshActionModal() {
	if m.modal == modalAction {
		m.actionSession.refreshChoices(m.actionAvailability)
	}
}

func (m *workspaceModel) invalidatePreviewIfChanged() {
	if m.modal == modalChooseURL {
		item, ok := m.selectedItem()
		if !ok || item.ID != m.urlItemID || routeFingerprint(item.Routes, nil) != m.urlRoutesFingerprint {
			m.modal, m.urlAction, m.urlItemID, m.urlRoutesFingerprint = modalNone, "", "", ""
			m.setBanner("Observed route URLs changed — select a fresh URL", true)
		}
		return
	}
	if m.modal == modalTerminateProcess {
		valid := len(m.processBatch) > 0
		for _, target := range m.processBatch {
			if _, err := m.revalidateProcessTarget(target); err != nil {
				valid = false
				break
			}
		}
		if !valid {
			m.modal = modalNone
			m.processBatch = nil
			m.setBanner("Selection changed — refresh required", true)
		}
		return
	}
	// Keep an explicit confirmation visible while refreshed state arrives. The
	// apply boundary rechecks refresh/readiness state and the preview before
	// starting a mutation; closing confirmation here would turn a background
	// refresh into an unsolicited operator action.
	if m.modal != modalAction && m.modal != modalDisableRoute && m.modal != modalHTTPPath && !(m.httpPathAction && m.modal == modalConfirm) {
		return
	}
	if !m.actionSession.previewChanged(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	) {
		return
	}
	m.modal = modalNone
	m.discardHTTPPathAction()
	m.setBanner("Selection changed — refresh required", true)
}

func observedMode(item exposure.ReconciledItem) exposuredata.ExposureMode {
	if len(item.Routes) == 1 {
		return item.Routes[0].Mode
	}
	return exposuredata.ExposureDisabled
}

func (m *workspaceModel) actionAvailability(mode exposuredata.ExposureMode) workspace.ActionAvailability {
	return workspace.ActionAvailabilityForItems(m.actionItems(), mode, workspace.ActionContext{
		ConfigError:    m.configErr,
		View:           m.view,
		Readiness:      m.readiness,
		ViewError:      m.viewErr,
		ReadinessError: m.readyErr,
		RefreshPending: m.refreshState.isPending(),
		ProcessBusy:    m.processBusy,
	})
}

func (m *workspaceModel) startOperation() tea.Cmd {
	// Refresh lifecycle belongs to the workspace, not the action modal. If a
	// refresh starts after confirmation opened, return to the selector and let
	// the workspace update its supplied choices when fresh state arrives.
	if m.refreshState.isPending() {
		if m.httpPathAction {
			m.modal = modalHTTPPath
			m.setBanner("HTTP path preview paused while refresh is in progress; review it again after refresh", true)
		} else {
			m.modal = modalAction
			m.refreshActionModal()
		}
		return nil
	}
	// Re-evaluate the complete action guard at the mutation boundary. A
	// readiness result can change while the confirmation modal is open without
	// changing route or listener fingerprints; confirmation must never bypass
	// that newer workspace-owned safety decision.
	availability := m.actionAvailability(m.actionSession.mode)
	if m.httpPathAction {
		availability = workspace.ActionAvailability{}
		item, ok := m.actionAnchorItem()
		if !ok {
			availability = workspace.ActionAvailability{Disabled: true, Reason: "No exact listener is selected"}
		} else if reason := m.httpPathBaseReason(item); reason != "" {
			availability = workspace.ActionAvailability{Disabled: true, Reason: reason}
		} else if status, message, remediation := workspace.HTTPPathStatus(m.readiness, m.httpPathMode); status != "ready" {
			availability = workspace.ActionAvailability{Disabled: true, Reason: httpPathReadinessReason(m.httpPathMode, status, message, remediation)}
		}
	}
	if availability.Disabled {
		if m.httpPathAction {
			m.modal = modalHTTPPath
		} else {
			m.modal = modalAction
			m.refreshActionModal()
		}
		if !availability.Wait {
			m.setBanner(availability.Reason, true)
		}
		return nil
	}
	if len(m.actionItems()) > 1 {
		return m.startBatchOperation()
	}
	return m.startSingleOperation()
}

func (m *workspaceModel) externalPreview() bool {
	item, ok := m.actionAnchorItem()
	if !ok {
		return false
	}
	routeKey := m.actionSession.routeID
	if routeKey == "" {
		routeKey = m.actionSession.routeKey
	}
	return workspace.ExternalPreview(item, routeKey)
}

func (m *workspaceModel) mutationApproval(item exposure.ReconciledItem, target target.Target) exposure.MutationApproval {
	approval := exposure.MutationApproval{
		Target:           target,
		RouteIDsHash:     exposure.RouteIDsHash(m.view.Exposures.Routes, target),
		TargetRoutesHash: exposure.RouteIdentityHash(m.view.Exposures.Routes, target),
		AllRoutesHash:    tailscale.RoutesHash(m.view.Exposures.Routes),
	}
	if item.Listener != nil {
		listener := *item.Listener
		approval.ListenerID = listener.ID
		approval.ListenerPID = listener.PID
		approval.ListenerProcess = listener.Process
		approval.ListenerStart = listener.ProcessStart
		approval.ListenerCommandLine = listener.CommandLine
		approval.ListenerTarget = listener.Target
	}
	return approval
}

func (m *workspaceModel) startSingleOperation() tea.Cmd {
	item, ok := m.actionAnchorItem()
	if !ok || item.ID != m.actionSession.itemID {
		if m.httpPathAction {
			m.modal = modalNone
			m.discardHTTPPathAction()
		}
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	if m.actionSession.previewChanged(
		routeFingerprint(m.view.Exposures.Routes, &m.actionSession.target),
		routeFingerprint(m.view.Exposures.Routes, nil),
		listenerFingerprint(m.view.Listeners, m.actionSession.target),
		selectionFingerprint(m.actionItems()),
	) {
		if m.httpPathAction {
			m.modal = modalNone
			m.discardHTTPPathAction()
		}
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	key := m.actionSession.target.Key()
	if m.httpPathAction {
		if len(m.actionItems()) != 1 || m.httpPath == "" {
			m.modal = modalHTTPPath
			m.setBanner("Named HTTP path preview is incomplete; enter a path and confirm again", true)
			return nil
		}
		if _, busy := m.activeOps[key]; busy {
			m.setBanner("This target already has an operation Applying", true)
			return nil
		}
		ctx, cancel := context.WithCancel(m.ctx)
		m.activeOps[key] = cancel
		m.markApplying(key)
		target := m.actionSession.target
		path, mode := m.httpPath, m.httpPathMode
		options := exposure.HTTPPathOptions{LocalhostBackendAlias: m.httpPathLocalhostBackend}
		controller, timeout := m.controller, m.cfg.OperationTimeout
		approval := m.mutationApproval(item, target)
		m.discardHTTPPathAction()
		m.transient = "Applying HTTP path " + path + " to " + target.String()
		return func() tea.Msg {
			receipt, err := controller.ApplyHTTPPathWithOptionsApproved(ctx, target, path, mode, mode == exposuredata.ExposureFunnel, options, timeout, approval)
			return operationDoneMsg{targetKey: key, receipt: receipt, err: err}
		}
	}
	if _, busy := m.activeOps[key]; busy {
		m.setBanner("This target already has an operation Applying", true)
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.activeOps[key] = cancel
	m.markApplying(key)
	mode, confirmExternal := m.actionSession.mode, m.externalPreview()
	confirmFunnel := mode == exposuredata.ExposureFunnel
	m.transient = "Applying " + string(mode) + " to " + m.actionSession.target.String()
	target := m.actionSession.target
	routeKey := m.actionSession.routeKey
	routeID := m.actionSession.routeID
	controller := m.controller
	timeout := m.cfg.OperationTimeout
	approval := m.mutationApproval(item, target)
	return func() tea.Msg {
		var receipt exposuredata.OperationReceipt
		var err error
		if mode == exposuredata.ExposureDisabled && routeID != "" {
			receipt, err = controller.ApplyRouteIdentityApproved(ctx, target, routeID, confirmExternal, timeout, approval)
		} else if mode == exposuredata.ExposureDisabled && routeKey != "" {
			receipt, err = controller.ApplyRouteApproved(ctx, target, routeKey, confirmExternal, timeout, approval)
		} else {
			receipt, err = controller.ApplyApproved(ctx, target, mode, confirmFunnel, confirmExternal, timeout, approval)
		}
		return operationDoneMsg{targetKey: key, receipt: receipt, err: err}
	}
}

func (m *workspaceModel) startBatchOperation() tea.Cmd {
	items := m.actionItems()
	if len(items) <= 1 {
		return m.startSingleOperation()
	}
	if m.actionSession.preview.selectionHash == "" || selectionFingerprint(items) != m.actionSession.preview.selectionHash || routeFingerprint(m.view.Exposures.Routes, nil) != m.actionSession.preview.allRoutesHash {
		m.setBanner("Selection changed — refresh required", true)
		return nil
	}
	requests := make([]batchOperation, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		target, ok := itemTarget(item)
		if !ok {
			m.setBanner(item.ID+": exact target is unavailable", true)
			return nil
		}
		key := target.Normalized().Key()
		if seen[key] {
			m.setBanner("Selection contains duplicate target identities; refresh and select exact listeners", true)
			return nil
		}
		seen[key] = true
		if _, busy := m.activeOps[key]; busy {
			m.setBanner("Selection contains a target with an operation Applying", true)
			return nil
		}
		if workspace.SameStateForItem(m.view, item, m.actionSession.mode) {
			continue
		}
		providerKey, routeID := batchRouteSelection(m.actionSession.mode, item)
		approval := m.mutationApproval(item, target)
		approval.AllowOtherRouteChanges = true
		routeChoice := routeID
		if routeChoice == "" {
			routeChoice = providerKey
		}
		requests = append(requests, batchOperation{itemID: item.ID, target: target, providerKey: providerKey, routeID: routeID, confirmExternal: workspace.ExternalPreview(item, routeChoice), approval: approval})
	}
	if len(requests) == 0 {
		m.modal = modalNone
		m.transient = fmt.Sprintf("Already %s for %d selected service(s)", m.actionSession.mode, len(items))
		return nil
	}
	batchCtx := newBatchContext(m.ctx)
	batchID := target.StableID("batch", string(m.actionSession.mode), time.Now().UTC().Format(time.RFC3339Nano))
	m.batchID, m.batchTotal, m.batchCompleted, m.batchFailures = batchID, len(requests), 0, 0
	m.batchCancel = batchCtx.cancel
	for _, request := range requests {
		key := request.target.Normalized().Key()
		m.activeOps[key] = batchCtx.cancel
		m.markApplying(key)
	}
	m.transient = fmt.Sprintf("Applying %s to %d selected services", m.actionSession.mode, len(requests))
	cmds := make([]tea.Cmd, 0, len(requests))
	mode, confirmFunnel := m.actionSession.mode, m.actionSession.mode == exposuredata.ExposureFunnel
	controller := m.controller
	timeout := m.cfg.OperationTimeout
	for index, request := range requests {
		request, index := request, index
		cmds = append(cmds, func() tea.Msg {
			var receipt exposuredata.OperationReceipt
			var err error
			if mode == exposuredata.ExposureDisabled && request.routeID != "" {
				receipt, err = controller.ApplyRouteIdentityApproved(batchCtx, request.target, request.routeID, request.confirmExternal, timeout, request.approval)
			} else if mode == exposuredata.ExposureDisabled && request.providerKey != "" {
				receipt, err = controller.ApplyRouteApproved(batchCtx, request.target, request.providerKey, request.confirmExternal, timeout, request.approval)
			} else {
				receipt, err = controller.ApplyApproved(batchCtx, request.target, mode, confirmFunnel, request.confirmExternal, timeout, request.approval)
			}
			return operationDoneMsg{targetKey: request.target.Normalized().Key(), receipt: receipt, err: err, batchID: batchID, batchIndex: index, batchTotal: len(requests)}
		})
	}
	return tea.Sequence(cmds...)
}
