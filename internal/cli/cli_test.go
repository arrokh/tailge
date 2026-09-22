package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/probe"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/tui"
)

func TestParseFlagValuesSeparatesBooleanAndStringFlags(t *testing.T) {
	parsed, err := parseFlagValues([]string{"--json", "8080", "--address=127.0.0.1", "--protocol", "tcp"}, map[string]bool{"json": false, "address": true, "protocol": true})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.bools["json"] || parsed.values["address"] != "127.0.0.1" || parsed.values["protocol"] != "tcp" || len(parsed.positionals) != 1 {
		t.Fatalf("unexpected parse result: %#v", parsed)
	}
	if _, err := parseFlagValues([]string{"--json=true"}, map[string]bool{"json": false}); err == nil {
		t.Fatal("accepted a value for boolean flag")
	}
	if _, err := parseFlagValues([]string{"--address"}, map[string]bool{"address": true}); err == nil {
		t.Fatal("accepted missing flag value")
	}
	if _, err := parseFlagValues([]string{"--json", "--json"}, map[string]bool{"json": false}); err == nil {
		t.Fatal("accepted duplicate boolean flag")
	}
	if _, err := parseFlagValues([]string{"--address", "127.0.0.1", "--address", "::1"}, map[string]bool{"address": true}); err == nil {
		t.Fatal("accepted duplicate value flag")
	}
}

func TestScanHumanPreservesPartialListenersOnSourceError(t *testing.T) {
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p42\ncapi\nn127.0.0.1:8080\nP0\nTST=LISTEN\n", Stderr: "Permission denied", ExitCode: 1}, errors.New("exit status 1")
	})}
	a := app{discoverer: discoverer}
	var out, errOut bytes.Buffer
	if code := a.scan(nil, &out, &errOut); code != fault.ErrPermission.ExitCode() || !strings.Contains(out.String(), "api") || !strings.Contains(errOut.String(), "Permission") {
		t.Fatalf("partial human scan lost data: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}

func TestScanJSONPreservesSourceError(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := app{
		config: manager,
		discoverer: &discovery.OSDiscoverer{
			OS:  "darwin",
			Now: time.Now,
			Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
				return runner.Result{ExitCode: 1, Stderr: "permission denied"}, context.DeadlineExceeded
			}),
		},
	}
	var stdout, stderr bytes.Buffer
	code := app.scan([]string{"--json"}, &stdout, &stderr)
	if code == 0 || stdout.Len() == 0 {
		t.Fatalf("expected non-zero JSON scan response: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var payload response
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Errors) == 0 || payload.Data == nil {
		t.Fatalf("source error/data missing: %#v", payload)
	}
}

func TestCompletionOutputsFixedShellScript(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := completion([]string{"bash"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "complete -F _tailge tailge") {
		t.Fatalf("completion failed: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if code := completion([]string{"powershell"}, &out, &errOut); code != fault.ErrInvalidInput.ExitCode() {
		t.Fatalf("invalid shell code=%d", code)
	}
}

func TestReadinessExitAndJSONPreserveReadOnlyFailureClass(t *testing.T) {
	readiness := readinessmodel.Readiness{Status: readinessmodel.ReadinessReadOnly, Modes: []readinessmodel.ModeReadiness{{Mode: exposuredata.ExposureServe, Status: readinessmodel.ReadinessReadOnly}}}
	var stdout, stderr bytes.Buffer
	if got := reportDoctorWithCode(&stdout, &stderr, readiness, nil, true, readinessExit(readiness)); got != fault.ErrDependency.ExitCode() {
		t.Fatalf("readiness JSON exit=%d", got)
	}
	var payload response
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil || len(payload.Errors) == 0 {
		t.Fatalf("readiness JSON omitted typed failure: err=%v payload=%#v", err, payload)
	}
	if got := readinessExit(readiness); got != fault.ErrDependency.ExitCode() {
		t.Fatalf("read-only exit=%d", got)
	}
	if got := readinessExit(readinessmodel.Readiness{Status: readinessmodel.ReadinessReady}); got != fault.ErrVerification.ExitCode() {
		t.Fatalf("missing-mode exit=%d", got)
	}
}

func TestSelfTestListenerIsLoopbackAndEphemeral(t *testing.T) {
	listener, target, err := openSelfTestListener()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if target.Address != "127.0.0.1" || target.Port < 1 || target.Port > 65535 || target.Protocol != "tcp" {
		t.Fatalf("unexpected self-test target: %#v", target)
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("self-test target is invalid: %v", err)
	}
}

func TestTargetParsingRequiresAddressForPortOnlyWhenRequested(t *testing.T) {
	target, err := parseTarget("8080", "127.0.0.1", "tcp")
	if err != nil || target.Address != "127.0.0.1" || target.Port != 8080 {
		t.Fatalf("unexpected target: %#v err=%v", target, err)
	}
	if _, err := parseTarget("8080", "", "tcp"); err != nil {
		t.Fatalf("bare port should default to localhost: %v", err)
	}
	if _, err := parseTarget("8080", "127.0.0.1\n", "tcp"); err == nil {
		t.Fatal("unsafe address flag was normalized into a valid target")
	}
	ipv6, err := parseTarget("8080", "::1", "tcp")
	if err != nil || ipv6.Address != "::1" {
		t.Fatalf("IPv6 address target=%#v err=%v", ipv6, err)
	}
}

func FuzzParseFlagValuesNeverPanics(f *testing.F) {
	for _, seed := range []string{"--json 8080", "--address=127.0.0.1", "--confirm-public", "--"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = parseFlagValues(strings.Fields(text), map[string]bool{"json": false, "confirm-public": false, "address": true})
	})
}

func TestServeProbeVerifiesCleanupAndPersistsVersionEvidence(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := target.Target{Address: "127.0.0.1", Port: 39001, Protocol: "tcp"}.Normalized()
	active := false
	provider := &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch {
		case command == "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case command == "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":true}}`}, nil
		case command == "serve --help":
			return runner.Result{Stdout: "status clear --tcp"}, nil
		case command == "funnel --help":
			return runner.Result{Stdout: "status reset --tcp"}, nil
		case command == "serve status --json":
			if active {
				return runner.Result{Stdout: `{"TCP":{"39001":"127.0.0.1:39001"}}`}, nil
			}
			return runner.Result{Stdout: `{}`}, nil
		case command == "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case command == "serve --bg --yes --tcp=39001 tcp://127.0.0.1:39001":
			active = true
			return runner.Result{}, nil
		case command == "serve --bg --tcp=39001 off":
			active = false
			return runner.Result{}, nil
		default:
			return runner.Result{}, context.DeadlineExceeded
		}
	})}
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p0\ncfixture\nn127.0.0.1:39001\n"}, nil
	})}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	probeRunner := probe.Runner{Discoverer: discoverer, Provider: provider, Config: manager, MutationLockPath: manager.Path + ".exposure.lock"}
	if err := probeRunner.Run(context.Background(), &cfg, target, exposuredata.ExposureServe, false); err != nil {
		t.Fatal(err)
	}
	if active || cfg.ServeProbeVersion != "1.102.4" {
		t.Fatalf("probe did not clean up or persist evidence: active=%t cfg=%#v", active, cfg)
	}
	loaded, _, missing, err := manager.Load(context.Background())
	if err != nil || missing || loaded.ServeProbeVersion != cfg.ServeProbeVersion {
		t.Fatalf("persisted evidence missing=%t cfg=%#v err=%v", missing, loaded, err)
	}
}

func TestDoctorDoesNotProbeWhenNodeIsNotReady(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	provider := &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Stopped","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":false}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		default:
			return runner.Result{}, errors.New("unexpected provider command: " + command)
		}
	})}
	var stdout, stderr bytes.Buffer
	code := (app{config: manager, tailscale: provider}).doctor([]string{"--tailscale", "--probe", "serve", "--target", "127.0.0.1:39001", "--confirm-test-route", "--json"}, &stdout, &stderr)
	if code != fault.ErrDependency.ExitCode() {
		t.Fatalf("not-ready node returned code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, call := range calls {
		if strings.Contains(call, "--bg") {
			t.Fatalf("probe mutated a not-ready node: %v", calls)
		}
	}
}

func TestDoctorDoesNotOverwriteInvalidConfigDuringProbe(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manager.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := "version: 1\nrefresh_interval: not-a-duration\n"
	if err := os.WriteFile(manager.Path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &tailscale.Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		switch strings.Join(args, " ") {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":true}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		default:
			return runner.Result{}, errors.New("unexpected provider command: " + strings.Join(args, " "))
		}
	}), Now: time.Now}
	var stdout, stderr bytes.Buffer
	code := (app{config: manager, tailscale: provider}).doctor([]string{"--tailscale", "--probe", "serve", "--target", "127.0.0.1:39001", "--confirm-test-route", "--json"}, &stdout, &stderr)
	if code != fault.ErrConfig.ExitCode() {
		t.Fatalf("invalid config did not block probe: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(manager.Path)
	if err != nil || string(data) != original {
		t.Fatalf("invalid config was overwritten: data=%q err=%v", data, err)
	}
}

func TestProbeRouteIdentityRequiresTransportAppropriateEvidence(t *testing.T) {
	tests := []struct {
		name  string
		route exposuredata.ExposureRoute
		want  bool
	}{
		{"https with URL", exposuredata.ExposureRoute{ProviderKey: "serve:https=443", Mode: exposuredata.ExposureServe, URL: "https://dev.example.ts.net"}, true},
		{"https without URL", exposuredata.ExposureRoute{ProviderKey: "serve:https=443", Mode: exposuredata.ExposureServe}, false},
		{"tcp without URL", exposuredata.ExposureRoute{ProviderKey: "serve:tcp=443", Mode: exposuredata.ExposureServe}, true},
		{"wrong mode", exposuredata.ExposureRoute{ProviderKey: "funnel:tcp=443", Mode: exposuredata.ExposureServe}, false},
	}
	for _, test := range tests {
		if got := probe.RouteIdentityComplete(test.route); got != test.want {
			t.Errorf("%s: got %t want %t", test.name, got, test.want)
		}
	}
}

func TestUncertainProbeCleansObservedRouteEvenWithUnexpectedSelector(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := target.Target{Address: "127.0.0.1", Port: 39004, Protocol: "tcp"}.Normalized()
	active := false
	provider := &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":true}}`}, nil
		case "serve --help", "funnel --help":
			return runner.Result{Stdout: "status --https --tcp off"}, nil
		case "serve status --json":
			if active {
				return runner.Result{Stdout: `{"TCP":{"39004":"127.0.0.1:39004"}}`}, nil
			}
			return runner.Result{Stdout: `{}`}, nil
		case "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --tcp=39004 tcp://127.0.0.1:39004":
			active = true
			return runner.Result{}, context.DeadlineExceeded
		case "serve --bg --tcp=39004 off":
			active = false
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p0\ncfixture\nn127.0.0.1:39004\n"}, nil
	})}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	probeRunner := probe.Runner{Discoverer: discoverer, Provider: provider, Config: manager, MutationLockPath: manager.Path + ".exposure.lock"}
	if err := probeRunner.Run(context.Background(), &cfg, target, exposuredata.ExposureServe, false); err == nil {
		t.Fatal("uncertain probe unexpectedly succeeded")
	}
	if active {
		t.Fatal("uncertain probe left the observed route active")
	}
}

func TestProbeCleanupDoesNotAssumeUnseenRouteWasRemoved(t *testing.T) {
	target := target.Target{Address: "127.0.0.1", Port: 39003, Protocol: "tcp"}.Normalized()
	err := (probe.Runner{}).CleanupRoute(context.Background(), target, exposuredata.ExposureServe, exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, "")
	if err == nil || !strings.Contains(err.Error(), "not observed") {
		t.Fatalf("unseen probe route was treated as cleaned: %v", err)
	}
}

func TestFunnelProbeRequiresPublicConfirmationAndCleansExactRoute(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := target.Target{Address: "127.0.0.1", Port: 39002, Protocol: "tcp"}.Normalized()
	active := false
	provider := &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"dev","Online":true}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status --tcp off"}, nil
		case "serve status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "funnel status --json":
			if active {
				return runner.Result{Stdout: `{"TCP":{"10000":"127.0.0.1:39002"},"AllowFunnel":{"dev.ts.net:10000":true}}`}, nil
			}
			return runner.Result{Stdout: `{}`}, nil
		case "funnel --bg --yes --tcp=10000 tcp://127.0.0.1:39002":
			active = true
			return runner.Result{}, nil
		case "funnel --tcp=10000 off":
			active = false
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	discoverer := &discovery.OSDiscoverer{OS: "darwin", Now: time.Now, Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: "p0\ncfixture\nn127.0.0.1:39002\n"}, nil
	})}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	probeRunner := probe.Runner{Discoverer: discoverer, Provider: provider, Config: manager, MutationLockPath: manager.Path + ".exposure.lock"}
	if err := probeRunner.Run(context.Background(), &cfg, target, exposuredata.ExposureFunnel, false); err == nil || active {
		t.Fatalf("unconfirmed funnel probe mutated state: active=%t err=%v", active, err)
	}
	if err := probeRunner.Run(context.Background(), &cfg, target, exposuredata.ExposureFunnel, true); err != nil {
		t.Fatal(err)
	}
	if active || cfg.FunnelProbeVersion != "1.102.4" {
		t.Fatalf("funnel probe did not clean up or persist evidence: active=%t cfg=%#v", active, cfg)
	}
}

type delayedReader struct {
	io.Reader
	delay time.Duration
}

func (r delayedReader) Read(p []byte) (int, error) {
	time.Sleep(r.delay)
	return r.Reader.Read(p)
}

func TestTUIKeepsDiscoveryAvailableWhenTailscaleIsUnavailable(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	discoverer := &discovery.OSDiscoverer{
		OS:  "darwin",
		Now: time.Now,
		Runner: runner.FuncRunner(func(_ context.Context, name string, _ ...string) (runner.Result, error) {
			if name != "lsof" {
				return runner.Result{}, nil
			}
			return runner.Result{Stdout: "p0\ncweb\nn127.0.0.1:3000\n", ExitCode: 0}, nil
		}),
	}
	provider := &tailscale.Adapter{Binary: "/missing/tailscale", Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{}, context.DeadlineExceeded
	}), Now: time.Now}
	var out, errOut bytes.Buffer
	code := tui.Run(delayedReader{Reader: strings.NewReader("?\nq\n"), delay: 500 * time.Millisecond}, &out, &errOut, discoverer, discovery.NewProcessTerminator(discoverer), provider, manager)
	if code != 0 || !strings.Contains(out.String(), "web") || !strings.Contains(out.String(), string(readinessmodel.ReadinessUnknown)) {
		t.Fatalf("TUI lost discovery/readiness state: code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
}
