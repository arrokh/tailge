package tailscale

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/runner"
	"github.com/arrokh/tailge/internal/target"
)

func statusJSON(backend string, online bool) string {
	return `{"BackendState":"` + backend + `","HaveNodeKey":true,"TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"devbox","DNSName":"devbox.tailnet.ts.net.","Online":` + boolText(online) + `}}`
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func readinessRunner() runner.FuncRunner {
	return func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch {
		case command == "version":
			return runner.Result{Stdout: "1.80.0\n", ExitCode: 0}, nil
		case command == "status --json":
			return runner.Result{Stdout: statusJSON("Running", true), ExitCode: 0}, nil
		case command == "serve --help":
			return runner.Result{Stdout: "serve status clear <service> --https --tcp", ExitCode: 0}, nil
		case command == "funnel --help":
			return runner.Result{Stdout: "funnel status reset --https value --tcp value", ExitCode: 0}, nil
		case command == "serve status --json":
			return runner.Result{Stdout: `{"TCP":{}}`, ExitCode: 0}, nil
		case command == "funnel status --json":
			return runner.Result{Stdout: `{}`, ExitCode: 0}, nil
		default:
			return runner.Result{ExitCode: 1}, errors.New("unexpected command: " + command)
		}
	}
}

func TestExactFlagOffRequiresNearbySyntaxEvidence(t *testing.T) {
	if exactFlagOff("--tcp value; descriptive text says this does not turn off routes") {
		t.Fatal("descriptive text was treated as exact off syntax")
	}
	if !exactFlagOff("usage: --tcp=443 off") {
		t.Fatal("expected exact off syntax")
	}
}

func TestHasValueFlagRecognizesV2HelpShape(t *testing.T) {
	if !hasValueFlag("--https value\n--tcp value", "--https") || !hasValueFlag("--https value\n--tcp value", "--tcp") {
		t.Fatal("v2 value flags were not recognized")
	}
	if hasValueFlag("status reset --tcp", "--tcp") {
		t.Fatal("bare flag token was treated as a value flag")
	}
}

func TestCapabilitiesRecognizesV2FunnelExactOffDespiteHelpOmission(t *testing.T) {
	adapter := &Adapter{Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		switch strings.Join(args, " ") {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset\n--https value\n--tcp value"}, nil
		default:
			return runner.Result{}, errors.New("unexpected help command")
		}
	}), Binary: "tailscale"}
	caps, err := adapter.Capabilities(context.Background())
	if err != nil || !caps.Funnel || !caps.ExactFunnel || !caps.FunnelHTTPS || !caps.FunnelTCP {
		t.Fatalf("capabilities=%#v err=%v", caps, err)
	}
}

func TestStatusRejectsMalformedNodeAddresses(t *testing.T) {
	adapter := &Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{Stdout: `{"BackendState":"Running","TailscaleIPs":["not-an-ip"]}`}, nil
	})}
	if _, err := adapter.Status(context.Background()); err == nil || fault.AsAppError(err).Code != fault.ErrUnknown {
		t.Fatalf("malformed node address was accepted: %v", err)
	}
}

func TestCapabilitiesRecognizesExactFunnelHelpWithoutBroadReset(t *testing.T) {
	adapter := &Adapter{Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		switch strings.Join(args, " ") {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stderr: "status clear drain --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stderr: "status reset --tcp --https off"}, nil
		default:
			return runner.Result{}, errors.New("unexpected help command")
		}
	}), Binary: "tailscale"}
	caps, err := adapter.Capabilities(context.Background())
	if err != nil || !caps.Serve || !caps.ExactServe || !caps.Funnel || !caps.ExactFunnel {
		t.Fatalf("capabilities=%#v err=%v", caps, err)
	}
}

func TestReadinessSeparatesServeAndFunnelMutationEvidence(t *testing.T) {
	adapter := &Adapter{Runner: readinessRunner(), Binary: "tailscale", Now: func() time.Time { return time.Unix(100, 0) }}
	report, err := adapter.Readiness(context.Background(), ReadinessOptions{ServeProbeVersion: "1.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != readinessmodel.ReadinessReady {
		t.Fatalf("overall readiness = %s, want ready", report.Status)
	}
	var serve, funnel readinessmodel.ModeReadiness
	for _, mode := range report.Modes {
		if mode.Mode == exposuredata.ExposureServe {
			serve = mode
		}
		if mode.Mode == exposuredata.ExposureFunnel {
			funnel = mode
		}
	}
	if serve.Status != readinessmodel.ReadinessReady || !serve.Probe {
		t.Fatalf("serve readiness = %#v", serve)
	}
	if serve.HTTPPathStatus != readinessmodel.ReadinessReadOnly || !strings.Contains(serve.HTTPPathMessage, "--set-path") {
		t.Fatalf("unsupported Serve HTTP path capability was hidden: %#v", serve)
	}
	if funnel.HTTPPathStatus != readinessmodel.ReadinessReadOnly || !strings.Contains(funnel.HTTPPathMessage, "--set-path") {
		t.Fatalf("unsupported Funnel HTTP path capability was hidden: %#v", funnel)
	}
	if funnel.Status != readinessmodel.ReadinessReady || funnel.Probe || len(funnel.Checks) == 0 || funnel.Checks[0].Status != readinessmodel.ReadinessReady {
		t.Fatalf("funnel readiness = %#v", funnel)
	}
}

func TestLegacyFunnelCannotClaimRawTCPReadinessOrMutate(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.80.0\n"}, nil
		case "status --json":
			return runner.Result{Stdout: statusJSON("Running", true)}, nil
		case "serve --help":
			return runner.Result{Stdout: "serve status clear --https --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "funnel status {on|off}"}, nil
		case "serve status --json":
			return runner.Result{Stdout: `{"TCP":{}}`}, nil
		case "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}

	report, err := adapter.Readiness(context.Background(), ReadinessOptions{ServeProbeVersion: "1.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	var funnel readinessmodel.ModeReadiness
	for _, mode := range report.Modes {
		if mode.Mode == exposuredata.ExposureFunnel {
			funnel = mode
		}
	}
	if funnel.Status != readinessmodel.ReadinessReadOnly || len(funnel.Checks) == 0 || !strings.Contains(funnel.Checks[0].Message, "raw-TCP") {
		t.Fatalf("legacy Funnel was not reported read-only for raw TCP: %#v", funnel)
	}

	calls = nil
	_, err = adapter.Set(context.Background(), ExposureChange{
		Target:        target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"},
		Mode:          exposuredata.ExposureFunnel,
		Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(nil)},
	})
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnsupported {
		t.Fatalf("legacy Funnel raw TCP mutation was not refused: %v", err)
	}
	for _, command := range calls {
		if strings.HasPrefix(command, "funnel --bg") {
			t.Fatalf("legacy Funnel mutation ran despite unsupported raw TCP: %q", command)
		}
	}
}

func TestListPreservesDependencyExitClassWhenRoutesAreUnavailable(t *testing.T) {
	adapter := &Adapter{Binary: "/missing/tailscale", Now: time.Now}
	snapshot, err := adapter.List(context.Background())
	if err == nil || snapshot.Error == nil || snapshot.Error.Code != fault.ErrDependency || fault.AsAppError(err).Code != fault.ErrDependency {
		t.Fatalf("dependency was misclassified: snapshot=%#v err=%v", snapshot, err)
	}
}

func TestListRejectsStatusDiagnosticsAsNonAuthoritative(t *testing.T) {
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		if strings.Join(args, " ") == "serve status --json" {
			return runner.Result{Stdout: `{}`, Stderr: "warning token=private"}, nil
		}
		return runner.Result{Stdout: `{}`}, nil
	})}
	snapshot, err := adapter.List(context.Background())
	if err == nil || snapshot.Authoritative || snapshot.Error == nil || snapshot.Error.Code != fault.ErrUnknown {
		t.Fatalf("diagnostic status was treated as authoritative: snapshot=%#v err=%v", snapshot, err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("status diagnostic leaked secret: %v", err)
	}
}

func TestReadinessFailureStillReportsBothModes(t *testing.T) {
	adapter := &Adapter{Runner: runner.FuncRunner(func(context.Context, string, ...string) (runner.Result, error) {
		return runner.Result{ExitCode: 1}, errors.New("daemon unavailable")
	}), Binary: "tailscale", Now: time.Now}
	report, err := adapter.Readiness(context.Background(), ReadinessOptions{})
	if err == nil || len(report.Modes) != 2 {
		t.Fatalf("expected error and two mode results: report=%#v err=%v", report, err)
	}
	for _, mode := range report.Modes {
		if mode.Status != readinessmodel.ReadinessNotReady && mode.Status != readinessmodel.ReadinessUnknown {
			t.Fatalf("mode %s hid readiness failure: %s", mode.Mode, mode.Status)
		}
	}
}

func TestFunnelHTTPSPortSupport(t *testing.T) {
	for _, port := range []int{443, 8443, 10000} {
		if !FunnelHTTPSPortSupported(port) {
			t.Errorf("Funnel HTTPS port %d was rejected", port)
		}
	}
	for _, port := range []int{3000, 4321, 65535} {
		if FunnelHTTPSPortSupported(port) {
			t.Errorf("unsupported Funnel HTTPS port %d was accepted", port)
		}
	}
}

func TestParseListenerSelectorAndRouteFingerprint(t *testing.T) {
	selector, err := ParseListenerSelector("serve:https=443", exposuredata.ExposureServe)
	if err != nil || selector.Transport != "https" || selector.Port != 443 {
		t.Fatalf("selector=%#v err=%v", selector, err)
	}
	if _, err := ParseListenerSelector("funnel:svc:api", exposuredata.ExposureFunnel); err == nil {
		t.Fatal("service selector was accepted as a deterministic listener")
	}
	first := exposuredata.ExposureRoute{ID: "route", ProviderKey: "serve:https=443", Target: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, Mode: exposuredata.ExposureServe, URL: "https://dev.ts.net:443", Backend: "http://127.0.0.1:3000"}
	second := first
	second.Backend = "tcp://127.0.0.1:3000"
	if RouteFingerprint(first) == RouteFingerprint(second) {
		t.Fatal("route fingerprint ignored backend identity")
	}
	second = first
	second.URL = "https://other.ts.net:443"
	if RouteFingerprint(first) == RouteFingerprint(second) {
		t.Fatal("route fingerprint ignored observed URL identity")
	}
}

func TestParseStatusFindsTCPRoute(t *testing.T) {
	routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":{"443":"127.0.0.1:8080"}}`), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes: %#v", len(routes), routes)
	}
	if routes[0].Target.Address != "127.0.0.1" || routes[0].Target.Port != 8080 || routes[0].Mode != exposuredata.ExposureServe || routes[0].ProviderKey != "serve:tcp=443" {
		t.Fatalf("unexpected route: %#v", routes[0])
	}
}

func TestParseStatusAcceptsFunnelPermissionWithoutHandler(t *testing.T) {
	routes, err := parseStatus(exposuredata.ExposureFunnel, []byte(`{"AllowFunnel":{"dev.example.ts.net:10000":true}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 0 {
		t.Fatalf("permission-only Funnel status was not treated as an authoritative empty route set: routes=%#v err=%v", routes, err)
	}
}

func TestParseStatusRejectsUnrecognizedNonemptyShape(t *testing.T) {
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"unexpected":"value"}`), time.Unix(1, 0)); err == nil {
		t.Fatal("unrecognized status shape was treated as authoritative empty state")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"Target":{"unexpected":true}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("non-string target field was treated as authoritative")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":null}`), time.Unix(1, 0)); err == nil {
		t.Fatal("null TCP field was treated as authoritative")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":{"443":"not-a-target"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed TCP route was treated as authoritative")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":{"443":{"HTTPS":true}}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("recognized route metadata without a target was treated as empty state")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"Service":"svc:missing-target"}`), time.Unix(1, 0)); err == nil {
		t.Fatal("recognized service metadata without a route was treated as empty state")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"Web":{"dev.example.ts.net":"not-a-route"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed Web route was treated as authoritative")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"Web":{"dev.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000","Backend":"http://127.0.0.1:3001"}}}}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("conflicting handler targets were treated as authoritative")
	}
	if _, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":{"443":"127.0.0.1:8080"},"AllowFunnel":{"dev.example.ts.net:443":"true"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed AllowFunnel permission was treated as private Serve")
	}
	if routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{}`), time.Unix(1, 0)); err != nil || len(routes) != 0 {
		t.Fatalf("empty status should represent no routes: routes=%#v err=%v", routes, err)
	}
	if routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"tcp":{"443":"127.0.0.1:8080"}}`), time.Unix(1, 0)); err != nil || len(routes) != 1 {
		t.Fatalf("case-insensitive TCP status was not parsed: routes=%#v err=%v", routes, err)
	}
	if routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"TCP":{"443":{"proxy":"127.0.0.1:8080"}}}`), time.Unix(1, 0)); err != nil || len(routes) != 1 {
		t.Fatalf("case-insensitive TCP target field was not parsed: routes=%#v err=%v", routes, err)
	} else if routes[0].ProviderKey != "serve:tcp=443" {
		t.Fatalf("TCP container was misclassified as HTTPS: %#v", routes[0])
	}
}

func TestParseStatusHandlesUppercaseURLRouteKeys(t *testing.T) {
	routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"HTTPS://DEV.example.ts.net:443":{"Proxy":"http://127.0.0.1:3000"}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "serve:https=443" || routes[0].Kind != exposuredata.RouteKindHTTPSRoot || routes[0].Path != "/" || routes[0].URL != "https://DEV.example.ts.net/" {
		t.Fatalf("unexpected uppercase URL route: %#v", routes[0])
	}
}

func TestParseStatusPreservesPathFromHTTPSURLRouteKey(t *testing.T) {
	routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"HTTPS://DEV.example.ts.net:443/api":{"Proxy":"http://127.0.0.1:3000"}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].Kind != exposuredata.RouteKindHTTPPath || routes[0].Path != "/api" || routes[0].ProviderKey != "serve:https=443" || routes[0].URL != "https://DEV.example.ts.net/api" {
		t.Fatalf("HTTPS URL route key lost handler path identity: %#v", routes[0])
	}
}

func TestParseStatusDoesNotTreatPortLikeHandlerPathAsEndpointPort(t *testing.T) {
	fixture := []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/api:4321":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	routes, err := parseStatus(exposuredata.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "serve:https=443" || routes[0].Path != "/api:4321" || routes[0].URL != "https://dev.example.ts.net/api:4321" {
		t.Fatalf("handler path suffix was mistaken for its HTTPS endpoint port or URL: %#v", routes[0])
	}
}

func TestParseStatusFindsWebProxyAndExactPublicPortSelector(t *testing.T) {
	fixture := []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	routes, err := parseStatus(exposuredata.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "serve:https=443" || routes[0].Kind != exposuredata.RouteKindHTTPSRoot || routes[0].Path != "/" || routes[0].URL != "https://dev.example.ts.net/" || routes[0].Target.Port != 3000 {
		t.Fatalf("unexpected web route: %#v", routes[0])
	}
}

func TestTargetMatchesIPv4AndIPv6Loopback(t *testing.T) {
	ipv4 := target.Target{Address: "127.0.0.1", Port: 4323, Protocol: "tcp"}
	ipv6 := target.Target{Address: "::1", Port: 4323, Protocol: "tcp"}
	if !targetMatches(ipv4, ipv6) || !targetMatches(ipv6, ipv4) {
		t.Fatal("loopback families were not matched")
	}
}

func TestParseStatusAcceptsTailscaleUnbracketedIPv6Proxy(t *testing.T) {
	fixture := []byte(`{"TCP":{"4322":{"HTTPS":true}},"Web":{"dev.example.ts.net:4322":{"Handlers":{"/":{"Proxy":"http://::1:4322"}}}}}`)
	routes, err := parseStatus(exposuredata.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 || routes[0].Target.Address != "::1" || routes[0].Target.Port != 4322 {
		t.Fatalf("unbracketed IPv6 proxy was not parsed: routes=%#v err=%v", routes, err)
	}
}

func TestHTTPSFunnelScopeUsesPublicEndpointPortNotBackendPort(t *testing.T) {
	fixture := []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/api":{"Proxy":"http://127.0.0.1:4321"}}}},"AllowFunnel":{"dev.example.ts.net:4321":true}}`)
	serveRoutes, err := parseStatus(exposuredata.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 1 || serveRoutes[0].ProviderKey != "serve:https=443" {
		t.Fatalf("backend-port Funnel permission hid the private HTTPS endpoint: routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err := parseStatus(exposuredata.ExposureFunnel, fixture, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 0 {
		t.Fatalf("Funnel permission on the backend port was applied to HTTPS: routes=%#v err=%v", funnelRoutes, err)
	}
}

func TestParseStatusUsesAllowFunnelToSeparateServeAndFunnel(t *testing.T) {
	private := []byte(`{"TCP":{"3000":{"HTTPS":true}},"Web":{"dev.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://0.0.0.0:3000"}}}}}`)
	serveRoutes, err := parseStatus(exposuredata.ExposureServe, private, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 1 {
		t.Fatalf("private Serve routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err := parseStatus(exposuredata.ExposureFunnel, private, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 0 {
		t.Fatalf("private config was incorrectly classified as Funnel: routes=%#v err=%v", funnelRoutes, err)
	}

	public := []byte(`{"TCP":{"3000":{"HTTPS":true}},"Web":{"dev.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://0.0.0.0:3000"}}}},"AllowFunnel":{"dev.example.ts.net:3000":true}}`)
	serveRoutes, err = parseStatus(exposuredata.ExposureServe, public, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 0 {
		t.Fatalf("public config was incorrectly retained as Serve: routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err = parseStatus(exposuredata.ExposureFunnel, public, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 1 || funnelRoutes[0].Mode != exposuredata.ExposureFunnel {
		t.Fatalf("public config was not classified as Funnel: routes=%#v err=%v", funnelRoutes, err)
	}

	mixed := []byte(`{"Web":{"private.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}},"public.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3001"}}}},"AllowFunnel":{"private.example.ts.net:3000":false,"public.example.ts.net:3000":true}}`)
	serveRoutes, err = parseStatus(exposuredata.ExposureServe, mixed, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 1 || serveRoutes[0].URL != "https://private.example.ts.net:3000/" {
		t.Fatalf("host-specific private Serve route was misclassified: routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err = parseStatus(exposuredata.ExposureFunnel, mixed, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 1 || funnelRoutes[0].URL != "https://public.example.ts.net:3000/" {
		t.Fatalf("host-specific public Funnel route was misclassified: routes=%#v err=%v", funnelRoutes, err)
	}
}

func TestParseStatusPreservesServicePathAndBackendIdentity(t *testing.T) {
	fixture := []byte(`{"Service":"svc:api","Web":{"dev.example.ts.net:8443":{"Handlers":{"/admin":{"Proxy":"http://127.0.0.1:3000/admin"}}}}}`)
	routes, err := parseStatus(exposuredata.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	route := routes[0]
	if route.Service != "svc:api" || route.Path != "/admin" || route.ProviderKey != "serve:https=8443" || route.Backend != "http://127.0.0.1:3000/admin" {
		t.Fatalf("scoped route identity was not preserved: %#v", route)
	}
}

func TestParseStatusDoesNotGuessRemovalSelectorWhenPublicPortIsMissing(t *testing.T) {
	routes, err := parseStatus(exposuredata.ExposureServe, []byte(`{"Web":{"dev.example.ts.net":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "" {
		t.Fatalf("guessed unsafe provider selector: %#v", routes[0])
	}
}

func TestHashIDsUsesCanonicalNULSeparator(t *testing.T) {
	want := hashIDs([]string{"serve:a", "serve:b"})
	if want == "" || want == hashIDs([]string{"serve:a\\x00serve:b"}) {
		t.Fatal("route fingerprint is not canonical")
	}
}

func TestListDoesNotDuplicatePrivateServeAsFunnel(t *testing.T) {
	status := `{"TCP":{"3000":{"HTTPS":true}},"Web":{"dev.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://0.0.0.0:3000"}}}}}`
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		switch strings.Join(args, " ") {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: status}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + strings.Join(args, " "))
		}
	})}
	snapshot, err := adapter.List(context.Background())
	if err != nil || !snapshot.Authoritative || len(snapshot.Routes) != 1 || snapshot.Routes[0].Mode != exposuredata.ExposureServe {
		t.Fatalf("private Serve status was duplicated or misclassified: routes=%#v authoritative=%t err=%v", snapshot.Routes, snapshot.Authoritative, err)
	}
}

func TestHTTPBackendPreservesIPv6UnlessLocalhostAliasIsExplicit(t *testing.T) {
	selected := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}
	exact := HTTPPathBackendArgument(selected)
	if exact != "http://[::1]:4321" {
		t.Fatalf("IPv6 HTTP path backend = %q, want exact loopback URL", exact)
	}
	if !HTTPPathBackendMatches("http://::1:4321", exact) || !HTTPPathBackendMatches("http://[::1]:4321/", exact) {
		t.Fatal("equivalent bracketed/unbracketed IPv6 HTTP backend spellings did not match")
	}
	for _, different := range []string{"tcp://[::1]:4321", "http://[::1]:4322", "http://[::1]:4321/admin"} {
		if HTTPPathBackendMatches(different, exact) {
			t.Fatalf("different backend identity matched the exact IPv6 backend: %q", different)
		}
	}
	alias := HTTPSBackendArgument(selected, true)
	if alias != "http://localhost:4321" || !HTTPPathBackendMatches("http://localhost:4321", alias) {
		t.Fatalf("explicit localhost backend alias = %q", alias)
	}
	if HTTPSBackendArgument(selected, false) != exact {
		t.Fatal("IPv6 backend was silently aliased without explicit permission")
	}
	if got := HTTPPathBackendArgument(target.Target{Address: "0.0.0.0", Port: 4321, Protocol: "tcp"}); got != "http://127.0.0.1:4321" {
		t.Fatalf("wildcard HTTP backend = %q", got)
	}
	if got := HTTPSBackendArgument(target.Target{Address: "::", Port: 4321, Protocol: "tcp"}, false); got != "http://[::1]:4321" {
		t.Fatalf("IPv6 wildcard HTTP backend = %q", got)
	}
}

func TestTCPBackendArgumentPreservesSpecificAndTranslatesWildcardAddresses(t *testing.T) {
	cases := []struct {
		name   string
		target target.Target
		want   string
	}{
		{name: "ipv4 loopback", target: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, want: "tcp://127.0.0.1:3000"},
		{name: "ipv6 loopback", target: target.Target{Address: "::1", Port: 3000, Protocol: "tcp"}, want: "tcp://[::1]:3000"},
		{name: "ipv4 wildcard", target: target.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, want: "tcp://127.0.0.1:3000"},
		{name: "ipv6 wildcard", target: target.Target{Address: "::", Port: 3000, Protocol: "tcp"}, want: "tcp://[::1]:3000"},
		{name: "specific ipv4", target: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, want: "tcp://192.168.1.20:3000"},
		{name: "specific ipv6", target: target.Target{Address: "fd7a:115c:a1e0::20", Port: 3000, Protocol: "tcp"}, want: "tcp://[fd7a:115c:a1e0::20]:3000"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := rawTCPBackendArgument(test.target, "")
			if err != nil || got != test.want {
				t.Fatalf("rawTCPBackendArgument(%#v, empty) = %q, err=%v; want %q", test.target, got, err, test.want)
			}
		})
	}
}

func TestRawTCPBackendArgumentPreservesRestoredAddressAndTransport(t *testing.T) {
	cases := []struct {
		name     string
		selected target.Target
		backend  string
		want     string
		wantErr  bool
	}{
		{name: "scheme-less non-loopback IPv4 status", selected: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, backend: "192.168.1.20:3000", want: "tcp://192.168.1.20:3000"},
		{name: "scheme-less non-loopback IPv6 status", selected: target.Target{Address: "fd7a:115c:a1e0::20", Port: 3000, Protocol: "tcp"}, backend: "[fd7a:115c:a1e0::20]:3000", want: "tcp://[fd7a:115c:a1e0::20]:3000"},
		{name: "lowest supported port", selected: target.Target{Address: "192.168.1.20", Port: 1, Protocol: "tcp"}, backend: "192.168.1.20:1", want: "tcp://192.168.1.20:1"},
		{name: "highest supported port", selected: target.Target{Address: "fd7a:115c:a1e0::20", Port: 65535, Protocol: "tcp"}, backend: "[fd7a:115c:a1e0::20]:65535", want: "tcp://[fd7a:115c:a1e0::20]:65535"},
		{name: "wildcard IPv4 maps to covered loopback", selected: target.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, backend: "127.0.0.1:3000", want: "tcp://127.0.0.1:3000"},
		{name: "wildcard IPv6 maps to covered loopback", selected: target.Target{Address: "::", Port: 3000, Protocol: "tcp"}, backend: "tcp://[::1]:3000", want: "tcp://[::1]:3000"},
		{name: "captured localhost spelling is preserved", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "localhost:3000", want: "tcp://localhost:3000"},
		{name: "IPv6 loopback remains numeric", selected: target.Target{Address: "::1", Port: 3000, Protocol: "tcp"}, backend: "tcp://[::1]:3000", want: "tcp://[::1]:3000"},
		{name: "HTTP backend rejected", selected: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, backend: "http://192.168.1.20:3000", wantErr: true},
		{name: "different backend address rejected", selected: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, backend: "192.168.1.21:3000", wantErr: true},
		{name: "different backend port rejected", selected: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, backend: "192.168.1.20:3001", wantErr: true},
		{name: "wildcard cannot restore a different loopback address", selected: target.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, backend: "127.0.0.2:3000", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := rawTCPBackendArgument(test.selected, test.backend)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("rawTCPBackendArgument(%#v, %q) = %q, err=%v; want %q, error=%t", test.selected, test.backend, got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestRawTCPBackendMatchesExactSelectedTarget(t *testing.T) {
	cases := []struct {
		name     string
		selected target.Target
		backend  string
		want     bool
	}{
		{name: "exact loopback", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "tcp://127.0.0.1:3000", want: true},
		{name: "scheme-less status", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "127.0.0.1:3000", want: true},
		{name: "covered wildcard loopback", selected: target.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, backend: "tcp://127.0.0.1:3000", want: true},
		{name: "different address", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "tcp://127.0.0.2:3000", want: false},
		{name: "different port", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "tcp://127.0.0.1:3001", want: false},
		{name: "missing backend identity", selected: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, backend: "", want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := RawTCPBackendMatchesTarget(test.selected, test.backend); got != test.want {
				t.Fatalf("RawTCPBackendMatchesTarget(%#v, %q) = %t, want %t", test.selected, test.backend, got, test.want)
			}
		})
	}
}

func TestHTTPBackendArgumentSupportsListenerAddressFamilies(t *testing.T) {
	cases := []struct {
		name   string
		target target.Target
		want   string
	}{
		{name: "ipv4 loopback", target: target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, want: "http://127.0.0.1:3000"},
		{name: "ipv6 loopback stays exact", target: target.Target{Address: "::1", Port: 3000, Protocol: "tcp"}, want: "http://[::1]:3000"},
		{name: "ipv4 wildcard uses contained loopback", target: target.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, want: "http://127.0.0.1:3000"},
		{name: "ipv6 wildcard uses contained loopback", target: target.Target{Address: "::", Port: 3000, Protocol: "tcp"}, want: "http://[::1]:3000"},
		{name: "specific ipv4 stays exact", target: target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, want: "http://192.168.1.20:3000"},
		{name: "specific ipv6 stays exact", target: target.Target{Address: "fd7a:115c:a1e0::20", Port: 3000, Protocol: "tcp"}, want: "http://[fd7a:115c:a1e0::20]:3000"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := HTTPPathBackendArgument(test.target); got != test.want {
				t.Fatalf("HTTPPathBackendArgument(%#v) = %q, want %q", test.target, got, test.want)
			}
		})
	}
}

func TestSetTranslatesBindAddressesAcrossServeFunnelAndHTTPS(t *testing.T) {
	addresses := []struct {
		address  string
		tcpHost  string
		httpHost string
	}{
		{address: "0.0.0.0", tcpHost: "127.0.0.1", httpHost: "127.0.0.1"},
		{address: "127.0.0.1", tcpHost: "127.0.0.1", httpHost: "127.0.0.1"},
		{address: "::", tcpHost: "[::1]", httpHost: "[::1]"},
		{address: "::1", tcpHost: "[::1]", httpHost: "[::1]"},
		{address: "192.168.1.20", tcpHost: "192.168.1.20", httpHost: "192.168.1.20"},
		{address: "fd7a:115c:a1e0::20", tcpHost: "[fd7a:115c:a1e0::20]", httpHost: "[fd7a:115c:a1e0::20]"},
	}
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		default:
			if strings.HasPrefix(command, "serve --bg --yes ") || strings.HasPrefix(command, "funnel --bg --yes ") {
				return runner.Result{}, nil
			}
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}

	for _, address := range addresses {
		for _, mode := range []exposuredata.ExposureMode{exposuredata.ExposureServe, exposuredata.ExposureFunnel} {
			target := target.Target{Address: address.address, Port: 3000, Protocol: "tcp"}.Normalized()
			listenPort := 3000
			if mode == exposuredata.ExposureFunnel {
				listenPort = 10000
			}
			for _, route := range []struct {
				name        string
				httpPath    bool
				transport   string
				backendHost string
			}{
				{name: "raw-tcp", transport: "tcp", backendHost: address.tcpHost},
				{name: "https-web", httpPath: true, transport: "https", backendHost: address.httpHost},
			} {
				port := listenPort
				if route.httpPath {
					port = 443
				}
				backendScheme := "tcp"
				if route.httpPath {
					backendScheme = "http"
				}
				pathFlag := ""
				if route.httpPath {
					pathFlag = "--set-path=/web "
				}
				want := string(mode) + " --bg --yes " + pathFlag + "--" + route.transport + "=" + strconv.Itoa(port) + " " + backendScheme + "://" + route.backendHost + ":3000"
				t.Run(string(mode)+"/"+route.name+"/"+address.address, func(t *testing.T) {
					_, err := adapter.Set(context.Background(), ExposureChange{
						Target: target,
						Mode:   mode,
						Path: func() string {
							if route.httpPath {
								return "/web"
							}
							return ""
						}(),
						HTTPPath: route.httpPath,
						Preconditions: ExposurePrecondition{
							RouteIDsHash:  hashIDs(nil),
							AllRoutesHash: RoutesHash(nil),
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					if got := calls[len(calls)-1]; got != want {
						t.Fatalf("provider mutation command = %q, want %q", got, want)
					}
				})
			}
		}
	}
}

func TestSetRestoresOnlyAnExactRawTCPBackend(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend string
		want    string
		wantErr bool
	}{
		{name: "scheme-less provider status", backend: "192.168.1.20:3000", want: "serve --bg --yes --tcp=3000 tcp://192.168.1.20:3000"},
		{name: "different backend is refused", backend: "192.168.1.21:3000", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
				command := strings.Join(args, " ")
				calls = append(calls, command)
				switch command {
				case "version":
					return runner.Result{Stdout: "1.102.4\n"}, nil
				case "serve --help":
					return runner.Result{Stdout: "status clear --https --tcp"}, nil
				case "funnel --help":
					return runner.Result{Stdout: "status reset --https value --tcp value"}, nil
				case "serve status --json", "funnel status --json":
					return runner.Result{Stdout: `{}`}, nil
				default:
					if strings.HasPrefix(command, "serve --bg --yes ") {
						return runner.Result{}, nil
					}
					return runner.Result{}, errors.New("unexpected command: " + command)
				}
			})}
			target := target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}
			_, err := adapter.Set(context.Background(), ExposureChange{
				Target:        target,
				Mode:          exposuredata.ExposureServe,
				ProviderKey:   "serve:tcp=3000",
				Backend:       test.backend,
				Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(nil)},
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("Set error = %v, want error=%t", err, test.wantErr)
			}
			if test.wantErr {
				for _, command := range calls {
					if strings.HasPrefix(command, "serve --bg --yes ") {
						t.Fatalf("unsafe backend reached mutation command: %q", command)
					}
				}
				return
			}
			if got := calls[len(calls)-1]; got != test.want {
				t.Fatalf("provider mutation command = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSetRefusesProviderEndpointCollision(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		case "serve status --json":
			return runner.Result{Stdout: `{"Web":{"dev.example.ts.net:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`}, nil
		case "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := target.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	_, err = adapter.Set(context.Background(), ExposureChange{Target: target, Mode: exposuredata.ExposureServe, Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(snapshot.Routes)}})
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnsafe {
		t.Fatalf("endpoint collision was not refused: %v", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "serve --bg") {
			t.Fatalf("mutating command ran despite endpoint collision: %v", calls)
		}
	}
}

func TestSetRestoresExplicitHTTPSRootOnExactProviderListener(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --set-path=/ --https=443 http://127.0.0.1:3000":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	_, err := adapter.Set(context.Background(), ExposureChange{
		Target:      target,
		Mode:        exposuredata.ExposureServe,
		ProviderKey: "serve:https=443",
		Path:        "/",
		HTTPSPort:   443,
		HTTPSRoot:   true,
		Preconditions: ExposurePrecondition{
			RouteIDsHash:  hashIDs(nil),
			AllRoutesHash: RoutesHash(nil),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --bg --yes --set-path=/ --https=443 http://127.0.0.1:3000" {
		t.Fatalf("restore selector was not preserved: %v", calls)
	}
}

func TestSetUsesSameHTTPSPortAndBackendPortForExplicitRoot(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https --tcp"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --set-path=/ --https=3000 http://127.0.0.1:3000":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	_, err := adapter.Set(context.Background(), ExposureChange{
		Target: target, Mode: exposuredata.ExposureServe, ProviderKey: "serve:https=3000",
		Path: "/", HTTPSPort: 3000, HTTPSRoot: true, Backend: "http://127.0.0.1:3000",
		Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --bg --yes --set-path=/ --https=3000 http://127.0.0.1:3000" {
		t.Fatalf("same-port HTTPS root changed the listener or backend port: %v", calls)
	}
	for _, command := range calls {
		if strings.HasPrefix(command, "funnel ") && command != "funnel --help" && command != "funnel status --json" {
			t.Fatalf("private root setup invoked a Funnel mutation: %v", calls)
		}
	}
}

func TestSetConfiguresExplicitFunnelHTTPSRootOnSupportedPort(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "funnel --bg --yes --set-path=/ --https=10000 http://127.0.0.1:3000":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	_, err := adapter.Set(context.Background(), ExposureChange{
		Target: target, Mode: exposuredata.ExposureFunnel, ProviderKey: "funnel:https=10000",
		Path: "/", HTTPSPort: 10000, HTTPSRoot: true, Backend: "http://127.0.0.1:3000",
		Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "funnel --bg --yes --set-path=/ --https=10000 http://127.0.0.1:3000" {
		t.Fatalf("Funnel root did not preserve the exact HTTPS port and HTTP backend: %v", calls)
	}
}

func TestFunnelRemoveUsesExactListenerFlagAndNeverReset(t *testing.T) {
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.1\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --tcp off"}, nil
		case "funnel status --json":
			return runner.Result{Stdout: `{"TCP":{"443":"127.0.0.1:3000"},"AllowFunnel":{"example.ts.net:443":true}}`}, nil
		case "serve status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "funnel --tcp=443 off":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	_, err = adapter.Remove(context.Background(), RouteSelector{ID: "funnel:tcp=443", Target: &target, Mode: exposuredata.ExposureFunnel, Backend: "127.0.0.1:3000", AllRoutesHash: RoutesHash(snapshot.Routes)}, hashIDs([]string{snapshot.Routes[0].ID}))
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "funnel --tcp=443 off" {
		t.Fatalf("last command=%q calls=%v", calls[len(calls)-1], calls)
	}
	for _, call := range calls {
		if strings.Contains(call, "reset") {
			t.Fatalf("broad funnel reset was invoked: %v", calls)
		}
	}
}

func TestCommandErrorPreservesTimeoutOverDiagnostics(t *testing.T) {
	err := (&Adapter{}).commandError("serve", runner.Result{ExitCode: -1, Stderr: "permission denied", Truncated: true}, context.DeadlineExceeded)
	if err.Code != fault.ErrTimeout || err.Exit != fault.ErrTimeout.ExitCode() {
		t.Fatalf("timeout was misclassified: %#v", err)
	}
}

func TestCommandErrorRedactsSensitiveStderr(t *testing.T) {
	adapter := &Adapter{Binary: "tailscale"}
	err := adapter.commandError("serve", runner.Result{ExitCode: 1, Stderr: "token=secret password=hunter2 Authorization: Bearer abc123 https://example.test/hook?opaque=private"}, errors.New("failed"))
	if strings.Contains(err.Message, "secret") || strings.Contains(err.Message, "hunter2") || strings.Contains(err.Message, "abc123") || strings.Contains(err.Message, "private") {
		t.Fatalf("secret leaked in error: %s", err.Message)
	}
	if got := redact("https://user:private@example.test/%ZZ?opaque=private%ZZ#fragment"); strings.Contains(got, "user") || strings.Contains(got, "private") || strings.Contains(got, "fragment") {
		t.Fatalf("malformed URL component leaked in error: %s", got)
	}
	if got := redact("https://user:private@example.test/hook#fragment"); strings.Contains(got, "user") || strings.Contains(got, "private") || strings.Contains(got, "fragment") {
		t.Fatalf("URL userinfo/fragment leaked in error: %s", got)
	}
	if got := redact("HTTPS://user:private@example.test/hook#fragment"); strings.Contains(got, "user") || strings.Contains(got, "private") || strings.Contains(got, "fragment") {
		t.Fatalf("uppercase URL userinfo/fragment leaked in error: %s", got)
	}
}

func FuzzParseStatusNeverPanics(f *testing.F) {
	for _, seed := range []string{`{"TCP":{"443":"127.0.0.1:8080"}}`, `{}`, `[]`, `not json`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = parseStatus(exposuredata.ExposureServe, []byte(text), time.Unix(1, 0))
	})
}

func TestSetHTTPPathRequiresExplicitNonRootSingleSegmentIntent(t *testing.T) {
	for _, test := range []struct {
		name      string
		path      string
		httpPath  bool
		wantError string
	}{
		{name: "root path", path: "/", httpPath: true, wantError: "non-root"},
		{name: "nested path", path: "/api/v2", httpPath: true, wantError: "non-root"},
		{name: "missing explicit intent", path: "/api", wantError: "explicit HTTP path intent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := &Adapter{Binary: "tailscale", Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
				command := strings.Join(args, " ")
				switch command {
				case "version":
					return runner.Result{Stdout: "1.102.4\n"}, nil
				case "serve --help":
					return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
				case "funnel --help":
					return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
				default:
					return runner.Result{}, errors.New("unexpected command: " + command)
				}
			})}
			targetRoute := target.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}
			_, err := adapter.Set(context.Background(), ExposureChange{
				Target: targetRoute, Mode: exposuredata.ExposureServe, ProviderKey: "serve:https=443", Path: test.path, HTTPPath: test.httpPath,
				Preconditions: ExposurePrecondition{RouteIDsHash: RouteIDsHash(nil, targetRoute), AllRoutesHash: RoutesHash(nil)},
			})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected %q validation failure, got %v", test.wantError, err)
			}
		})
	}
}

func TestSetHTTPPathPreservesSiblingAndExactIPv6Backend(t *testing.T) {
	const status = `{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/docs":{"Proxy":"http://127.0.0.1:3000"}}}},"AllowFunnel":{"dev.example.ts.net:443":false}}`
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json":
			return runner.Result{Stdout: status}, nil
		case "funnel status --json":
			return runner.Result{Stdout: status}, nil
		case "serve --bg --yes --set-path=/api-v2 --https=443 http://[::1]:4321":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 1 || snapshot.Routes[0].Path != "/docs" {
		t.Fatalf("sibling route status=%#v err=%v", snapshot.Routes, err)
	}
	backendTarget := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	_, err = adapter.Set(context.Background(), ExposureChange{
		Target: backendTarget, Mode: exposuredata.ExposureServe, ProviderKey: "serve:https=443", Path: "/api-v2", HTTPPath: true,
		Preconditions: ExposurePrecondition{RouteIDsHash: RouteIDsHash(snapshot.Routes, backendTarget), AllRoutesHash: RoutesHash(snapshot.Routes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --bg --yes --set-path=/api-v2 --https=443 http://[::1]:4321" {
		t.Fatalf("unexpected exact path command: %q", calls[len(calls)-1])
	}
	for _, call := range calls {
		if strings.Contains(call, "reset") || strings.Contains(call, "clear") {
			t.Fatalf("sibling handlers were changed with a broad reset: %v", calls)
		}
	}
}

func TestSetHTTPPathSuggestsExplicitAliasWhenTailscaleRejectsNumericIPv6(t *testing.T) {
	listener := target.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --set-path=/api --https=443 http://[::1]:4321":
			return runner.Result{Stderr: "unknown proxy destination: http://[::1]:4321", ExitCode: 1}, errors.New("exit status 1")
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	_, err := adapter.Set(context.Background(), ExposureChange{
		Target: listener, Mode: exposuredata.ExposureServe, ProviderKey: "serve:https=443", Path: "/api", HTTPPath: true,
		Backend: "http://[::1]:4321", Preconditions: ExposurePrecondition{RouteIDsHash: RouteIDsHash(nil, listener), AllRoutesHash: RoutesHash(nil)},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown proxy destination") {
		t.Fatalf("numeric IPv6 proxy rejection was not preserved: %v", err)
	}
	remediation := fault.AsAppError(err).Remediation
	for _, want := range []string{"--localhost-backend", "disable", "exact", "weakens", "TUI preserves numeric IPv6 backends"} {
		if !strings.Contains(remediation, want) {
			t.Fatalf("numeric IPv6 failure omitted %q remediation: %q", want, remediation)
		}
	}
	if strings.Contains(remediation, "Ctrl+B") {
		t.Fatalf("numeric IPv6 failure remediation offered an obsolete TUI shortcut: %q", remediation)
	}
}

func TestSetHTTPPathRejectsCollisionAndEndpointVisibilityConflict(t *testing.T) {
	cases := []struct {
		name, status, mode string
		want               string
	}{
		{name: "duplicate path", status: `{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/api":{"Proxy":"http://127.0.0.1:3000"}}}},"AllowFunnel":{"dev.example.ts.net:443":false}}`, mode: "serve", want: "already configured"},
		{name: "visibility conflict", status: `{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/docs":{"Proxy":"http://127.0.0.1:3000"}}}},"AllowFunnel":{"dev.example.ts.net:443":true}}`, mode: "serve", want: "visibility"},
		{name: "unaddressable existing route", status: `{"Web":{"dev.example.ts.net":{"Handlers":{"/docs":{"Proxy":"http://127.0.0.1:3000"}}}}}`, mode: "serve", want: "no exact endpoint selector"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
				command := strings.Join(args, " ")
				calls = append(calls, command)
				switch command {
				case "version":
					return runner.Result{Stdout: "1.102.4\n"}, nil
				case "serve --help":
					return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
				case "funnel --help":
					return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
				case "serve status --json", "funnel status --json":
					return runner.Result{Stdout: test.status}, nil
				default:
					return runner.Result{}, errors.New("unexpected command: " + command)
				}
			})}
			snapshot, err := adapter.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Set(context.Background(), ExposureChange{Target: target.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}, Mode: exposuredata.ExposureMode(test.mode), ProviderKey: "serve:https=443", Path: "/api", HTTPPath: true, Preconditions: ExposurePrecondition{RouteIDsHash: RouteIDsHash(nil, target.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}), AllRoutesHash: RoutesHash(snapshot.Routes)}})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q conflict, got %v", test.want, err)
			}
			for _, call := range calls {
				if strings.Contains(call, "--set-path=/api") {
					t.Fatalf("mutating command ran despite conflict: %v", calls)
				}
			}
		})
	}
}

func TestRemoveFunnelHTTPPathUsesExactHandlerAndPreservesSibling(t *testing.T) {
	const status = `{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/api":{"Proxy":"http://127.0.0.1:3000"},"/docs":{"Proxy":"http://127.0.0.1:3001"}}}},"AllowFunnel":{"dev.example.ts.net:443":true}}`
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: status}, nil
		case "funnel --set-path=/api --https=443 off":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 2 {
		t.Fatalf("routes=%#v err=%v", snapshot.Routes, err)
	}
	var targetRoute target.Target
	var pathRoute exposuredata.ExposureRoute
	for _, route := range snapshot.Routes {
		if route.Path == "/api" {
			targetRoute, pathRoute = route.Target, route
		}
	}
	if pathRoute.Mode != exposuredata.ExposureFunnel || targetRoute.Port != 3000 {
		t.Fatalf("Funnel path identity was not parsed exactly: %#v", pathRoute)
	}
	_, err = adapter.Remove(context.Background(), RouteSelector{ID: "funnel:https=443", Target: &targetRoute, Mode: exposuredata.ExposureFunnel, Path: pathRoute.Path, Backend: pathRoute.Backend, AllRoutesHash: RoutesHash(snapshot.Routes)}, RouteIDsHash(snapshot.Routes, targetRoute))
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "funnel --set-path=/api --https=443 off" {
		t.Fatalf("exact Funnel path removal command=%q", calls[len(calls)-1])
	}
}

func TestRemoveHTTPPathRejectsRawTCPSelector(t *testing.T) {
	targetRoute := target.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		calls = append(calls, strings.Join(args, " "))
		return runner.Result{}, nil
	})}
	_, err := adapter.Remove(context.Background(), RouteSelector{
		ID: "serve:tcp=443", Target: &targetRoute, Mode: exposuredata.ExposureServe,
		Path: "/api", Backend: "http://127.0.0.1:3000", AllRoutesHash: "routes-hash",
	}, "route-hash")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "https listener selector") {
		t.Fatalf("named path removal accepted raw TCP selector: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("invalid path/TCP selector reached provider: %v", calls)
	}
}

func TestRemoveHTTPPathUsesExactHandlerAndPreservesSibling(t *testing.T) {
	const status = `{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/api":{"Proxy":"http://127.0.0.1:3000"},"/docs":{"Proxy":"http://127.0.0.1:3001"}}}},"AllowFunnel":{"dev.example.ts.net:443":false}}`
	calls := []string{}
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		calls = append(calls, command)
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https value --tcp value --set-path value"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset --https value --tcp value --set-path value off"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: status}, nil
		case "serve --set-path=/api --bg --https=443 off":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 2 {
		t.Fatalf("routes=%#v err=%v", snapshot.Routes, err)
	}
	var targetRoute target.Target
	var pathRoute exposuredata.ExposureRoute
	for _, route := range snapshot.Routes {
		if route.Path == "/api" {
			targetRoute, pathRoute = route.Target, route
		}
	}
	_, err = adapter.Remove(context.Background(), RouteSelector{ID: "serve:https=443", Target: &targetRoute, Mode: exposuredata.ExposureServe, Path: pathRoute.Path, Backend: pathRoute.Backend, AllRoutesHash: RoutesHash(snapshot.Routes)}, RouteIDsHash(snapshot.Routes, targetRoute))
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --set-path=/api --bg --https=443 off" {
		t.Fatalf("exact path removal command=%q", calls[len(calls)-1])
	}
}
