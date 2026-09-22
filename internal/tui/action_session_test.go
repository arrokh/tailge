package tui

import (
	"testing"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/workspace"
)

func TestExposureActionSessionChoiceStateIsSuppliedByWorkspace(t *testing.T) {
	var session exposureActionSession
	session.open("listener", target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, exposuredata.ExposureFunnel)
	session.refreshChoices(func(mode exposuredata.ExposureMode) exposureActionAvailability {
		return workspace.ActionAvailability{Disabled: mode == exposuredata.ExposureFunnel, Reason: "waiting", Wait: mode == exposuredata.ExposureFunnel}
	})
	choice, ok := session.selectedChoice()
	if !ok || choice.mode != exposuredata.ExposureFunnel || !choice.disabled || !choice.wait || choice.reason != "waiting" {
		t.Fatalf("unexpected supplied choice: %#v, ok=%t", choice, ok)
	}
}

func TestExposureActionSessionPreviewFingerprint(t *testing.T) {
	var session exposureActionSession
	session.capturePreview("route", "all", "listener", "selection")
	if session.previewChanged("route", "all", "listener", "selection") {
		t.Fatal("unchanged preview was invalidated")
	}
	if !session.previewChanged("changed", "all", "listener", "selection") {
		t.Fatal("route change did not invalidate preview")
	}
}
