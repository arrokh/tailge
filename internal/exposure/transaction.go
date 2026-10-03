package exposure

import (
	"context"
	"strconv"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

// apply is the Controller lifecycle adapter around exactExposureOperation.
// Controller reserves the target, publishes lifecycle events, and records the
// final receipt; the operation owns the ordered provider mutation protocol.
func (c *Controller) apply(ctx context.Context, target targetmodel.Target, mode exposuredata.ExposureMode, selectedProviderKey string, selectedRouteMode exposuredata.ExposureMode, confirmFunnel, confirmExternal bool, timeout time.Duration, approval *MutationApproval) (exposuredata.OperationReceipt, error) {
	op := c.newExactOperation(target, mode, selectedProviderKey, "", selectedRouteMode, confirmFunnel, confirmExternal, timeout, approval)
	return c.trackOperation(ctx, op)
}

func (c *Controller) applyRouteIdentity(ctx context.Context, target targetmodel.Target, routeID string, confirmExternal bool, timeout time.Duration, approval *MutationApproval) (exposuredata.OperationReceipt, error) {
	if routeID == "" {
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrInvalidInput, "exposure", "an exact route identity is required", false, "invalid", "Refresh and select one exact route before disabling.")
	}
	op := c.newExactOperation(target, exposuredata.ExposureDisabled, "", routeID, exposuredata.ExposureDisabled, false, confirmExternal, timeout, approval)
	return c.trackOperation(ctx, op)
}

func (c *Controller) applyHTTPPath(ctx context.Context, target targetmodel.Target, path string, mode exposuredata.ExposureMode, confirmFunnel bool, options HTTPPathOptions, timeout time.Duration, approval *MutationApproval) (exposuredata.OperationReceipt, error) {
	op := c.newExactOperation(target, mode, "", "", exposuredata.ExposureDisabled, confirmFunnel, false, timeout, approval)
	op.httpPath = path
	op.httpPathIntent = true
	op.httpsPort = 443
	op.localhostBackendAlias = options.LocalhostBackendAlias
	return c.trackOperation(ctx, op)
}

func (c *Controller) applyHTTPSRoot(ctx context.Context, target targetmodel.Target, options HTTPSRootOptions, timeout time.Duration, approval *MutationApproval) (exposuredata.OperationReceipt, error) {
	mode := options.Mode
	if mode == "" {
		mode = exposuredata.ExposureServe
	}
	op := c.newExactOperation(target, mode, "", "", exposuredata.ExposureDisabled, options.ConfirmFunnel, options.ConfirmExternal, timeout, approval)
	op.httpPath = "/"
	op.httpsPort = options.HTTPSPort
	op.httpsRootIntent = true
	op.localhostBackendAlias = options.LocalhostBackendAlias
	op.replaceRawTCP = options.ReplaceRawTCP
	op.expectedRawTCPRouteID = options.ExpectedRawTCPRouteID
	op.replaceExistingRoute = options.ReplaceExistingRoute
	op.expectedRouteID = options.ExpectedRouteID
	return c.trackOperation(ctx, op)
}

func (c *Controller) newExactOperation(target targetmodel.Target, mode exposuredata.ExposureMode, selectedProviderKey, selectedRouteID string, selectedRouteMode exposuredata.ExposureMode, confirmFunnel, confirmExternal bool, timeout time.Duration, approval *MutationApproval) exactExposureOperation {
	return exactExposureOperation{
		dependencies: exactOperationDependencies{
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
		},
		target:              target,
		mode:                mode,
		selectedProviderKey: selectedProviderKey,
		selectedRouteID:     selectedRouteID,
		selectedRouteMode:   selectedRouteMode,
		confirmFunnel:       confirmFunnel,
		confirmExternal:     confirmExternal,
		timeout:             timeout,
		approval:            approval,
	}
}

func (c *Controller) trackOperation(ctx context.Context, op exactExposureOperation) (receipt exposuredata.OperationReceipt, applyErr error) {
	if err := op.validate(); err != nil {
		return exposuredata.OperationReceipt{}, err
	}

	c.mu.Lock()
	if c.busy == nil {
		c.busy = map[string]bool{}
	}
	if c.desired == nil {
		c.desired = map[string]exposuredata.ExposureMode{}
	}
	if c.operations == nil {
		c.operations = map[string]exposuredata.OperationReceipt{}
	}
	if c.operationStates == nil {
		c.operationStates = map[string]exposuredata.ExposureState{}
	}
	if c.operationStarted == nil {
		c.operationStarted = map[string]time.Time{}
	}
	operationKey := op.target.Key()
	if c.busy[operationKey] {
		c.mu.Unlock()
		return exposuredata.OperationReceipt{}, fault.NewError(fault.ErrOperation, "exposure", "another operation is already applying to this target", true, "applying", "Wait for the current operation to finish.")
	}
	operationStarted := c.now()
	operationID := targetmodel.StableID("apply", operationKey, string(op.mode), operationStarted.UTC().Format(time.RFC3339Nano))
	if op.httpPathIntent {
		operationID = targetmodel.StableID("apply-http-path", operationKey, string(op.mode), op.httpPath, operationStarted.UTC().Format(time.RFC3339Nano))
	} else if op.httpsRootIntent {
		operationID = targetmodel.StableID("apply-https-root", operationKey, strconv.Itoa(op.httpsPort), operationStarted.UTC().Format(time.RFC3339Nano))
	}
	op.operationID = operationID
	op.operationStarted = operationStarted
	receipt = exposuredata.OperationReceipt{ID: operationID, StartedAt: operationStarted}
	c.busy[operationKey] = true
	c.desired[operationKey] = op.mode
	c.operationStates[operationKey] = exposuredata.ExposureApplying
	c.operationStarted[operationID] = operationStarted
	c.recordEventLocked(exposuredata.OperationEvent{OperationID: operationID, Phase: "start", At: operationStarted, Target: op.target, Mode: op.mode, Capability: operationCapability(op.mode), State: exposuredata.ExposureApplying})
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
			appErr := fault.AsAppError(applyErr)
			if receipt.Error == nil {
				receipt.Error = ptr(appErr.Safe())
			}
			if receipt.ExitCode == 0 {
				receipt.ExitCode = appErr.Exit
			}
		}
		state := exposuredata.ExposureUnverified
		if applyErr == nil && receipt.Verified {
			state = exposuredata.ExposureSucceeded
		} else if applyErr != nil {
			state = operationStateForError(applyErr)
		}
		c.mu.Lock()
		c.operationStates[operationKey] = state
		c.operations[operationKey] = receipt
		started := c.operationStarted[operationID]
		delete(c.operationStarted, operationID)
		c.recordEventLocked(exposuredata.OperationEvent{OperationID: operationID, Phase: "final", At: receipt.FinishedAt, Duration: eventDuration(started, receipt.FinishedAt), Target: op.target, Mode: op.mode, Capability: operationCapability(op.mode), State: state, ErrorCode: operationErrorCode(applyErr)})
		delete(c.busy, operationKey)
		c.mu.Unlock()
	}()

	return op.run(ctx)
}
