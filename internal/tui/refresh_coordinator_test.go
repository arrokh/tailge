package tui

import (
	"errors"
	"testing"
)

func TestRefreshCoordinatorRejectsOlderResultsAndCoalescesRequests(t *testing.T) {
	var coordinator refreshCoordinator
	first, started := coordinator.start()
	if !started || first != 1 || !coordinator.isPending() {
		t.Fatalf("first refresh = seq %d started=%t pending=%t", first, started, coordinator.isPending())
	}
	if second, started := coordinator.start(); started || second != first {
		t.Fatalf("concurrent refresh was not coalesced: seq=%d started=%t", second, started)
	}
	if coordinator.accept(first-1, refreshViewPart) {
		t.Fatal("older generation was accepted")
	}
	if coordinator.finish(first - 1) {
		t.Fatal("older generation completed refresh")
	}
	if !coordinator.accept(first, refreshViewPart) || !coordinator.accept(first, refreshReadinessPart) || !coordinator.finish(first) {
		t.Fatal("current generation did not complete")
	}
	if coordinator.accept(first, refreshViewPart) || coordinator.finish(first) {
		t.Fatal("completed generation accepted a duplicate result")
	}
	if coordinator.isPending() || !coordinator.consumeAgain() {
		t.Fatalf("coalesced refresh was not queued: pending=%t", coordinator.isPending())
	}
}

func TestRefreshCoordinatorTracksRetryAndFailures(t *testing.T) {
	var coordinator refreshCoordinator
	coordinator.requestRetryPreview()
	if !coordinator.consumeRetryPreview() || coordinator.consumeRetryPreview() {
		t.Fatal("retry preview request was not consumed exactly once")
	}
	coordinator.recordResult(errors.New("listener"), nil)
	coordinator.recordResult(errors.New("listener"), errors.New("readiness"))
	if coordinator.failureCount() != 2 {
		t.Fatalf("failure streak=%d, want 2", coordinator.failureCount())
	}
	coordinator.recordResult(nil, nil)
	if coordinator.failureCount() != 0 {
		t.Fatalf("successful refresh did not reset failures: %d", coordinator.failureCount())
	}
}

func TestRefreshCoordinatorRejectsDuplicateSourceResults(t *testing.T) {
	var coordinator refreshCoordinator
	seq, started := coordinator.start()
	if !started {
		t.Fatal("refresh did not start")
	}
	if !coordinator.accept(seq, refreshViewPart) {
		t.Fatal("first view result was rejected")
	}
	if coordinator.accept(seq, refreshViewPart) {
		t.Fatal("duplicate view result was accepted")
	}
	if !coordinator.accept(seq, refreshReadinessPart) || !coordinator.finish(seq) {
		t.Fatal("refresh did not finish after distinct source results")
	}
}
