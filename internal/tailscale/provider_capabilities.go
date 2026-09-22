package tailscale

import (
	"context"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
)

type Capabilities struct {
	Serve        bool
	Funnel       bool
	ExactServe   bool
	ExactFunnel  bool
	ServeHTTPS   bool
	ServeTCP     bool
	FunnelHTTPS  bool
	FunnelTCP    bool
	FunnelLegacy bool
	Service      bool
	Version      string
}

func (a *Adapter) Version(ctx context.Context) (string, error) {
	if a.binaryPath() == "" {
		return "", fault.NewError(fault.ErrDependency, "tailscale", "tailscale executable was not found", true, "unavailable", "Install Tailscale and retry.")
	}
	result, err := a.run(ctx, "version")
	if err != nil {
		return "", a.commandError("version", result, err)
	}
	if result.Truncated {
		return "", fault.NewError(fault.ErrUnknown, "tailscale", "version output was truncated", true, "partial", "Retry with a healthy Tailscale installation.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return "", fault.NewError(fault.ErrUnknown, "tailscale", "version emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	line := strings.TrimSpace(sanitizeText(strings.SplitN(result.Stdout, "\n", 2)[0]))
	if line == "" {
		return "", fault.NewError(fault.ErrUnknown, "tailscale", "tailscale version output was empty", true, "unknown", "Retry after checking the Tailscale installation.")
	}
	return line, nil
}

func (a *Adapter) Capabilities(ctx context.Context) (Capabilities, error) {
	version, err := a.Version(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	serve, err := a.help(ctx, "serve")
	if err != nil {
		return Capabilities{}, err
	}
	funnel, err := a.help(ctx, "funnel")
	if err != nil {
		return Capabilities{}, err
	}
	serveLower, funnelLower := strings.ToLower(serve), strings.ToLower(funnel)
	serveClear := hasSubcommand(serveLower, "clear")
	serviceFlag := hasValueFlag(serveLower, "--service") || strings.Contains(serveLower, "--service=")
	serveHTTPS := strings.Contains(serveLower, "--https")
	serveTCP := strings.Contains(serveLower, "--tcp")
	serveExactOff := exactFlagOff(serveLower)
	funnelReset := hasSubcommand(funnelLower, "reset")
	funnelLegacy := strings.Contains(funnelLower, "{on|off}") || strings.Contains(funnelLower, "{on,off}")
	funnelHTTPS := strings.Contains(funnelLower, "--https")
	funnelTCP := strings.Contains(funnelLower, "--tcp")
	// The v2 Funnel CLI accepts `tailscale funnel --https=<port> off`
	// (and the equivalent --tcp form), but its --help output omits the
	// positional `off` syntax. Recognize the typed value form; Set and Remove
	// perform bounded read-after-write verification before reporting success.
	// Legacy {on|off} syntax remains supported separately.
	funnelV2ExactOff := hasValueFlag(funnelLower, "--https") || hasValueFlag(funnelLower, "--tcp")
	funnelExactOff := exactFlagOff(funnelLower) || funnelV2ExactOff
	return Capabilities{
		Serve:        hasSubcommand(serveLower, "status"),
		Funnel:       hasSubcommand(funnelLower, "status") && (funnelReset || funnelLegacy || funnelExactOff),
		ExactServe:   (serveClear || serveExactOff) && (serveHTTPS || serveTCP),
		ExactFunnel:  funnelLegacy || (funnelExactOff && (funnelHTTPS || funnelTCP)),
		ServeHTTPS:   serveHTTPS,
		ServeTCP:     serveTCP,
		FunnelHTTPS:  funnelHTTPS,
		FunnelTCP:    funnelTCP,
		FunnelLegacy: funnelLegacy,
		Service:      serviceFlag,
		Version:      version,
	}, nil
}

func (a *Adapter) help(ctx context.Context, command string) (string, error) {
	result, err := a.run(ctx, command, "--help")
	if err != nil {
		return "", a.commandError(command+" --help", result, err)
	}
	if result.Truncated {
		return "", fault.NewError(fault.ErrUnknown, "tailscale", command+" help output was truncated", true, "partial", "Retry before changing exposure capabilities.")
	}
	// Some Tailscale CLI versions print normal help to stderr even with a zero
	// exit status. Capability parsing therefore considers both streams, while
	// still applying the bounded capture and conservative token checks above.
	return result.Stdout + "\n" + result.Stderr, nil
}

// ReadinessOptions carries optional persisted Serve compatibility evidence.
// Funnel readiness is capability-driven when exact listener flags are exposed;
// each Funnel mutation still performs bounded read-after-write verification.
type ReadinessOptions struct {
	ServeProbeVersion  string
	FunnelProbeVersion string
}

func (a *Adapter) Readiness(ctx context.Context, options ReadinessOptions) (readinessmodel.Readiness, error) {
	now := a.now()
	report := readinessmodel.Readiness{At: now, Status: readinessmodel.ReadinessUnknown, Checks: []readinessmodel.ReadinessCheck{}, Modes: []readinessmodel.ModeReadiness{}}
	if a.binaryPath() == "" {
		check := readinessmodel.ReadinessCheck{Name: "binary", Status: readinessmodel.ReadinessNotReady, Message: "tailscale executable was not found", Remediation: "Install Tailscale and retry.", CheckedAt: now}
		report.Checks = append(report.Checks, check)
		report.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		report.Status = readinessmodel.ReadinessNotReady
		return report, fault.NewError(fault.ErrDependency, "tailscale", check.Message, true, "unavailable", check.Remediation)
	}
	report.Binary = a.binaryPath()
	version, versionErr := a.Version(ctx)
	if versionErr != nil {
		check := checkFromError("version", versionErr, now)
		report.Checks = append(report.Checks, check)
		report.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		report.Status = check.Status
		return report, versionErr
	}
	report.Version = version
	status, statusErr := a.Status(ctx)
	if statusErr != nil {
		check := checkFromError("status", statusErr, now)
		report.Checks = append(report.Checks, check)
		report.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		report.Status = check.Status
		return report, statusErr
	}
	backendState := sanitizeText(status.BackendState)
	daemonCheck := statusValue(backendState == "Running", "Tailscale control service is running", "Tailscale backend is "+backendState, now)
	identityCheck := statusValue(status.HaveNodeKey && (status.Self.HostName != "" || status.Self.DNSName != ""), "node identity is available", "node identity is missing", now)
	connectedCheck := statusValue(status.BackendState == "Running" && hasValidNodeAddress(status) && status.Self.Online, "node is connected", "node is not currently connected", now)
	report.Daemon, report.Identity, report.Connected = daemonCheck.Status, identityCheck.Status, connectedCheck.Status
	report.Checks = append(report.Checks, daemonCheck, identityCheck, connectedCheck)
	caps, capErr := a.Capabilities(ctx)
	if capErr != nil {
		check := checkFromError("capabilities", capErr, now)
		report.Checks = append(report.Checks, check)
		report.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		report.Status = check.Status
		return report, capErr
	}
	baseStatus := baseReadiness(report)
	for _, mode := range []exposuredata.ExposureMode{exposuredata.ExposureServe, exposuredata.ExposureFunnel} {
		modeReady := readinessmodel.ModeReadiness{Mode: mode, Status: readinessmodel.ReadinessUnknown, Checks: []readinessmodel.ReadinessCheck{}, Remote: "not checked"}
		supported, exact, probeVersion := false, false, ""
		if mode == exposuredata.ExposureServe {
			supported, exact, probeVersion = caps.Serve, caps.ExactServe, options.ServeProbeVersion
		} else {
			supported, exact, probeVersion = caps.Funnel, caps.ExactFunnel, options.FunnelProbeVersion
		}
		if baseStatus != readinessmodel.ReadinessReady {
			modeReady.Status = baseStatus
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: "node readiness", Status: baseStatus, Message: "Tailscale node is not ready for exposure changes", Remediation: "Fix the daemon, identity, and connection checks, then retry.", CheckedAt: now})
		} else if !supported {
			modeReady.Status = readinessmodel.ReadinessNotReady
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: string(mode) + " capability", Status: readinessmodel.ReadinessNotReady, Message: string(mode) + " is not supported by the installed Tailscale CLI", Remediation: "Use a supported Tailscale version and policy.", CheckedAt: now})
		} else if !exact {
			modeReady.Status = readinessmodel.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: string(mode) + " exact route operations", Status: readinessmodel.ReadinessReadOnly, Message: "exact route replacement/removal is not available", Remediation: "Tailge will not use a broad reset.", CheckedAt: now})
		} else if mode == exposuredata.ExposureFunnel {
			// Funnel uses the exact listener flags discovered from the installed
			// CLI. The mutation path performs its own bounded set/read-after-write
			// verification, so a manual disposable public probe is not required to
			// unlock the action. Public exposure still requires explicit confirmation.
			modeReady.Status = readinessmodel.ReadinessReady
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: "funnel exact route capability", Status: readinessmodel.ReadinessReady, Message: "exact Funnel listener operations are available; each action is verified after mutation", CheckedAt: now})
		} else if probeVersion == "" || probeVersion != version {
			modeReady.Status = readinessmodel.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: readinessmodel.ReadinessReadOnly, Message: "this adapter version has not passed an explicit compatibility probe", Remediation: "Run `tailge doctor --tailscale --probe ...` with a disposable listener.", CheckedAt: now})
		} else {
			modeReady.Status = readinessmodel.ReadinessReady
			modeReady.Probe = true
			modeReady.Checks = append(modeReady.Checks, readinessmodel.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: readinessmodel.ReadinessReady, Message: "set, verify, and cleanup evidence matches this adapter version", CheckedAt: now})
		}
		report.Modes = append(report.Modes, modeReady)
	}
	report.Status = aggregateReadiness(report.Modes, report.Checks)
	// Mode-specific readiness is returned in the report. Funnel can be ready
	// from exact CLI capabilities without a manual public probe; its public
	// confirmation and per-operation verification gates remain mandatory.
	return report, nil
}

func modeFailures(status readinessmodel.ReadinessStatus, message, remediation string, now time.Time) []readinessmodel.ModeReadiness {
	modes := make([]readinessmodel.ModeReadiness, 0, 2)
	for _, mode := range []exposuredata.ExposureMode{exposuredata.ExposureServe, exposuredata.ExposureFunnel} {
		modes = append(modes, readinessmodel.ModeReadiness{Mode: mode, Status: status, Remote: "not checked", Checks: []readinessmodel.ReadinessCheck{{Name: "node readiness", Status: status, Message: message, Remediation: remediation, CheckedAt: now}}})
	}
	return modes
}

func baseReadiness(readiness readinessmodel.Readiness) readinessmodel.ReadinessStatus {
	statuses := []readinessmodel.ReadinessStatus{readiness.Daemon, readiness.Identity, readiness.Connected}
	for _, status := range statuses {
		if status == readinessmodel.ReadinessUnknown {
			return readinessmodel.ReadinessUnknown
		}
	}
	for _, status := range statuses {
		if status != readinessmodel.ReadinessReady {
			return readinessmodel.ReadinessNotReady
		}
	}
	return readinessmodel.ReadinessReady
}

func checkFromError(name string, err error, now time.Time) readinessmodel.ReadinessCheck {
	app := fault.AsAppError(err)
	return readinessmodel.ReadinessCheck{Name: name, Status: statusFromError(app), Message: app.Message, Remediation: app.Remediation, CheckedAt: now}
}

func statusFromError(err *fault.AppError) readinessmodel.ReadinessStatus {
	if err == nil {
		return readinessmodel.ReadinessUnknown
	}
	switch err.Code {
	case fault.ErrDependency, fault.ErrPermission, fault.ErrOperation:
		return readinessmodel.ReadinessNotReady
	default:
		return readinessmodel.ReadinessUnknown
	}
}

func statusValue(ok bool, success, failure string, now time.Time) readinessmodel.ReadinessCheck {
	if ok {
		return readinessmodel.ReadinessCheck{Name: "status", Status: readinessmodel.ReadinessReady, Message: success, CheckedAt: now}
	}
	return readinessmodel.ReadinessCheck{Name: "status", Status: readinessmodel.ReadinessNotReady, Message: failure, Remediation: "Fix Tailscale setup and retry.", CheckedAt: now}
}

func aggregateReadiness(modes []readinessmodel.ModeReadiness, checks []readinessmodel.ReadinessCheck) readinessmodel.ReadinessStatus {
	if len(modes) == 0 {
		return readinessmodel.ReadinessUnknown
	}
	allReady := true
	anyNotReady := false
	anyUnknown := false
	for _, c := range checks {
		if c.Status == readinessmodel.ReadinessNotReady {
			anyNotReady = true
		}
		if c.Status == readinessmodel.ReadinessUnknown {
			anyUnknown = true
		}
	}
	for _, mode := range modes {
		if mode.Status != readinessmodel.ReadinessReady {
			allReady = false
		}
		if mode.Status == readinessmodel.ReadinessNotReady {
			anyNotReady = true
		}
		if mode.Status == readinessmodel.ReadinessUnknown {
			anyUnknown = true
		}
	}
	if allReady && !anyNotReady && !anyUnknown {
		return readinessmodel.ReadinessReady
	}
	if anyUnknown {
		return readinessmodel.ReadinessUnknown
	}
	if anyNotReady {
		return readinessmodel.ReadinessNotReady
	}
	return readinessmodel.ReadinessReadOnly
}
