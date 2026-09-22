package tui

// Guarded local-process termination and sequential process batches.

import (
	"context"
	"fmt"
	"time"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/workspace"
	tea "github.com/charmbracelet/bubbletea"
)

type processTarget struct {
	itemID      string
	listener    discovery.Listener
	fingerprint string
}

type processDoneMsg struct {
	itemID     string
	pid        int
	err        error
	batchIndex int
	batchTotal int
}

func (m *workspaceModel) openTerminateProcess() {
	if m.processBusy {
		m.setBanner("Process termination is already in progress", true)
		return
	}
	targets, reason := m.collectProcessTargets()
	if reason != "" {
		m.setBanner("Terminate unavailable: "+reason, true)
		return
	}
	if len(targets) == 0 {
		m.setBanner("Terminate unavailable: no local listener is selected", true)
		return
	}
	m.processBatch = targets
	first := targets[0]
	m.modal = modalTerminateProcess
	m.modalItemID = first.itemID
	m.modalTarget = first.listener.Target
	m.modalProcess = first.listener
	m.modalProcessFingerprint = first.fingerprint
	m.confirmFocus = false
}

func (m *workspaceModel) startTerminateProcess() tea.Cmd {
	if m.processBusy {
		m.setBanner("Process termination is already in progress", true)
		return nil
	}
	if len(m.processBatch) > 1 {
		m.processBatchIndex = 0
		m.processBatchDone = 0
		m.processBatchFailed = 0
		return m.startNextProcessTermination()
	}
	var target processTarget
	if len(m.processBatch) == 1 {
		target = m.processBatch[0]
		m.processBatch = nil
	} else {
		item, ok := m.selectedItem()
		if !ok || item.ID != m.modalItemID || item.Listener == nil || workspace.ProcessFingerprint(*item.Listener) != m.modalProcessFingerprint {
			m.setBanner("Selection changed — refresh required", true)
			return nil
		}
		target = processTarget{itemID: item.ID, listener: *item.Listener, fingerprint: m.modalProcessFingerprint}
	}
	return m.startProcessTarget(target, 0, 0)
}

func (m *workspaceModel) startNextProcessTermination() tea.Cmd {
	if m.processBatchIndex >= len(m.processBatch) {
		return nil
	}
	return m.startProcessTarget(m.processBatch[m.processBatchIndex], m.processBatchIndex, len(m.processBatch))
}

func (m *workspaceModel) startProcessTarget(target processTarget, batchIndex, batchTotal int) tea.Cmd {
	listener, err := m.revalidateProcessTarget(target)
	if err != nil {
		return func() tea.Msg {
			return processDoneMsg{itemID: target.itemID, pid: target.listener.PID, err: err, batchIndex: batchIndex, batchTotal: batchTotal}
		}
	}
	if m.processTerminator == nil {
		err := fmt.Errorf("process termination is unsupported on this platform")
		return func() tea.Msg {
			return processDoneMsg{itemID: target.itemID, pid: listener.PID, err: err, batchIndex: batchIndex, batchTotal: batchTotal}
		}
	}
	ctx, cancel := context.WithTimeout(m.ctx, boundedTimeout(m.cfg.OperationTimeout, 15*time.Second, 10*time.Minute))
	m.processBusy = true
	m.processCancel = cancel
	m.transient = fmt.Sprintf("Terminating process %d (%d/%d)", listener.PID, batchIndex+1, maxInt(1, batchTotal))
	return func() tea.Msg {
		defer cancel()
		return processDoneMsg{itemID: target.itemID, pid: listener.PID, err: m.processTerminator.Terminate(ctx, listener), batchIndex: batchIndex, batchTotal: batchTotal}
	}
}

func (m *workspaceModel) finishProcessBatch(message processDoneMsg) tea.Cmd {
	m.processBatchDone++
	if message.err != nil {
		m.processBatchFailed++
	}
	if message.err != nil && m.quittingAfterCancel {
		m.processUnverified = true
	}
	if m.processBatchDone < message.batchTotal && !m.quittingAfterCancel {
		m.processBatchIndex++
		m.transient = fmt.Sprintf("Terminating services (%d/%d)", m.processBatchDone, message.batchTotal)
		return m.startNextProcessTermination()
	}
	if m.quittingAfterCancel && m.processBatchDone < message.batchTotal {
		m.processUnverified = true
	}
	failed := m.processBatchFailed
	total := message.batchTotal
	m.processBatch = nil
	m.processBatchIndex, m.processBatchDone, m.processBatchFailed = 0, 0, 0
	if failed > 0 {
		m.setBanner(fmt.Sprintf("Process termination completed with %d failure(s) out of %d", failed, total), true)
	} else {
		m.transient = fmt.Sprintf("Terminated %d process(es)", total)
	}
	if m.quittingAfterCancel {
		if !m.hasPendingOperations() {
			m.quittingAfterCancel = false
			m.cancel()
			return tea.Quit
		}
		return nil
	}
	return m.startRefresh()
}
