package tailscale

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/runner"
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
	if _, err := adapter.Status(context.Background()); err == nil || model.AsAppError(err).Code != model.ErrUnknown {
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
	readiness, err := adapter.Readiness(context.Background(), ReadinessOptions{ServeProbeVersion: "1.80.0"})
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Status != model.ReadinessReady {
		t.Fatalf("overall readiness = %s, want ready", readiness.Status)
	}
	var serve, funnel model.ModeReadiness
	for _, mode := range readiness.Modes {
		if mode.Mode == model.ExposureServe {
			serve = mode
		}
		if mode.Mode == model.ExposureFunnel {
			funnel = mode
		}
	}
	if serve.Status != model.ReadinessReady || !serve.Probe {
		t.Fatalf("serve readiness = %#v", serve)
	}
	if funnel.Status != model.ReadinessReady || funnel.Probe || len(funnel.Checks) == 0 || funnel.Checks[0].Status != model.ReadinessReady {
		t.Fatalf("funnel readiness = %#v", funnel)
	}
}

func TestListPreservesDependencyExitClassWhenRoutesAreUnavailable(t *testing.T) {
	adapter := &Adapter{Binary: "/missing/tailscale", Now: time.Now}
	snapshot, err := adapter.List(context.Background())
	if err == nil || snapshot.Error == nil || snapshot.Error.Code != model.ErrDependency || model.AsAppError(err).Code != model.ErrDependency {
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
	if err == nil || snapshot.Authoritative || snapshot.Error == nil || snapshot.Error.Code != model.ErrUnknown {
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
	readiness, err := adapter.Readiness(context.Background(), ReadinessOptions{})
	if err == nil || len(readiness.Modes) != 2 {
		t.Fatalf("expected error and two mode results: readiness=%#v err=%v", readiness, err)
	}
	for _, mode := range readiness.Modes {
		if mode.Status != model.ReadinessNotReady && mode.Status != model.ReadinessUnknown {
			t.Fatalf("mode %s hid readiness failure: %s", mode.Mode, mode.Status)
		}
	}
}

func TestParseListenerSelectorAndRouteFingerprint(t *testing.T) {
	selector, err := ParseListenerSelector("serve:https=443", model.ExposureServe)
	if err != nil || selector.Transport != "https" || selector.Port != 443 {
		t.Fatalf("selector=%#v err=%v", selector, err)
	}
	if _, err := ParseListenerSelector("funnel:svc:api", model.ExposureFunnel); err == nil {
		t.Fatal("service selector was accepted as a deterministic listener")
	}
	first := model.ExposureRoute{ID: "route", ProviderKey: "serve:https=443", Target: model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, Mode: model.ExposureServe, URL: "https://dev.ts.net:443", Backend: "http://127.0.0.1:3000"}
	second := first
	second.Backend = "tcp://127.0.0.1:3000"
	if RouteFingerprint(first) == RouteFingerprint(second) {
		t.Fatal("route fingerprint ignored backend identity")
	}
}

func TestParseStatusFindsTCPRoute(t *testing.T) {
	routes, err := parseStatus(model.ExposureServe, []byte(`{"TCP":{"443":"127.0.0.1:8080"}}`), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes: %#v", len(routes), routes)
	}
	if routes[0].Target.Address != "127.0.0.1" || routes[0].Target.Port != 8080 || routes[0].Mode != model.ExposureServe || routes[0].ProviderKey != "serve:tcp=443" {
		t.Fatalf("unexpected route: %#v", routes[0])
	}
}

func TestParseStatusAcceptsFunnelPermissionWithoutHandler(t *testing.T) {
	routes, err := parseStatus(model.ExposureFunnel, []byte(`{"AllowFunnel":{"dev.example.ts.net:10000":true}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 0 {
		t.Fatalf("permission-only Funnel status was not treated as an authoritative empty route set: routes=%#v err=%v", routes, err)
	}
}

func TestParseStatusRejectsUnrecognizedNonemptyShape(t *testing.T) {
	if _, err := parseStatus(model.ExposureServe, []byte(`{"unexpected":"value"}`), time.Unix(1, 0)); err == nil {
		t.Fatal("unrecognized status shape was treated as authoritative empty state")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"Target":{"unexpected":true}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("non-string target field was treated as authoritative")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"TCP":null}`), time.Unix(1, 0)); err == nil {
		t.Fatal("null TCP field was treated as authoritative")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"TCP":{"443":"not-a-target"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed TCP route was treated as authoritative")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"TCP":{"443":{"HTTPS":true}}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("recognized route metadata without a target was treated as empty state")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"Service":"svc:missing-target"}`), time.Unix(1, 0)); err == nil {
		t.Fatal("recognized service metadata without a route was treated as empty state")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"Web":{"dev.example.ts.net":"not-a-route"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed Web route was treated as authoritative")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"Web":{"dev.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000","Backend":"http://127.0.0.1:3001"}}}}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("conflicting handler targets were treated as authoritative")
	}
	if _, err := parseStatus(model.ExposureServe, []byte(`{"TCP":{"443":"127.0.0.1:8080"},"AllowFunnel":{"dev.example.ts.net:443":"true"}}`), time.Unix(1, 0)); err == nil {
		t.Fatal("malformed AllowFunnel permission was treated as private Serve")
	}
	if routes, err := parseStatus(model.ExposureServe, []byte(`{}`), time.Unix(1, 0)); err != nil || len(routes) != 0 {
		t.Fatalf("empty status should represent no routes: routes=%#v err=%v", routes, err)
	}
	if routes, err := parseStatus(model.ExposureServe, []byte(`{"tcp":{"443":"127.0.0.1:8080"}}`), time.Unix(1, 0)); err != nil || len(routes) != 1 {
		t.Fatalf("case-insensitive TCP status was not parsed: routes=%#v err=%v", routes, err)
	}
	if routes, err := parseStatus(model.ExposureServe, []byte(`{"TCP":{"443":{"proxy":"127.0.0.1:8080"}}}`), time.Unix(1, 0)); err != nil || len(routes) != 1 {
		t.Fatalf("case-insensitive TCP target field was not parsed: routes=%#v err=%v", routes, err)
	} else if routes[0].ProviderKey != "serve:tcp=443" {
		t.Fatalf("TCP container was misclassified as HTTPS: %#v", routes[0])
	}
}

func TestParseStatusHandlesUppercaseURLRouteKeys(t *testing.T) {
	routes, err := parseStatus(model.ExposureServe, []byte(`{"HTTPS://DEV.example.ts.net:443":{"Proxy":"http://127.0.0.1:3000"}}`), time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "serve:https=443" || routes[0].URL != "HTTPS://DEV.example.ts.net:443" {
		t.Fatalf("unexpected uppercase URL route: %#v", routes[0])
	}
}

func TestParseStatusFindsWebProxyAndExactPublicPortSelector(t *testing.T) {
	fixture := []byte(`{"TCP":{"443":{"HTTPS":true}},"Web":{"dev.example.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`)
	routes, err := parseStatus(model.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	if routes[0].ProviderKey != "serve:https=443" || routes[0].URL != "https://dev.example.ts.net:443" || routes[0].Target.Port != 3000 {
		t.Fatalf("unexpected web route: %#v", routes[0])
	}
}

func TestTargetMatchesIPv4AndIPv6Loopback(t *testing.T) {
	ipv4 := model.Target{Address: "127.0.0.1", Port: 4323, Protocol: "tcp"}
	ipv6 := model.Target{Address: "::1", Port: 4323, Protocol: "tcp"}
	if !targetMatches(ipv4, ipv6) || !targetMatches(ipv6, ipv4) {
		t.Fatal("loopback families were not matched")
	}
}

func TestParseStatusAcceptsTailscaleUnbracketedIPv6Proxy(t *testing.T) {
	fixture := []byte(`{"TCP":{"4322":{"HTTPS":true}},"Web":{"dev.example.ts.net:4322":{"Handlers":{"/":{"Proxy":"http://::1:4322"}}}}}`)
	routes, err := parseStatus(model.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 || routes[0].Target.Address != "::1" || routes[0].Target.Port != 4322 {
		t.Fatalf("unbracketed IPv6 proxy was not parsed: routes=%#v err=%v", routes, err)
	}
}

func TestParseStatusUsesAllowFunnelToSeparateServeAndFunnel(t *testing.T) {
	private := []byte(`{"TCP":{"3000":{"HTTPS":true}},"Web":{"dev.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://0.0.0.0:3000"}}}}}`)
	serveRoutes, err := parseStatus(model.ExposureServe, private, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 1 {
		t.Fatalf("private Serve routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err := parseStatus(model.ExposureFunnel, private, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 0 {
		t.Fatalf("private config was incorrectly classified as Funnel: routes=%#v err=%v", funnelRoutes, err)
	}

	public := []byte(`{"TCP":{"3000":{"HTTPS":true}},"Web":{"dev.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://0.0.0.0:3000"}}}},"AllowFunnel":{"dev.example.ts.net:3000":true}}`)
	serveRoutes, err = parseStatus(model.ExposureServe, public, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 0 {
		t.Fatalf("public config was incorrectly retained as Serve: routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err = parseStatus(model.ExposureFunnel, public, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 1 || funnelRoutes[0].Mode != model.ExposureFunnel {
		t.Fatalf("public config was not classified as Funnel: routes=%#v err=%v", funnelRoutes, err)
	}

	mixed := []byte(`{"Web":{"private.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}},"public.example.ts.net:3000":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3001"}}}},"AllowFunnel":{"private.example.ts.net:3000":false,"public.example.ts.net:3000":true}}`)
	serveRoutes, err = parseStatus(model.ExposureServe, mixed, time.Unix(1, 0))
	if err != nil || len(serveRoutes) != 1 || serveRoutes[0].URL != "https://private.example.ts.net:3000" {
		t.Fatalf("host-specific private Serve route was misclassified: routes=%#v err=%v", serveRoutes, err)
	}
	funnelRoutes, err = parseStatus(model.ExposureFunnel, mixed, time.Unix(1, 0))
	if err != nil || len(funnelRoutes) != 1 || funnelRoutes[0].URL != "https://public.example.ts.net:3000" {
		t.Fatalf("host-specific public Funnel route was misclassified: routes=%#v err=%v", funnelRoutes, err)
	}
}

func TestParseStatusPreservesServicePathAndBackendIdentity(t *testing.T) {
	fixture := []byte(`{"Service":"svc:api","Web":{"dev.example.ts.net:8443":{"Handlers":{"/admin":{"Proxy":"http://127.0.0.1:3000/admin"}}}}}`)
	routes, err := parseStatus(model.ExposureServe, fixture, time.Unix(1, 0))
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes=%#v err=%v", routes, err)
	}
	route := routes[0]
	if route.Service != "svc:api" || route.Path != "/admin" || route.ProviderKey != "serve:https=8443" || route.Backend != "http://127.0.0.1:3000/admin" {
		t.Fatalf("scoped route identity was not preserved: %#v", route)
	}
}

func TestParseStatusDoesNotGuessRemovalSelectorWhenPublicPortIsMissing(t *testing.T) {
	routes, err := parseStatus(model.ExposureServe, []byte(`{"Web":{"dev.example.ts.net":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3000"}}}}}`), time.Unix(1, 0))
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
	if err != nil || !snapshot.Authoritative || len(snapshot.Routes) != 1 || snapshot.Routes[0].Mode != model.ExposureServe {
		t.Fatalf("private Serve status was duplicated or misclassified: routes=%#v authoritative=%t err=%v", snapshot.Routes, snapshot.Authoritative, err)
	}
}

func TestTargetArgumentUsesReachableBackendAddress(t *testing.T) {
	cases := []struct {
		name   string
		target model.Target
		want   string
	}{
		{name: "loopback", target: model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}, want: "127.0.0.1:3000"},
		{name: "ipv6 loopback", target: model.Target{Address: "::1", Port: 3000, Protocol: "tcp"}, want: "http://localhost:3000"},
		{name: "ipv4 wildcard", target: model.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}, want: "http://127.0.0.1:3000"},
		{name: "ipv6 wildcard", target: model.Target{Address: "::", Port: 3000, Protocol: "tcp"}, want: "http://[::1]:3000"},
		{name: "local network", target: model.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, want: "http://192.168.1.20:3000"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := targetArgument(test.target); got != test.want {
				t.Fatalf("targetArgument(%#v) = %q, want %q", test.target, got, test.want)
			}
		})
	}
}

func TestSetUsesLoopbackBackendForWildcardTarget(t *testing.T) {
	adapter := &Adapter{Binary: "tailscale", Now: time.Now, Runner: runner.FuncRunner(func(_ context.Context, _ string, args ...string) (runner.Result, error) {
		command := strings.Join(args, " ")
		switch command {
		case "version":
			return runner.Result{Stdout: "1.102.4\n"}, nil
		case "serve --help":
			return runner.Result{Stdout: "status clear --https --tcp"}, nil
		case "funnel --help":
			return runner.Result{Stdout: "status reset"}, nil
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --tcp=3000 tcp://127.0.0.1:3000":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := model.Target{Address: "0.0.0.0", Port: 3000, Protocol: "tcp"}.Normalized()
	if _, err := adapter.Set(context.Background(), ExposureChange{
		Target: target,
		Mode:   model.ExposureServe,
		Preconditions: ExposurePrecondition{
			RouteIDsHash:  hashIDs(nil),
			AllRoutesHash: RoutesHash(nil),
		},
	}); err != nil {
		t.Fatal(err)
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
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	_, err = adapter.Set(context.Background(), ExposureChange{Target: target, Mode: model.ExposureServe, Preconditions: ExposurePrecondition{RouteIDsHash: hashIDs(nil), AllRoutesHash: RoutesHash(snapshot.Routes)}})
	if err == nil || model.AsAppError(err).Code != model.ErrUnsafe {
		t.Fatalf("endpoint collision was not refused: %v", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "serve --bg") {
			t.Fatalf("mutating command ran despite endpoint collision: %v", calls)
		}
	}
}

func TestSetRestoresExactProviderListenerSelector(t *testing.T) {
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
		case "serve status --json", "funnel status --json":
			return runner.Result{Stdout: `{}`}, nil
		case "serve --bg --yes --https=443 http://127.0.0.1:3000":
			return runner.Result{}, nil
		default:
			return runner.Result{}, errors.New("unexpected command: " + command)
		}
	})}
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	_, err := adapter.Set(context.Background(), ExposureChange{
		Target:      target,
		Mode:        model.ExposureServe,
		ProviderKey: "serve:https=443",
		Preconditions: ExposurePrecondition{
			RouteIDsHash:  hashIDs(nil),
			AllRoutesHash: RoutesHash(nil),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "serve --bg --yes --https=443 http://127.0.0.1:3000" {
		t.Fatalf("restore selector was not preserved: %v", calls)
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
	target := model.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	snapshot, err := adapter.List(context.Background())
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	_, err = adapter.Remove(context.Background(), RouteSelector{ID: "funnel:tcp=443", Target: &target, Mode: model.ExposureFunnel, Backend: "127.0.0.1:3000", AllRoutesHash: RoutesHash(snapshot.Routes)}, hashIDs([]string{snapshot.Routes[0].ID}))
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
	if err.Code != model.ErrTimeout || err.Exit != model.ErrTimeout.ExitCode() {
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
		_, _ = parseStatus(model.ExposureServe, []byte(text), time.Unix(1, 0))
	})
}
