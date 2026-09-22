package tailscale

import (
	"context"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/model"
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
		return "", model.NewError(model.ErrDependency, "tailscale", "tailscale executable was not found", true, "unavailable", "Install Tailscale and retry.")
	}
	result, err := a.run(ctx, "version")
	if err != nil {
		return "", a.commandError("version", result, err)
	}
	if result.Truncated {
		return "", model.NewError(model.ErrUnknown, "tailscale", "version output was truncated", true, "partial", "Retry with a healthy Tailscale installation.")
	}
	if strings.TrimSpace(result.Stderr) != "" {
		return "", model.NewError(model.ErrUnknown, "tailscale", "version emitted diagnostics: "+redact(strings.TrimSpace(result.Stderr)), true, "unknown", "Retry with a healthy Tailscale installation.")
	}
	line := strings.TrimSpace(sanitizeText(strings.SplitN(result.Stdout, "\n", 2)[0]))
	if line == "" {
		return "", model.NewError(model.ErrUnknown, "tailscale", "tailscale version output was empty", true, "unknown", "Retry after checking the Tailscale installation.")
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
		return "", model.NewError(model.ErrUnknown, "tailscale", command+" help output was truncated", true, "partial", "Retry before changing exposure capabilities.")
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

func (a *Adapter) Readiness(ctx context.Context, options ReadinessOptions) (model.Readiness, error) {
	now := a.now()
	readiness := model.Readiness{At: now, Status: model.ReadinessUnknown, Checks: []model.ReadinessCheck{}, Modes: []model.ModeReadiness{}}
	if a.binaryPath() == "" {
		check := model.ReadinessCheck{Name: "binary", Status: model.ReadinessNotReady, Message: "tailscale executable was not found", Remediation: "Install Tailscale and retry.", CheckedAt: now}
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = model.ReadinessNotReady
		return readiness, model.NewError(model.ErrDependency, "tailscale", check.Message, true, "unavailable", check.Remediation)
	}
	readiness.Binary = a.binaryPath()
	version, versionErr := a.Version(ctx)
	if versionErr != nil {
		check := checkFromError("version", versionErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, versionErr
	}
	readiness.Version = version
	status, statusErr := a.Status(ctx)
	if statusErr != nil {
		check := checkFromError("status", statusErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, statusErr
	}
	backendState := sanitizeText(status.BackendState)
	daemonCheck := statusValue(backendState == "Running", "Tailscale control service is running", "Tailscale backend is "+backendState, now)
	identityCheck := statusValue(status.HaveNodeKey && (status.Self.HostName != "" || status.Self.DNSName != ""), "node identity is available", "node identity is missing", now)
	connectedCheck := statusValue(status.BackendState == "Running" && hasValidNodeAddress(status) && status.Self.Online, "node is connected", "node is not currently connected", now)
	readiness.Daemon, readiness.Identity, readiness.Connected = daemonCheck.Status, identityCheck.Status, connectedCheck.Status
	readiness.Checks = append(readiness.Checks, daemonCheck, identityCheck, connectedCheck)
	caps, capErr := a.Capabilities(ctx)
	if capErr != nil {
		check := checkFromError("capabilities", capErr, now)
		readiness.Checks = append(readiness.Checks, check)
		readiness.Modes = modeFailures(check.Status, check.Message, check.Remediation, now)
		readiness.Status = check.Status
		return readiness, capErr
	}
	baseStatus := baseReadiness(readiness)
	for _, mode := range []model.ExposureMode{model.ExposureServe, model.ExposureFunnel} {
		modeReady := model.ModeReadiness{Mode: mode, Status: model.ReadinessUnknown, Checks: []model.ReadinessCheck{}, Remote: "not checked"}
		supported, exact, probeVersion := false, false, ""
		if mode == model.ExposureServe {
			supported, exact, probeVersion = caps.Serve, caps.ExactServe, options.ServeProbeVersion
		} else {
			supported, exact, probeVersion = caps.Funnel, caps.ExactFunnel, options.FunnelProbeVersion
		}
		if baseStatus != model.ReadinessReady {
			modeReady.Status = baseStatus
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: "node readiness", Status: baseStatus, Message: "Tailscale node is not ready for exposure changes", Remediation: "Fix the daemon, identity, and connection checks, then retry.", CheckedAt: now})
		} else if !supported {
			modeReady.Status = model.ReadinessNotReady
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " capability", Status: model.ReadinessNotReady, Message: string(mode) + " is not supported by the installed Tailscale CLI", Remediation: "Use a supported Tailscale version and policy.", CheckedAt: now})
		} else if !exact {
			modeReady.Status = model.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " exact route operations", Status: model.ReadinessReadOnly, Message: "exact route replacement/removal is not available", Remediation: "Tailge will not use a broad reset.", CheckedAt: now})
		} else if mode == model.ExposureFunnel {
			// Funnel uses the exact listener flags discovered from the installed
			// CLI. The mutation path performs its own bounded set/read-after-write
			// verification, so a manual disposable public probe is not required to
			// unlock the action. Public exposure still requires explicit confirmation.
			modeReady.Status = model.ReadinessReady
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: "funnel exact route capability", Status: model.ReadinessReady, Message: "exact Funnel listener operations are available; each action is verified after mutation", CheckedAt: now})
		} else if probeVersion == "" || probeVersion != version {
			modeReady.Status = model.ReadinessReadOnly
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: model.ReadinessReadOnly, Message: "this adapter version has not passed an explicit compatibility probe", Remediation: "Run `tailge doctor --tailscale --probe ...` with a disposable listener.", CheckedAt: now})
		} else {
			modeReady.Status = model.ReadinessReady
			modeReady.Probe = true
			modeReady.Checks = append(modeReady.Checks, model.ReadinessCheck{Name: string(mode) + " compatibility probe", Status: model.ReadinessReady, Message: "set, verify, and cleanup evidence matches this adapter version", CheckedAt: now})
		}
		readiness.Modes = append(readiness.Modes, modeReady)
	}
	readiness.Status = aggregateReadiness(readiness.Modes, readiness.Checks)
	// Mode-specific readiness is returned in the report. Funnel can be ready
	// from exact CLI capabilities without a manual public probe; its public
	// confirmation and per-operation verification gates remain mandatory.
	return readiness, nil
}

func modeFailures(status model.ReadinessStatus, message, remediation string, now time.Time) []model.ModeReadiness {
	modes := make([]model.ModeReadiness, 0, 2)
	for _, mode := range []model.ExposureMode{model.ExposureServe, model.ExposureFunnel} {
		modes = append(modes, model.ModeReadiness{Mode: mode, Status: status, Remote: "not checked", Checks: []model.ReadinessCheck{{Name: "node readiness", Status: status, Message: message, Remediation: remediation, CheckedAt: now}}})
	}
	return modes
}

func baseReadiness(readiness model.Readiness) model.ReadinessStatus {
	statuses := []model.ReadinessStatus{readiness.Daemon, readiness.Identity, readiness.Connected}
	for _, status := range statuses {
		if status == model.ReadinessUnknown {
			return model.ReadinessUnknown
		}
	}
	for _, status := range statuses {
		if status != model.ReadinessReady {
			return model.ReadinessNotReady
		}
	}
	return model.ReadinessReady
}

func checkFromError(name string, err error, now time.Time) model.ReadinessCheck {
	app := model.AsAppError(err)
	return model.ReadinessCheck{Name: name, Status: statusFromError(app), Message: app.Message, Remediation: app.Remediation, CheckedAt: now}
}

func statusFromError(err *model.AppError) model.ReadinessStatus {
	if err == nil {
		return model.ReadinessUnknown
	}
	switch err.Code {
	case model.ErrDependency, model.ErrPermission, model.ErrOperation:
		return model.ReadinessNotReady
	default:
		return model.ReadinessUnknown
	}
}

func statusValue(ok bool, success, failure string, now time.Time) model.ReadinessCheck {
	if ok {
		return model.ReadinessCheck{Name: "status", Status: model.ReadinessReady, Message: success, CheckedAt: now}
	}
	return model.ReadinessCheck{Name: "status", Status: model.ReadinessNotReady, Message: failure, Remediation: "Fix Tailscale setup and retry.", CheckedAt: now}
}

func aggregateReadiness(modes []model.ModeReadiness, checks []model.ReadinessCheck) model.ReadinessStatus {
	if len(modes) == 0 {
		return model.ReadinessUnknown
	}
	allReady := true
	anyNotReady := false
	anyUnknown := false
	for _, c := range checks {
		if c.Status == model.ReadinessNotReady {
			anyNotReady = true
		}
		if c.Status == model.ReadinessUnknown {
			anyUnknown = true
		}
	}
	for _, mode := range modes {
		if mode.Status != model.ReadinessReady {
			allReady = false
		}
		if mode.Status == model.ReadinessNotReady {
			anyNotReady = true
		}
		if mode.Status == model.ReadinessUnknown {
			anyUnknown = true
		}
	}
	if allReady && !anyNotReady && !anyUnknown {
		return model.ReadinessReady
	}
	if anyUnknown {
		return model.ReadinessUnknown
	}
	if anyNotReady {
		return model.ReadinessNotReady
	}
	return model.ReadinessReadOnly
}
