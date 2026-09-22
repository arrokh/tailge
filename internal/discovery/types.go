package discovery

import (
	"time"

	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/target"
)

type MetadataQuality string

const (
	MetadataComplete MetadataQuality = "complete"
	MetadataPartial  MetadataQuality = "partial"
	MetadataUnknown  MetadataQuality = "unknown"
)

type Listener struct {
	ID           string              `json:"id"`
	Target       target.Target       `json:"target"`
	Name         string              `json:"name"`
	PID          int                 `json:"pid,omitempty"`
	Process      string              `json:"process,omitempty"`
	CommandLine  string              `json:"command_line,omitempty"`
	ProcessStart string              `json:"process_start,omitempty"`
	Scope        target.NetworkScope `json:"scope"`
	Metadata     MetadataQuality     `json:"metadata"`
	FirstSeen    time.Time           `json:"first_seen"`
	LastSeen     time.Time           `json:"last_seen"`
}

type ListenerSnapshot struct {
	At            time.Time        `json:"at"`
	Authoritative bool             `json:"authoritative"`
	Stale         bool             `json:"stale,omitempty"`
	Listeners     []Listener       `json:"listeners"`
	Warnings      []string         `json:"warnings,omitempty"`
	Error         *fault.SafeError `json:"error,omitempty"`
	Source        string           `json:"source"`
}
