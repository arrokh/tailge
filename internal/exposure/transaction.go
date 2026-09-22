package exposure

import (
	"context"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

// apply is the Controller lifecycle adapter around exactExposureOperation.
// Controller reserves the target, publishes lifecycle events, and records the
// final receipt; the operation owns the ordered provider mutation protocol.
func (c *Controller) apply(ctx context.Context, target model.Target, mode model.ExposureMode, selectedProviderKey string, selectedRouteMode model.ExposureMode, confirmFunnel, confirmExternal bool, timeout time.Duration, approval *MutationApproval) (receipt model.OperationReceipt, applyErr error) {
	op := exactExposureOperation{
		target:              target,
		mode:                mode,
		selectedProviderKey: selectedProviderKey,
		selectedRouteMode:   selectedRouteMode,
		confirmFunnel:       confirmFunnel,
		confirmExternal:     confirmExternal,
		timeout:             timeout,
		approval:            approval,
	}
	op.dependencies = exactOperationDependencies{
		discoverer:         c.Discoverer,
		provider:           c.Provider,
		mutationLockPath:   c.MutationLockPath,
		readinessOptions:   c.ReadinessOptions,
		now:                c.now,
		decorateRoutes:     c.decorateExposureRoutes,
		markManaged:        c.markManagedRoute,
		revokeManaged:      c.revokeManagedRoute,
		recordReceipt:      c.recordReceiptEvent,
		recordVerification: c.recordVerificationEvent,
	}
	if err := op.validate(); err != nil {
		return model.OperationReceipt{}, err
	}

	c.mu.Lock()
	if c.busy == nil {
		c.busy = map[string]bool{}
	}
	if c.desired == nil {
		c.desired = map[string]model.ExposureMode{}
	}
	if c.operations == nil {
		c.operations = map[string]model.OperationReceipt{}
	}
	if c.operationStates == nil {
		c.operationStates = map[string]model.ExposureState{}
	}
	if c.operationStarted == nil {
		c.operationStarted = map[string]time.Time{}
	}
	operationKey := op.target.Key()
	if c.busy[operationKey] {
		c.mu.Unlock()
		return model.OperationReceipt{}, model.NewError(model.ErrOperation, "exposure", "another operation is already applying to this target", true, "applying", "Wait for the current operation to finish.")
	}
	operationStarted := c.now()
	operationID := model.StableID("apply", operationKey, string(op.mode), operationStarted.UTC().Format(time.RFC3339Nano))
	op.operationID = operationID
	op.operationStarted = operationStarted
	receipt = model.OperationReceipt{ID: operationID, StartedAt: operationStarted}
	c.busy[operationKey] = true
	c.desired[operationKey] = op.mode
	c.operationStates[operationKey] = model.ExposureApplying
	c.operationStarted[operationID] = operationStarted
	c.recordEventLocked(model.OperationEvent{OperationID: operationID, Phase: "start", At: operationStarted, Target: op.target, Mode: op.mode, Capability: operationCapability(op.mode), State: model.ExposureApplying})
	c.mu.Unlock()
	defer func() {
		if receipt.ID == "" {
			receipt.ID = operationID
		}
		if receipt.StartedAt.IsZero() {
			receipt.StartedAt = operationStarted
		}
		if receipt.FinishedAt.IsZero() {
			receipt.FinishedAt = c.now()
		}
		if applyErr != nil {
			receipt.Verified = false
			appErr := model.AsAppError(applyErr)
			if receipt.Error == nil {
				receipt.Error = ptr(appErr.Safe())
			}
			if receipt.ExitCode == 0 {
				receipt.ExitCode = appErr.Exit
			}
		}
		state := model.ExposureUnverified
		if applyErr == nil && receipt.Verified {
			state = model.ExposureSucceeded
		} else if applyErr != nil {
			state = operationStateForError(applyErr)
		}
		c.mu.Lock()
		c.operationStates[operationKey] = state
		c.operations[operationKey] = receipt
		started := c.operationStarted[operationID]
		delete(c.operationStarted, operationID)
		c.recordEventLocked(model.OperationEvent{OperationID: operationID, Phase: "final", At: receipt.FinishedAt, Duration: eventDuration(started, receipt.FinishedAt), Target: op.target, Mode: op.mode, Capability: operationCapability(op.mode), State: state, ErrorCode: operationErrorCode(applyErr)})
		delete(c.busy, operationKey)
		c.mu.Unlock()
	}()

	return op.run(ctx)
}
