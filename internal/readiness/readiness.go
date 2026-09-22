package readiness

import (
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
)

type ReadinessStatus string

const (
	ReadinessReady    ReadinessStatus = "ready"
	ReadinessReadOnly ReadinessStatus = "read_only"
	ReadinessNotReady ReadinessStatus = "not_ready"
	ReadinessUnknown  ReadinessStatus = "unknown"
)

type ReadinessCheck struct {
	Name        string          `json:"name"`
	Status      ReadinessStatus `json:"status"`
	Message     string          `json:"message"`
	Remediation string          `json:"remediation,omitempty"`
	CheckedAt   time.Time       `json:"checked_at"`
}

type ModeReadiness struct {
	Mode   exposuredata.ExposureMode `json:"mode"`
	Status ReadinessStatus           `json:"status"`
	Checks []ReadinessCheck          `json:"checks"`
	Probe  bool                      `json:"probe_verified"`
	Remote string                    `json:"remote_reachability"`
}

type Readiness struct {
	At        time.Time        `json:"at"`
	Binary    string           `json:"binary,omitempty"`
	Version   string           `json:"version,omitempty"`
	Daemon    ReadinessStatus  `json:"daemon"`
	Identity  ReadinessStatus  `json:"identity"`
	Connected ReadinessStatus  `json:"connected"`
	Status    ReadinessStatus  `json:"status"`
	Checks    []ReadinessCheck `json:"checks"`
	Modes     []ModeReadiness  `json:"modes"`
	Warnings  []string         `json:"warnings,omitempty"`
}
