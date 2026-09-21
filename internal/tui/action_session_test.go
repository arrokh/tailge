package tui

import (
	"testing"

	"github.com/arrokh/tailge/internal/model"
)

func TestExposureActionSessionChoiceStateIsSuppliedByWorkspace(t *testing.T) {
	var session exposureActionSession
	session.open("listener", model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, model.ExposureFunnel)
	session.refreshChoices(func(mode model.ExposureMode) exposureActionAvailability {
		return exposureActionAvailability{disabled: mode == model.ExposureFunnel, reason: "waiting", wait: mode == model.ExposureFunnel}
	})
	choice, ok := session.selectedChoice()
	if !ok || choice.mode != model.ExposureFunnel || !choice.disabled || !choice.wait || choice.reason != "waiting" {
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
