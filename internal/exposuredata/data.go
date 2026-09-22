package exposuredata

import (
	"fmt"
	"time"

	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/target"
)

type ExposureMode string

const (
	ExposureDisabled ExposureMode = "disabled"
	ExposureServe    ExposureMode = "serve"
	ExposureFunnel   ExposureMode = "funnel"
)

func (m ExposureMode) Valid() bool {
	return m == ExposureDisabled || m == ExposureServe || m == ExposureFunnel
}

type Ownership string

const (
	OwnershipManaged  Ownership = "managed"
	OwnershipExternal Ownership = "external"
	OwnershipUnknown  Ownership = "unknown"
)

type ExposureState string

const (
	ExposureActive      ExposureState = "active"
	ExposureInactive    ExposureState = "inactive_configured"
	ExposureUnsupported ExposureState = "unsupported"
	ExposureAmbiguous   ExposureState = "ambiguous"
	ExposureUnknown     ExposureState = "unknown"
	ExposureUnavailable ExposureState = "unavailable"
	ExposureApplying    ExposureState = "applying"
	ExposureSucceeded   ExposureState = "succeeded"
	ExposureFailed      ExposureState = "failed"
	ExposureCancelled   ExposureState = "cancelled"
	ExposureUnverified  ExposureState = "unverified"
)

type ExposureRoute struct {
	ID             string           `json:"id"`
	ProviderKey    string           `json:"provider_key,omitempty"`
	Service        string           `json:"service,omitempty"`
	Path           string           `json:"path,omitempty"`
	Target         target.Target    `json:"target"`
	Mode           ExposureMode     `json:"mode"`
	URL            string           `json:"url,omitempty"`
	Ownership      Ownership        `json:"ownership"`
	State          ExposureState    `json:"state"`
	LastSeen       time.Time        `json:"last_seen"`
	LastVerifiedAt time.Time        `json:"last_verified_at,omitempty"`
	LastError      *fault.SafeError `json:"last_error,omitempty"`
	Recommendation string           `json:"recommendation,omitempty"`
	Source         string           `json:"source,omitempty"`
	// Backend is retained internally so an exact rollback can reuse the
	// provider's original backend URL and not silently change its scheme/path.
	Backend string `json:"-"`
}

type ExposureSnapshot struct {
	At            time.Time        `json:"at"`
	Authoritative bool             `json:"authoritative"`
	Stale         bool             `json:"stale,omitempty"`
	Routes        []ExposureRoute  `json:"routes"`
	Warnings      []string         `json:"warnings,omitempty"`
	Error         *fault.SafeError `json:"error,omitempty"`
	Source        string           `json:"source"`
}

type OperationEvent struct {
	OperationID string          `json:"operation_id"`
	Phase       string          `json:"phase"`
	At          time.Time       `json:"at"`
	Duration    string          `json:"duration,omitempty"`
	Target      target.Target   `json:"target"`
	Mode        ExposureMode    `json:"mode"`
	Capability  string          `json:"capability,omitempty"`
	State       ExposureState   `json:"state"`
	ErrorCode   fault.ErrorCode `json:"error_code,omitempty"`
}

type OperationReceipt struct {
	ID         string           `json:"id"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at"`
	Command    string           `json:"command,omitempty"`
	ExitCode   int              `json:"exit_code"`
	Stdout     string           `json:"stdout,omitempty"`
	Stderr     string           `json:"stderr,omitempty"`
	Verified   bool             `json:"verified"`
	Error      *fault.SafeError `json:"error,omitempty"`
}

func InactiveRecommendation(t target.Target) string {
	return fmt.Sprintf("No listener is currently accepting connections on %s. Start the service and refresh, or disable this stale exposure after reviewing its public/private scope.", t.String())
}
