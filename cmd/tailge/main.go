package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/probe"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/tui"
)

const schemaVersion = 1

type app struct {
	config     config.Manager
	discoverer *discovery.OSDiscoverer
	tailscale  *tailscale.Adapter
}

func newApp() app {
	r := runner.ExecRunner{MaxOutput: runner.DefaultMaxOutput}
	d := discovery.New(r)
	t := tailscale.New(r)
	manager, err := config.NewManager("")
	if err != nil {
		// NewManager errors are handled by commands that need persistence. Keep
		// an empty manager so scan can still work and report the failure there.
		manager = config.Manager{}
	}
	return app{config: manager, discoverer: d, tailscale: t}
}

type response struct {
	SchemaVersion int               `json:"schema_version"`
	Data          any               `json:"data,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
	Errors        []model.SafeError `json:"errors,omitempty"`
}

type exposureStatusData struct {
	View      exposure.View   `json:"view"`
	Readiness model.Readiness `json:"readiness"`
}

func main() {
	os.Exit(newApp().run(os.Args[1:], os.Stdout, os.Stderr))
}

func (a app) run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		if !isTTY(os.Stdin) || !isTTY(os.Stdout) {
			printError(stderr, model.NewError(model.ErrInvalidInput, "cli", "tailge requires a TTY for the interactive interface", false, "non_interactive", "Use `tailge scan`, `tailge exposure status`, or another CLI command."))
			return 2
		}
		return tui.Run(os.Stdin, stdout, stderr, a.discoverer, a.tailscale, a.config)
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintln(stdout, "tailge dev")
		return 0
	case "scan":
		return a.scan(args[1:], stdout, stderr)
	case "exposure":
		return a.exposure(args[1:], stdout, stderr)
	case "config":
		return a.configCommand(args[1:], stdout, stderr)
	case "doctor":
		return a.doctor(args[1:], stdout, stderr)
	case "completion":
		return completion(args[1:], stdout, stderr)
	default:
		printError(stderr, model.NewError(model.ErrInvalidInput, "cli", "unknown command "+strconv.Quote(args[0]), false, "invalid", "Run `tailge --help` for available commands."))
		return 2
	}
}

func (a app) scan(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseFlagValues(args, map[string]bool{"json": false})
	if err != nil {
		if wantsJSON(args) {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	if len(parsed.positionals) != 0 {
		return reportMaybeJSON(stdout, stderr, invalidArgs("scan does not accept positional arguments"), parsed.bools["json"])
	}
	if a.discoverer == nil {
		return reportMaybeJSON(stdout, stderr, model.NewError(model.ErrDependency, "discovery", "listener discovery is unavailable", true, "unavailable", "Retry with a supported platform adapter."), parsed.bools["json"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	snapshot, scanErr := a.discoverer.List(ctx)
	if parsed.bools["json"] {
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: snapshot, Warnings: snapshot.Warnings, Errors: errorsFrom(scanErr, snapshot.Error)})
	}
	printListeners(stdout, snapshot.Listeners)
	for _, warning := range snapshot.Warnings {
		fmt.Fprintln(stderr, "warning:", warning)
	}
	if scanErr != nil {
		printError(stderr, scanErr)
		return model.AsAppError(scanErr).Exit
	}
	return 0
}

func (a app) exposure(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return reportError(stderr, invalidArgs("exposure requires status, serve, funnel, or disable"))
	}
	subcommand := args[0]
	if subcommand == "status" {
		return a.exposureStatus(args[1:], stdout, stderr)
	}
	if subcommand != "serve" && subcommand != "funnel" && subcommand != "disable" {
		return reportError(stderr, invalidArgs("unknown exposure command "+strconv.Quote(subcommand)))
	}
	parsed, err := parseFlagValues(args[1:], map[string]bool{"confirm-public": false, "confirm-external": false, "json": false, "address": true, "protocol": true, "mode": true})
	if err != nil {
		if wantsJSON(args[1:]) {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	if len(parsed.positionals) != 1 {
		return reportMaybeJSON(stdout, stderr, invalidArgs("exposure "+subcommand+" requires exactly one target"), parsed.bools["json"])
	}
	target, err := parseTarget(parsed.positionals[0], parsed.values["address"], parsed.values["protocol"])
	if err != nil {
		return reportMaybeJSON(stdout, stderr, model.WrapError(model.ErrInvalidInput, "cli", err.Error(), false, "invalid", "Use address:port or a port with --address.", err), parsed.bools["json"])
	}
	unlock, err := exposure.AcquireMutationLock(context.Background(), a.config.Path+".exposure.lock")
	if err != nil {
		if parsed.bools["json"] {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	defer unlock()
	cfg, _, err := a.config.Ensure(context.Background())
	if err != nil {
		if parsed.bools["json"] {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	controller := exposure.NewController(a.discoverer, a.tailscale)
	controller.MutationLockPath = a.config.Path + ".exposure.lock"
	controller.ReadinessOptions = tailscale.ReadinessOptions{ServeProbeVersion: cfg.ServeProbeVersion, FunnelProbeVersion: cfg.FunnelProbeVersion}
	mode := model.ExposureDisabled
	if subcommand == "serve" {
		mode = model.ExposureServe
	}
	if subcommand == "funnel" {
		mode = model.ExposureFunnel
	}
	selectedRouteMode := model.ExposureDisabled
	if parsed.values["mode"] != "" {
		if subcommand != "disable" {
			return reportMaybeJSON(stdout, stderr, invalidArgs("--mode is only valid with exposure disable"), parsed.bools["json"])
		}
		selectedRouteMode = model.ExposureMode(parsed.values["mode"])
		if selectedRouteMode != model.ExposureServe && selectedRouteMode != model.ExposureFunnel {
			return reportMaybeJSON(stdout, stderr, invalidArgs("--mode must be serve or funnel when disabling"), parsed.bools["json"])
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.OperationTimeout)
	defer cancel()
	var receipt model.OperationReceipt
	var applyErr error
	if subcommand == "disable" && selectedRouteMode != model.ExposureDisabled {
		receipt, applyErr = controller.ApplyRouteMode(ctx, target, selectedRouteMode, parsed.bools["confirm-external"], cfg.OperationTimeout)
	} else {
		receipt, applyErr = controller.Apply(ctx, target, mode, parsed.bools["confirm-public"], parsed.bools["confirm-external"], cfg.OperationTimeout)
	}
	if parsed.bools["json"] {
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: receipt, Errors: errorsFrom(applyErr, receipt.Error)})
	}
	if applyErr != nil {
		printError(stderr, applyErr)
		return model.AsAppError(applyErr).Exit
	}
	if receipt.Verified {
		fmt.Fprintf(stdout, "operation %s verified\n", receipt.ID)
	} else {
		fmt.Fprintf(stdout, "operation %s completed\n", receipt.ID)
	}
	return 0
}

func (a app) exposureStatus(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseFlagValues(args, map[string]bool{"json": false})
	if err != nil {
		if wantsJSON(args) {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	if len(parsed.positionals) != 0 {
		err := invalidArgs("exposure status does not accept positional arguments")
		if parsed.bools["json"] {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	controller := exposure.NewController(a.discoverer, a.tailscale)
	view, refreshErr := controller.Refresh(ctx)
	cfg, _, _, cfgErr := a.config.Load(ctx)
	if cfgErr != nil {
		cfg = config.Defaults()
	}
	readiness := model.Readiness{At: time.Now(), Status: model.ReadinessUnknown}
	var readinessErr error
	if a.tailscale == nil {
		readinessErr = model.NewError(model.ErrDependency, "cli", "Tailscale provider is unavailable", true, "unavailable", "Configure Tailscale and retry.")
	} else {
		readiness, readinessErr = a.tailscale.Readiness(ctx, tailscale.ReadinessOptions{ServeProbeVersion: cfg.ServeProbeVersion, FunnelProbeVersion: cfg.FunnelProbeVersion})
	}
	data := exposureStatusData{View: view, Readiness: readiness}
	if parsed.bools["json"] {
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: data, Warnings: view.Warnings, Errors: errorsFrom(refreshErr, view.Exposures.Error, view.Listeners.Error, cfgErr, readinessErr)})
	}
	if refreshErr != nil && len(view.Items) == 0 {
		printError(stderr, refreshErr)
		return model.AsAppError(refreshErr).Exit
	}
	printView(stdout, view)
	printReadiness(stdout, readiness)
	for _, warning := range view.Warnings {
		fmt.Fprintln(stderr, "warning:", warning)
	}
	if refreshErr != nil {
		return model.AsAppError(refreshErr).Exit
	}
	if readinessErr != nil {
		printError(stderr, readinessErr)
		return model.AsAppError(readinessErr).Exit
	}
	if cfgErr != nil {
		printError(stderr, cfgErr)
		return model.AsAppError(cfgErr).Exit
	}
	return 0
}

func (a app) configCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return reportError(stderr, invalidArgs("config requires path, show, set, or validate"))
	}
	subcommand := args[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch subcommand {
	case "path":
		if len(args) != 1 {
			return reportError(stderr, invalidArgs("config path accepts no arguments"))
		}
		fmt.Fprintln(stdout, a.config.Path)
		return 0
	case "show":
		if len(args) != 1 {
			return reportError(stderr, invalidArgs("config show accepts no arguments"))
		}
		cfg, warnings, _, err := a.config.Load(ctx)
		if err != nil {
			printError(stderr, err)
			return model.AsAppError(err).Exit
		}
		for _, warning := range warnings {
			fmt.Fprintln(stderr, "warning:", warning)
		}
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: cfg, Warnings: warnings})
	case "validate":
		if len(args) != 1 {
			return reportError(stderr, invalidArgs("config validate accepts no arguments"))
		}
		_, warnings, missing, err := a.config.Load(ctx)
		if err != nil {
			printError(stderr, err)
			return model.AsAppError(err).Exit
		}
		if missing {
			fmt.Fprintln(stdout, "config is valid (defaults; file is not created by this command)")
		} else {
			fmt.Fprintln(stdout, "config is valid")
		}
		for _, warning := range warnings {
			fmt.Fprintln(stderr, "warning:", warning)
		}
		return 0
	case "set":
		if len(args) != 3 {
			return reportError(stderr, invalidArgs("config set requires key and value"))
		}
		cfg, err := a.config.Set(ctx, args[1], args[2])
		if err != nil {
			printError(stderr, err)
			return model.AsAppError(err).Exit
		}
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: cfg})
	default:
		return reportError(stderr, invalidArgs("unknown config command "+strconv.Quote(subcommand)))
	}
}

func (a app) doctor(args []string, stdout, stderr io.Writer) int {
	parsed, err := parseFlagValues(args, map[string]bool{"tailscale": false, "json": false, "probe": true, "target": true, "confirm-test-route": false, "confirm-public": false, "self-test-listener": false})
	if err != nil {
		if wantsJSON(args) {
			return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
		}
		return reportError(stderr, err)
	}
	if !parsed.bools["tailscale"] {
		return reportMaybeJSON(stdout, stderr, invalidArgs("doctor currently requires --tailscale"), parsed.bools["json"] || wantsJSON(args))
	}
	if len(parsed.positionals) != 0 {
		return reportMaybeJSON(stdout, stderr, invalidArgs("doctor does not accept positional arguments"), parsed.bools["json"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if a.tailscale == nil {
		err := model.NewError(model.ErrDependency, "doctor", "Tailscale provider is unavailable", true, "unavailable", "Configure Tailscale and retry.")
		return reportDoctor(stdout, stderr, model.Readiness{At: time.Now(), Status: model.ReadinessUnknown}, err, parsed.bools["json"])
	}
	cfg, _, cfgErr := a.config.Ensure(ctx)
	if cfgErr != nil {
		cfg = config.Defaults()
	}
	readiness, readErr := a.tailscale.Readiness(ctx, tailscale.ReadinessOptions{ServeProbeVersion: cfg.ServeProbeVersion, FunnelProbeVersion: cfg.FunnelProbeVersion})
	if cfgErr != nil {
		return reportDoctor(stdout, stderr, readiness, cfgErr, parsed.bools["json"])
	}
	if readErr != nil {
		// A compatibility probe is itself a bounded mutation. Do not use it to
		// bypass a failed base readiness check or to mutate while the provider's
		// node state is unknown.
		return reportDoctor(stdout, stderr, readiness, readErr, parsed.bools["json"])
	}
	if parsed.bools["self-test-listener"] && parsed.values["probe"] == "" {
		return reportDoctor(stdout, stderr, readiness, invalidArgs("--self-test-listener requires --probe serve or funnel"), parsed.bools["json"])
	}
	if parsed.values["probe"] != "" {
		if readiness.Daemon != model.ReadinessReady || readiness.Identity != model.ReadinessReady || readiness.Connected != model.ReadinessReady {
			err := model.NewError(model.ErrDependency, "doctor", "compatibility probe requires a ready Tailscale node", false, "not_ready", "Fix the daemon, identity, and connection checks before probing route mutations.")
			return reportDoctor(stdout, stderr, readiness, err, parsed.bools["json"])
		}
		if !parsed.bools["confirm-test-route"] {
			return reportDoctor(stdout, stderr, readiness, model.NewError(model.ErrUnsafe, "doctor", "compatibility probe requires --confirm-test-route", false, "not_confirmed", "Supply a disposable listener target and explicit confirmation."), parsed.bools["json"])
		}
		var (
			target       model.Target
			testListener net.Listener
		)
		if parsed.bools["self-test-listener"] {
			if parsed.values["target"] != "" {
				return reportDoctor(stdout, stderr, readiness, invalidArgs("--target cannot be combined with --self-test-listener"), parsed.bools["json"])
			}
			testListener, target, err = openSelfTestListener()
			if err != nil {
				return reportDoctor(stdout, stderr, readiness, model.WrapError(model.ErrDependency, "doctor", "could not open a disposable loopback listener", true, "unavailable", "Choose an available local TCP interface and retry.", err), parsed.bools["json"])
			}
			defer testListener.Close()
		} else {
			if parsed.values["target"] == "" {
				return reportDoctor(stdout, stderr, readiness, invalidArgs("--target is required for a compatibility probe (or use --self-test-listener)"), parsed.bools["json"])
			}
			var parseErr error
			target, parseErr = model.ParseTarget(parsed.values["target"], "tcp")
			if parseErr != nil {
				return reportDoctor(stdout, stderr, readiness, model.WrapError(model.ErrInvalidInput, "doctor", parseErr.Error(), false, "invalid", "Provide a disposable address:port.", parseErr), parsed.bools["json"])
			}
		}
		probeRunner := probe.Runner{Discoverer: a.discoverer, Provider: a.tailscale, Config: a.config, MutationLockPath: a.config.Path + ".exposure.lock"}
		probeErr := probeRunner.Run(ctx, &cfg, target, model.ExposureMode(parsed.values["probe"]), parsed.bools["confirm-public"])
		if probeErr != nil {
			return reportDoctor(stdout, stderr, readiness, probeErr, parsed.bools["json"])
		}
		readiness, readErr = a.tailscale.Readiness(ctx, tailscale.ReadinessOptions{ServeProbeVersion: cfg.ServeProbeVersion, FunnelProbeVersion: cfg.FunnelProbeVersion})
	}
	if readErr != nil {
		return reportDoctor(stdout, stderr, readiness, readErr, parsed.bools["json"])
	}
	code := readinessExit(readiness)
	return reportDoctorWithCode(stdout, stderr, readiness, nil, parsed.bools["json"], code)
}

func openSelfTestListener() (net.Listener, model.Target, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, model.Target{}, err
	}
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcpAddress.Port < 1 || tcpAddress.Port > 65535 {
		_ = listener.Close()
		return nil, model.Target{}, errors.New("self-test listener returned an invalid TCP address")
	}
	return listener, model.Target{Address: "127.0.0.1", Port: tcpAddress.Port, Protocol: "tcp"}, nil
}

func reportDoctor(stdout, stderr io.Writer, readiness model.Readiness, err error, jsonOutput bool) int {
	return reportDoctorWithCode(stdout, stderr, readiness, err, jsonOutput, errorCode(err, readinessExit(readiness)))
}
func reportDoctorWithCode(stdout, stderr io.Writer, readiness model.Readiness, err error, jsonOutput bool, code int) int {
	if err == nil && code != 0 {
		err = readinessStatusError(readiness, code)
	}
	if jsonOutput {
		jsonCode := writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Data: readiness, Errors: errorsFrom(err)})
		if jsonCode != 0 {
			return jsonCode
		}
		return code
	}
	printReadiness(stdout, readiness)
	if err != nil {
		printError(stderr, err)
	}
	return code
}

func readinessStatusError(r model.Readiness, code int) error {
	if code == model.ErrVerification.ExitCode() || r.Status == model.ReadinessUnknown {
		return model.NewError(model.ErrVerification, "doctor", "Tailscale readiness is unknown", true, "unknown", "Retry `tailge doctor --tailscale` after checking Tailscale status.")
	}
	return model.NewError(model.ErrDependency, "doctor", "Tailscale readiness is "+string(r.Status), true, string(r.Status), "Complete the reported readiness checks before enabling exposure.")
}

func readinessExit(r model.Readiness) int {
	if len(r.Modes) == 0 {
		return model.ErrVerification.ExitCode()
	}
	for _, mode := range r.Modes {
		if mode.Status == model.ReadinessUnknown {
			return model.ErrVerification.ExitCode()
		}
		if mode.Status == model.ReadinessNotReady {
			return model.ErrDependency.ExitCode()
		}
		if mode.Status != model.ReadinessReady {
			return model.ErrDependency.ExitCode()
		}
	}
	if r.Status == model.ReadinessUnknown {
		return model.ErrVerification.ExitCode()
	}
	if r.Status != model.ReadinessReady {
		return model.ErrDependency.ExitCode()
	}
	return 0
}

func errorCode(err error, fallback int) int {
	if err == nil {
		return fallback
	}
	return model.AsAppError(err).Exit
}
func errorsFrom(values ...any) []model.SafeError {
	result := []model.SafeError{}
	seen := map[string]bool{}
	for _, value := range values {
		var safe model.SafeError
		switch value := value.(type) {
		case nil:
			continue
		case error:
			appErr := model.AsAppError(value)
			if appErr == nil {
				continue
			}
			safe = appErr.Safe()
		case *model.SafeError:
			if value == nil {
				continue
			}
			safe = *value
		case model.SafeError:
			safe = value
		default:
			continue
		}
		key := string(safe.Code) + safe.Message
		if !seen[key] {
			result = append(result, safe)
			seen[key] = true
		}
	}
	return result
}
func reportError(w io.Writer, err error) int { printError(w, err); return model.AsAppError(err).Exit }
func reportMaybeJSON(stdout, stderr io.Writer, err error, jsonOutput bool) int {
	if jsonOutput {
		return writeJSON(stdout, stderr, response{SchemaVersion: schemaVersion, Errors: errorsFrom(err)})
	}
	return reportError(stderr, err)
}
func completion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return reportError(stderr, invalidArgs("completion requires bash, zsh, or fish"))
	}
	switch args[0] {
	case "bash":
		fmt.Fprintln(stdout, `_tailge() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  COMPREPLY=( $(compgen -W "scan exposure config doctor version help completion" -- "$cur") )
}
complete -F _tailge tailge`)
	case "zsh":
		fmt.Fprintln(stdout, `#compdef tailge
_arguments '1:command:(scan exposure config doctor version help completion)'`)
	case "fish":
		fmt.Fprintln(stdout, `complete -c tailge -f -n '__fish_use_subcommand' -a 'scan exposure config doctor version help completion'`)
	default:
		return reportError(stderr, invalidArgs("completion requires bash, zsh, or fish"))
	}
	return 0
}

func invalidArgs(message string) *model.AppError {
	return model.NewError(model.ErrInvalidInput, "cli", message, false, "invalid", "Run `tailge --help` for command syntax.")
}

func writeJSON(stdout, stderr io.Writer, value response) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		printError(stderr, model.WrapError(model.ErrOperation, "cli", "cannot encode JSON response", false, "failed", "Retry the command.", err))
		return 6
	}
	_, _ = fmt.Fprintln(stdout, string(data))
	if len(value.Errors) > 0 {
		return value.Errors[0].ExitCode
	}
	return 0
}

func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return true
		}
	}
	return false
}

// flagValues parses the same syntax while retaining values.
type flagValues struct {
	bools       map[string]bool
	values      map[string]string
	positionals []string
}

func parseFlagValues(args []string, allowed map[string]bool) (flagValues, error) {
	result := flagValues{bools: map[string]bool{}, values: map[string]string{}, positionals: []string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			result.positionals = append(result.positionals, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		value, hasEqual := "", false
		if index := strings.IndexByte(name, '='); index >= 0 {
			value, hasEqual, name = name[index+1:], true, name[:index]
		}
		accepts, ok := allowed[name]
		if !ok {
			return result, invalidArgs("unknown flag " + strconv.Quote("--"+name))
		}
		if accepts {
			if _, duplicate := result.values[name]; duplicate {
				return result, invalidArgs("flag --" + name + " was provided more than once")
			}
			if !hasEqual {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					return result, invalidArgs("flag --" + name + " requires a value")
				}
				i++
				value = args[i]
			}
			result.values[name] = value
		} else if hasEqual {
			return result, invalidArgs("flag --" + name + " does not accept a value")
		} else {
			if result.bools[name] {
				return result, invalidArgs("flag --" + name + " was provided more than once")
			}
			result.bools[name] = true
		}
	}
	return result, nil
}

func parseTarget(raw, address, protocol string) (model.Target, error) {
	if strings.IndexFunc(address, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' || r == '"' }) >= 0 {
		return model.Target{}, fmt.Errorf("address contains unsupported control or quoting characters")
	}
	if address != "" && !strings.Contains(raw, ":") {
		raw = net.JoinHostPort(model.NormalizeAddress(address), raw)
	}
	return model.ParseTarget(raw, protocol)
}

func isTTY(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
func printError(w io.Writer, err error) {
	appErr := model.AsAppError(err)
	fmt.Fprintf(w, "error [%s]: %s\n", appErr.Code, appErr.Message)
	if appErr.Remediation != "" {
		fmt.Fprintln(w, "next:", appErr.Remediation)
	}
}
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "tailge — local service exposure TUI")
	fmt.Fprintln(w, "usage: tailge | tailge scan [--json] | tailge doctor --tailscale [--json]")
	fmt.Fprintln(w, "       tailge doctor --tailscale --probe serve|funnel --target ADDRESS:PORT --confirm-test-route [--confirm-public]")
	fmt.Fprintln(w, "       tailge doctor --tailscale --probe serve|funnel --self-test-listener --confirm-test-route [--confirm-public]")
	fmt.Fprintln(w, "       tailge exposure status [--json]")
	fmt.Fprintln(w, "       tailge exposure serve|funnel|disable TARGET [--address ADDR] [--mode serve|funnel] [--confirm-public] [--confirm-external] [--json]")
	fmt.Fprintln(w, "       tailge config path|show|set KEY VALUE|validate | tailge completion bash|zsh|fish")
}
func printListeners(w io.Writer, listeners []model.Listener) {
	fmt.Fprintln(w, "SERVICE\tTARGET\tPID\tSCOPE\tMETADATA")
	for _, l := range listeners {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", valueOr(l.Name, "unknown"), l.Target.String(), l.PID, l.Scope, l.Metadata)
	}
}
func appendDetail(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func printView(w io.Writer, view exposure.View) {
	fmt.Fprintln(w, "SERVICE/TARGET\tMODE\tSTATE\tOWNERSHIP\tDETAIL")
	for _, item := range view.Items {
		detail := item.Warning
		if len(item.Routes) > 0 && item.Routes[0].URL != "" {
			detail = appendDetail(detail, "url="+item.Routes[0].URL)
		}
		if item.OperationState != "" {
			detail = appendDetail(detail, "operation="+string(item.OperationState))
		}
		if item.DesiredMode != "" && item.DesiredMode != item.Mode {
			detail = appendDetail(detail, "desired="+string(item.DesiredMode))
		}
		if item.Listener != nil {
			route := "disabled"
			own := "-"
			if len(item.Routes) > 0 {
				route = string(item.Routes[0].Mode)
				own = string(item.Routes[0].Ownership)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", valueOr(item.Listener.Name, item.Listener.Target.String()), route, item.State, own, detail)
		} else if len(item.Routes) > 0 {
			r := item.Routes[0]
			routeDetail := detail
			if item.Recommendation != "" {
				routeDetail = item.Recommendation
				if detail != "" {
					routeDetail = appendDetail(routeDetail, detail)
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Target.String(), r.Mode, item.State, r.Ownership, routeDetail)
		}
	}
}
func printReadiness(w io.Writer, r model.Readiness) {
	fmt.Fprintf(w, "Tailscale: %s\n", r.Status)
	if r.Binary != "" {
		fmt.Fprintf(w, "Binary: %s\nVersion: %s\n", r.Binary, r.Version)
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "- %s: %s — %s\n", c.Name, c.Status, c.Message)
	}
	for _, m := range r.Modes {
		fmt.Fprintf(w, "%s: %s (probe=%t, remote=%s)\n", m.Mode, m.Status, m.Probe, m.Remote)
	}
}
func valueOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
