package tui

import "time"

type refreshPart uint8

const (
	refreshViewPart refreshPart = iota
	refreshReadinessPart
)

// refreshCoordinator owns refresh generations and lifecycle policy. The
// workspace remains responsible for publishing the accepted source results;
// this module decides which result is current and when a refresh is complete.
type refreshCoordinator struct {
	seq               uint64
	pending           bool
	viewDone          bool
	readinessDone     bool
	again             bool
	retryAfterRefresh bool
	failureStreak     int
}

func (r *refreshCoordinator) start() (uint64, bool) {
	if r.pending {
		r.again = true
		return r.seq, false
	}
	r.seq++
	r.pending = true
	r.viewDone = false
	r.readinessDone = false
	return r.seq, true
}

func (r *refreshCoordinator) accept(seq uint64, part refreshPart) bool {
	if !r.pending || seq != r.seq {
		return false
	}
	switch part {
	case refreshViewPart:
		if r.viewDone {
			return false
		}
		r.viewDone = true
	case refreshReadinessPart:
		if r.readinessDone {
			return false
		}
		r.readinessDone = true
	default:
		return false
	}
	return true
}

func (r *refreshCoordinator) finish(seq uint64) bool {
	if !r.pending || seq != r.seq || !r.viewDone || !r.readinessDone {
		return false
	}
	r.pending = false
	return true
}

func (r *refreshCoordinator) consumeAgain() bool {
	if !r.again {
		return false
	}
	r.again = false
	return true
}

func (r *refreshCoordinator) requestRetryPreview() {
	r.retryAfterRefresh = true
}

func (r *refreshCoordinator) consumeRetryPreview() bool {
	if !r.retryAfterRefresh {
		return false
	}
	r.retryAfterRefresh = false
	return true
}

func (r *refreshCoordinator) recordResult(viewErr, readinessErr error) {
	if viewErr != nil || readinessErr != nil {
		if r.failureStreak < 6 {
			r.failureStreak++
		}
		return
	}
	r.failureStreak = 0
}

func (r refreshCoordinator) sequence() uint64   { return r.seq }
func (r refreshCoordinator) isPending() bool    { return r.pending }
func (r refreshCoordinator) viewComplete() bool { return r.viewDone }
func (r refreshCoordinator) failureCount() int  { return r.failureStreak }

func refreshBackoff(base time.Duration, failures int) time.Duration {
	if base <= 0 {
		base = 5 * time.Second
	}
	if failures <= 0 || base >= 30*time.Second {
		return base
	}
	interval := base
	for i := 0; i < failures && interval < 30*time.Second; i++ {
		if interval > 15*time.Second {
			return 30 * time.Second
		}
		interval *= 2
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}
