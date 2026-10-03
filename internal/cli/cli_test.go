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

	"github.com/arrokh/tailge/internal/buildinfo"
	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposure"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/probe"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/target"
	"github.com/arrokh/tailge/internal/tui"
)

type staticListenerObserver struct {
	snapshot discovery.ListenerSnapshot
	err      error
}

func (observer *staticListenerObserver) List(context.Context) (discovery.ListenerSnapshot, error) {
	return observer.snapshot, observer.err
}

func httpPathTestAdapter(active *bool, calls *[]string) *tailscale.Adapter {
	return httpPathTestAdapterWithBackend(active, calls, "http://127.0.0.1:4321")
}

func httpPathTestAdapterWithBackend(active *bool, calls *[]string, backend string) *tailscale.Adapter {
	status := `{"TCP":{"443":{"HTTPS":true}},"Web":{"devbox.tailnet.ts.net:443":{"Handlers":{"/node-4321":{"Proxy":"` + backend + `"}}}},"AllowFunnel":{"devbox.tailnet.ts.net:443":false}}`
	setCommand := "serve --bg --yes --set-path=/node-4321 --https=443 " + backend
	return &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		*calls = append(*calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"devbox","DNSName":"devbox.tailnet.ts.net.","TailscaleIPs":["100.64.0.2"],"Online":true}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json", "funnel status --json":
			if !*active {
				return runner.Result{Stdout: `{}`}, nil
			}
			return runner.Result{Stdout: status}, nil
		case setCommand:
			*active = true
			return runner.Result{}, nil
		case "serve --set-path=/node-4321 --bg --https=443 off":
			*active = false
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
}

func customHTTPSRootTestAdapter(active *bool, calls *[]string) *tailscale.Adapter {
	const status = `{"TCP":{"4321":{"HTTPS":true}},"Web":{"devbox.tailnet.ts.net:4321":{"Handlers":{"/":{"Proxy":"http://localhost:4321"}}}},"AllowFunnel":{"devbox.tailnet.ts.net:4321":false}}`
	return &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		*calls = append(*calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"devbox","DNSName":"devbox.tailnet.ts.net.","TailscaleIPs":["100.64.0.2"],"Online":true}}`}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json", "funnel status --json":
			if *active {
				return runner.Result{Stdout: status}, nil
			}
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --set-path=/ --https=4321 http://localhost:4321":
			*active = true
			return runner.Result{}, nil
		case "serve --set-path=/ --bg --https=4321 off":
			*active = false
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
}

func rawTCPConversionTestAdapter(state *int, calls *[]string) *tailscale.Adapter {
	const status = `{"BackendState":"Running","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"devbox","DNSName":"devbox.tailnet.ts.net.","TailscaleIPs":["100.64.0.2"],"Online":true}}`
	const rawRoute = `{"TCP":{"4321":"127.0.0.1:4321"}}`
	const httpsRoot = `{"TCP":{"4321":{"HTTPS":true}},"Web":{"devbox.tailnet.ts.net:4321":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:4321"}}}},"AllowFunnel":{"devbox.tailnet.ts.net:4321":false}}`
	return &tailscale.Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		*calls = append(*calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "status --json":
			return runner.Result{Stdout: status}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json":
			switch *state {
			case 1:
				return runner.Result{Stdout: rawRoute}, nil
			case 2:
				return runner.Result{Stdout: httpsRoot}, nil
			default:
				return runner.Result{Stdout: `{}`}, nil
			}
		case "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --tcp=4321 off":
			*state = 0
			return runner.Result{}, nil
		case "serve --bg --yes --set-path=/ --https=4321 http://127.0.0.1:4321":
			*state = 2
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
}

func TestVersionReportsBuildCommitAndRepository(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "3d16efbb9058b146749de0d81aa7dd5eede3e9da"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	want := "tailge 3d16efbb9058  https://github.com/arrokh/tailge\n"
	for _, command := range []string{"version", "--version"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := (app{}).run([]string{command}, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("version exit code=%d, stderr=%q", code, stderr.String())
			}
			if stdout.String() != want {
				t.Fatalf("version output=%q, want %q", stdout.String(), want)
			}
		})
	}
}

func TestHumanStatusShowsEveryHTTPSPathAndCapabilityReason(t *testing.T) {
	listener := discovery.Listener{ID: "listener", Name: "web", Target: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}}
	view := exposure.View{Items: []exposure.ReconciledItem{{
		ID: listener.ID, Listener: &listener, State: exposuredata.ExposureActive,
		Routes: []exposuredata.ExposureRoute{
			{ID: "api", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", URL: "https://dev.example.ts.net/api", Target: listener.Target, Mode: exposuredata.ExposureServe},
			{ID: "docs", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", URL: "https://dev.example.ts.net/docs", Target: listener.Target, Mode: exposuredata.ExposureServe},
		},
	}}}
	var output bytes.Buffer
	printView(&output, view)
	for _, want := range []string{"web\tmultiple\tactive\tmultiple", "https-path=/api", "https-path=/docs", "https://dev.example.ts.net/api", "https://dev.example.ts.net/docs"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human route status omitted %q: %s", want, output.String())
		}
	}
	output.Reset()
	printReadiness(&output, readinessmodel.Readiness{Modes: []readinessmodel.ModeReadiness{{
		Mode: exposuredata.ExposureServe, Status: readinessmodel.ReadinessReady,
		HTTPPathStatus: readinessmodel.ReadinessReadOnly, HTTPPathMessage: "--set-path is unavailable", HTTPPathRemediation: "Upgrade Tailscale",
	}}})
	for _, want := range []string{"HTTPS paths: read_only", "HTTP path reason: --set-path is unavailable", "HTTP path next: Upgrade Tailscale"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human readiness omitted %q: %s", want, output.String())
		}
	}
}

func TestHTTPSRootCLIUsesExplicitCustomPortAndExactRemoval(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("version: 1\nserve_probe_version: 1.102.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenerTarget := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{{ID: "node-listener", Target: listenerTarget, Name: "Node.js", Process: "node", PID: 41, Scope: target.ScopeLoopback, Metadata: discovery.MetadataComplete}}}}
	active := false
	var calls []string
	application := app{config: manager, discoverer: observer, tailscale: customHTTPSRootTestAdapter(&active, &calls)}
	var stdout, stderr bytes.Buffer
	code := application.exposure([]string{"http", "serve", listenerTarget.String(), "--root", "--https-port", "4321", "--localhost-backend", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("custom HTTPS root CLI failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !active || !strings.Contains(stdout.String(), `"kind": "https_root"`) || !strings.Contains(stdout.String(), `"url": "https://devbox.tailnet.ts.net:4321/"`) {
		t.Fatalf("custom root route was not configured or observed: active=%t output=%s", active, stdout.String())
	}
	if !containsCall(calls, "serve --bg --yes --set-path=/ --https=4321 http://localhost:4321") {
		t.Fatalf("root route did not use explicit custom HTTPS port and localhost alias: %v", calls)
	}
	stdout.Reset()
	stderr.Reset()
	code = application.exposure([]string{"http", "disable", listenerTarget.String(), "--root", "--https-port", "4321", "--confirm-external", "--json"}, &stdout, &stderr)
	if code != 0 || active {
		t.Fatalf("exact custom root route removal failed: code=%d active=%t stdout=%q stderr=%q", code, active, stdout.String(), stderr.String())
	}
	if !containsCall(calls, "serve --set-path=/ --bg --https=4321 off") {
		t.Fatalf("root route was not removed with exact handler selector: %v", calls)
	}
}

func TestHTTPSRootCLIConvertsExactRawTCPRouteOnlyWithExplicitConfirmation(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("version: 1\nserve_probe_version: 1.102.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenerTarget := target.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{{ID: "node-listener", Target: listenerTarget, Name: "Node.js", Process: "node", PID: 41, Scope: target.ScopeLoopback, Metadata: discovery.MetadataComplete}}}}
	state := 1
	var calls []string
	application := app{config: manager, discoverer: observer, tailscale: rawTCPConversionTestAdapter(&state, &calls)}
	var stdout, stderr bytes.Buffer
	args := []string{"http", "serve", listenerTarget.String(), "--root", "--https-port", "4321", "--replace-raw-tcp", "--json"}
	code := application.exposure(args, &stdout, &stderr)
	if code == 0 || (!strings.Contains(stderr.String(), "unknown/external ownership") && !strings.Contains(stdout.String(), "unknown/external ownership")) || state != 1 {
		t.Fatalf("unknown-owned route was converted without confirmation: code=%d state=%d stdout=%q stderr=%q", code, state, stdout.String(), stderr.String())
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "serve --bg") {
			t.Fatalf("CLI mutated the route before external confirmation: %v", calls)
		}
	}

	stdout.Reset()
	stderr.Reset()
	args = append(args, "--confirm-external")
	code = application.exposure(args, &stdout, &stderr)
	if code != 0 || state != 2 || !strings.Contains(stdout.String(), `"kind": "https_root"`) || !strings.Contains(stdout.String(), `"url": "https://devbox.tailnet.ts.net:4321/"`) {
		t.Fatalf("confirmed same-port root conversion failed: code=%d state=%d stdout=%q stderr=%q", code, state, stdout.String(), stderr.String())
	}
	if !containsCall(calls, "serve --bg --tcp=4321 off") || !containsCall(calls, "serve --bg --yes --set-path=/ --https=4321 http://127.0.0.1:4321") {
		t.Fatalf("CLI did not remove only the exact TCP selector and configure the HTTPS root: %v", calls)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "funnel ") && call != "funnel --help" && call != "funnel status --json" {
			t.Fatalf("private HTTPS-root conversion invoked Funnel: %v", calls)
		}
	}
}

func TestNamedHTTPPathCLIExplicitlySelectsLocalhostBackendForIPv6(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("version: 1\nserve_probe_version: 1.102.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenerTarget := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{{ID: "node-listener", Target: listenerTarget, Name: "Node.js", Process: "node", PID: 41, Scope: target.ScopeLoopback, Metadata: discovery.MetadataComplete}}}}
	active := false
	var calls []string
	adapter := httpPathTestAdapterWithBackend(&active, &calls, "http://localhost:4321")
	application := app{config: manager, discoverer: observer, tailscale: adapter}
	var stdout, stderr bytes.Buffer
	code := application.exposure([]string{"http", "serve", listenerTarget.String(), "--path", "node-4321", "--localhost-backend", "--json"}, &stdout, &stderr)
	if code != 0 || !active {
		t.Fatalf("IPv6 named path with explicit alias failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !containsCall(calls, "serve --bg --yes --set-path=/node-4321 --https=443 http://localhost:4321") || !strings.Contains(stdout.String(), `"address": "127.0.0.1"`) || !strings.Contains(stdout.String(), `"url": "https://devbox.tailnet.ts.net/node-4321"`) || !strings.Contains(stdout.String(), "weakens the exact IPv6 address guarantee") {
		t.Fatalf("CLI did not pass or report explicit localhost backend: calls=%v output=%s", calls, stdout.String())
	}
}

func TestNamedHTTPPathCLIPreservesNumericIPv6AndSuggestsExplicitAlias(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("version: 1\nserve_probe_version: 1.102.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenerTarget := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{{ID: "node-listener", Target: listenerTarget, Name: "Node.js", Process: "node", PID: 41, Scope: target.ScopeLoopback, Metadata: discovery.MetadataComplete}}}}
	active := false
	var calls []string
	application := app{config: manager, discoverer: observer, tailscale: httpPathTestAdapterWithBackend(&active, &calls, "http://[::1]:4321")}
	var stdout, stderr bytes.Buffer
	code := application.exposure([]string{"http", "serve", listenerTarget.String(), "--path", "node-4321", "--json"}, &stdout, &stderr)
	if code != 0 || !active {
		t.Fatalf("numeric IPv6 named path failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !containsCall(calls, "serve --bg --yes --set-path=/node-4321 --https=443 http://[::1]:4321") || strings.Contains(strings.Join(calls, " "), "http://localhost:4321") || !strings.Contains(stdout.String(), "unknown proxy destination") || !strings.Contains(stdout.String(), "--localhost-backend") {
		t.Fatalf("CLI did not preserve exact IPv6 or provide opt-in guidance: calls=%v output=%s", calls, stdout.String())
	}
}

func TestHTTPSRootCLIRequiresExplicitPortAndAliasOptIn(t *testing.T) {
	application := app{discoverer: &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true}}}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "root port required", args: []string{"http", "serve", "[::1]:4321", "--root"}, want: "requires an explicit --https-port"},
		{name: "raw conversion requires root", args: []string{"http", "serve", "[::1]:4321", "--replace-raw-tcp"}, want: "only valid for a private Serve HTTPS root"},
		{name: "root external confirmation requires conversion intent", args: []string{"http", "serve", "[::1]:4321", "--root", "--https-port", "4321", "--confirm-external"}, want: "requires --replace-raw-tcp"},
		{name: "raw conversion is private Serve only", args: []string{"http", "funnel", "[::1]:4321", "--root", "--https-port", "4321", "--replace-raw-tcp"}, want: "only valid for a private Serve HTTPS root"},
		{name: "path and root conflict", args: []string{"http", "serve", "[::1]:4321", "--root", "--https-port", "4321", "--path", "api"}, want: "mutually exclusive"},
		{name: "named path stays on standard port", args: []string{"http", "serve", "[::1]:4321", "--path", "api", "--https-port", "4321"}, want: "use --root"},
		{name: "alias requires IPv6 listener", args: []string{"http", "serve", "127.0.0.1:4321", "--path", "api", "--localhost-backend"}, want: "only valid for IPv6 listeners"},
		{name: "custom root remains private", args: []string{"http", "funnel", "[::1]:4321", "--root", "--https-port", "4321"}, want: "only through private Serve"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := application.exposure(test.args, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("unsafe HTTPS root arguments were accepted or unclear: code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func TestHTTPPathCLIRequiresIntentAndPublicConfirmation(t *testing.T) {
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true}}
	var calls []string
	adapter := httpPathTestAdapter(new(bool), &calls)
	application := app{discoverer: observer, tailscale: adapter}
	var stdout, stderr bytes.Buffer
	code := application.exposure([]string{"http", "funnel", "127.0.0.1:4321"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "every path") {
		t.Fatalf("Funnel confirmation was not explicit: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, call := range calls {
		if strings.Contains(call, "--set-path") {
			t.Fatalf("provider mutation ran without public confirmation: %v", calls)
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = application.exposure([]string{"http", "serve", "127.0.0.1:4321", "--path", "../bad"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "lowercase slug") {
		t.Fatalf("invalid path accepted: code=%d stderr=%q", code, stderr.String())
	}
}

func TestHTTPPathCLIGeneratesPathAndRemovesOnlyExactHandler(t *testing.T) {
	manager, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.Path, []byte("version: 1\nserve_probe_version: 1.102.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listenerTarget := target.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	observer := &staticListenerObserver{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{{ID: "node-listener", Target: listenerTarget, Name: "Node.js", Process: "node", PID: 41, Scope: target.ScopeLoopback, Metadata: discovery.MetadataComplete}}}}
	active := false
	var calls []string
	application := app{config: manager, discoverer: observer, tailscale: httpPathTestAdapter(&active, &calls)}
	var stdout, stderr bytes.Buffer
	code := application.exposure([]string{"http", "serve", listenerTarget.String(), "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("HTTP route CLI failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !active || !strings.Contains(stdout.String(), `"path": "/node-4321"`) || !strings.Contains(stdout.String(), `"url": "https://devbox.tailnet.ts.net/node-4321"`) {
		t.Fatalf("generated route was not persisted or configured: active=%t output=%s", active, stdout.String())
	}
	if !containsCall(calls, "serve --bg --yes --set-path=/node-4321 --https=443 http://127.0.0.1:4321") {
		t.Fatalf("generated path did not use the explicit HTTPS handler operation: %v", calls)
	}
	observer.snapshot.Listeners[0].Name = "renamed-process"
	observer.snapshot.Listeners[0].Process = "renamed-process"
	stdout.Reset()
	stderr.Reset()
	code = application.exposure([]string{"disable", listenerTarget.String(), "--json"}, &stdout, &stderr)
	if code == 0 || !active || !strings.Contains(stdout.String(), "requires explicit route identity") || containsCall(calls, "serve --set-path=/node-4321 --bg --https=443 off") {
		t.Fatalf("raw disable unexpectedly removed a named HTTP route: code=%d active=%t stdout=%q calls=%v", code, active, stdout.String(), calls)
	}
	stdout.Reset()
	stderr.Reset()
	code = application.exposure([]string{"http", "disable", listenerTarget.String(), "--path", "node-4321", "--confirm-external"}, &stdout, &stderr)
	if code != 0 || active || !strings.Contains(stdout.String(), "HTTPS path /node-4321") || !strings.Contains(stdout.String(), "removed and verified") {
		t.Fatalf("exact path disable feedback was unclear: code=%d active=%t stdout=%q stderr=%q", code, active, stdout.String(), stderr.String())
	}
	if !containsCall(calls, "serve --set-path=/node-4321 --bg --https=443 off") {
		t.Fatalf("disable did not remove the persisted exact path: %v", calls)
	}
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}

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
