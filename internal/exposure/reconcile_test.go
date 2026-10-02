package exposure

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	readinessmodel "github.com/arrokh/tailge/internal/readiness"
	"github.com/arrokh/tailge/internal/tailscale"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

type fakeDiscoverer struct {
	snapshot discovery.ListenerSnapshot
	err      error
}

func (f *fakeDiscoverer) List(context.Context) (discovery.ListenerSnapshot, error) {
	return f.snapshot, f.err
}

type disappearingDiscoverer struct {
	target targetmodel.Target
	calls  int
}

func (d *disappearingDiscoverer) List(context.Context) (discovery.ListenerSnapshot, error) {
	d.calls++
	if d.calls == 1 {
		return discovery.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []discovery.Listener{testListener(d.target, "api")}}, nil
	}
	return discovery.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []discovery.Listener{}}, nil
}

type sequencedDiscoverer struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (d *sequencedDiscoverer) List(context.Context) (discovery.ListenerSnapshot, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	if call == 1 {
		close(d.firstStarted)
		<-d.releaseFirst
	}
	target := targetmodel.Target{Address: "127.0.0.1", Port: 10000 + call, Protocol: "tcp"}.Normalized()
	return discovery.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}, nil
}

type fakeProvider struct {
	mu               sync.Mutex
	snapshot         exposuredata.ExposureSnapshot
	caps             tailscale.Capabilities
	readiness        readinessmodel.Readiness
	readinessErr     error
	sets             []tailscale.ExposureChange
	removes          []tailscale.RouteSelector
	setErr           error
	setErrMode       exposuredata.ExposureMode
	setProviderKey   string
	removeErr        error
	afterRemoveRoute *exposuredata.ExposureRoute
	setStarted       chan struct{}
	releaseSet       chan struct{}
	setOnce          sync.Once
	blockList        <-chan struct{}
}

func (f *fakeProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return f.caps, nil
}
func (f *fakeProvider) Readiness(context.Context, tailscale.ReadinessOptions) (readinessmodel.Readiness, error) {
	return f.readiness, f.readinessErr
}
func (f *fakeProvider) List(context.Context) (exposuredata.ExposureSnapshot, error) {
	if f.blockList != nil {
		<-f.blockList
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copySnapshot := f.snapshot
	copySnapshot.Routes = append([]exposuredata.ExposureRoute(nil), f.snapshot.Routes...)
	return copySnapshot, nil
}
func (f *fakeProvider) Set(_ context.Context, change tailscale.ExposureChange) (exposuredata.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, change)
	if f.setStarted != nil {
		f.setOnce.Do(func() { close(f.setStarted) })
		<-f.releaseSet
	}
	if f.setErr != nil && (f.setErrMode == "" || f.setErrMode == change.Mode) {
		return exposuredata.OperationReceipt{}, f.setErr
	}
	providerKey := change.ProviderKey
	if providerKey == "" {
		providerKey = tailscale.ProviderKeyForTargetWithCapabilities(change.Mode, change.Target, f.caps)
	}
	id := providerKey
	kind := exposuredata.RouteKindRawTCP
	url := ""
	backend := change.Backend
	if change.HTTPPath || change.HTTPSRoot {
		kind = exposuredata.RouteKindHTTPPath
		if change.HTTPSRoot {
			kind = exposuredata.RouteKindHTTPSRoot
		}
		id = targetmodel.StableID(string(change.Mode), change.Target.Key(), providerKey, change.Path, change.Backend)
		url = "https://dev.example.ts.net" + change.Path
		if change.HTTPSPort != 443 {
			url = "https://dev.example.ts.net:" + strconv.Itoa(change.HTTPSPort) + change.Path
		}
		if backendTarget, err := targetmodel.ParseTarget(change.Backend, "tcp"); err == nil {
			change.Target = backendTarget
		}
	}
	if f.setProviderKey != "" {
		providerKey = f.setProviderKey
	}
	f.snapshot.Routes = append(f.snapshot.Routes, exposuredata.ExposureRoute{ID: id, ProviderKey: providerKey, Kind: kind, Service: change.Service, Path: change.Path, Backend: backend, Target: change.Target, Mode: change.Mode, URL: url, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive, LastSeen: time.Now()})
	return exposuredata.OperationReceipt{ID: id, Verified: false}, nil
}
func (f *fakeProvider) Remove(_ context.Context, selector tailscale.RouteSelector, _ string) (exposuredata.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, selector)
	if f.removeErr != nil {
		return exposuredata.OperationReceipt{}, f.removeErr
	}
	for i, route := range f.snapshot.Routes {
		if (route.ID == selector.ID || route.ProviderKey == selector.ID) && (selector.Path == "" || route.Path == selector.Path) && (selector.Backend == "" || route.Backend == selector.Backend) && (selector.Mode == "" || selector.Mode == exposuredata.ExposureDisabled || route.Mode == selector.Mode) {
			f.snapshot.Routes = append(f.snapshot.Routes[:i], f.snapshot.Routes[i+1:]...)
			break
		}
	}
	if f.afterRemoveRoute != nil {
		f.snapshot.Routes = append(f.snapshot.Routes, *f.afterRemoveRoute)
		f.afterRemoveRoute = nil
	}
	return exposuredata.OperationReceipt{ID: "remove-" + selector.ID}, nil
}

func testListener(target targetmodel.Target, name string) discovery.Listener {
	return discovery.Listener{ID: name, Target: target, Name: name, Process: name, PID: 10, Scope: targetmodel.ScopeForAddress(target.Address), Metadata: discovery.MetadataComplete}
}

func ready() readinessmodel.Readiness {
	return readinessmodel.Readiness{Status: readinessmodel.ReadinessReady, Modes: []readinessmodel.ModeReadiness{
		{Mode: exposuredata.ExposureServe, Status: readinessmodel.ReadinessReady},
		{Mode: exposuredata.ExposureFunnel, Status: readinessmodel.ReadinessReady},
	}}
}

func TestMatchesDoesNotTreatWildcardRouteAsEverySpecificListener(t *testing.T) {
	wildcard := targetmodel.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}
	loopback := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}
	otherLocal := targetmodel.Target{Address: "192.168.1.2", Port: 8080, Protocol: "tcp"}
	if Matches(wildcard, loopback) || Matches(wildcard, otherLocal) {
		t.Fatal("wildcard route matched a specific listener")
	}
	if !Matches(loopback, wildcard) {
		t.Fatal("loopback route did not match wildcard listener")
	}
}

func TestMatchesTreatsIPv4AndIPv6LoopbackAsLogicalLoopback(t *testing.T) {
	ipv4 := targetmodel.Target{Address: "127.0.0.1", Port: 4323, Protocol: "tcp"}
	ipv6 := targetmodel.Target{Address: "::1", Port: 4323, Protocol: "tcp"}
	if !Matches(ipv4, ipv6) || !Matches(ipv6, ipv4) {
		t.Fatal("loopback families were not correlated")
	}
}

func TestReconcileTreatsErrorBearingSnapshotsAsUnknown(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	snapshot := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}, Error: &fault.SafeError{Code: fault.ErrUnknown, Message: "partial"}}
	view := Reconcile(snapshot, exposuredata.ExposureSnapshot{Authoritative: true}, nil)
	if view.Listeners.Authoritative || len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureUnknown {
		t.Fatalf("error-bearing listener snapshot was treated as safe: %#v", view)
	}
}

func TestReconcileWarnsForNetworkAndSensitiveListeners(t *testing.T) {
	target := targetmodel.Target{Address: "0.0.0.0", Port: 5432, Protocol: "tcp"}.Normalized()
	listener := testListener(target, "postgres")
	view := Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{listener}}, exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, nil)
	if len(view.Items) != 1 || !strings.Contains(view.Items[0].Warning, "local network") || !strings.Contains(view.Items[0].Warning, "sensitive") {
		t.Fatalf("missing listener warnings: %#v", view.Items)
	}
}

func TestReconcileAdvisesAliasForNumericIPv6HTTPSBackend(t *testing.T) {
	target := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	route := exposuredata.ExposureRoute{ID: "api-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://[::1]:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	view := Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}, exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, nil)
	if len(view.Items) != 1 || !strings.Contains(view.Items[0].Recommendation, "unknown proxy destination") || !strings.Contains(view.Items[0].Recommendation, "--localhost-backend") {
		t.Fatalf("numeric IPv6 HTTPS route lacks actionable backend guidance: %#v", view.Items)
	}

	aliasTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	route.Backend, route.Target = "http://localhost:4321", aliasTarget
	view = Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}, exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, nil)
	if len(view.Items) == 0 || view.Items[0].Recommendation != "" {
		t.Fatalf("localhost alias route still recommends changing backend: %#v", view.Items)
	}
}

func TestReconcileShowsInactiveConfiguredRouteRecommendation(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	route := exposuredata.ExposureRoute{ID: "serve:api", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:8080", URL: "https://dev.example.ts.net/api", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	view := Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{}}, exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, nil)
	if len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureInactive {
		t.Fatalf("unexpected view: %#v", view.Items)
	}
	if view.Items[0].Recommendation == "" || view.Items[0].Routes[0].State != exposuredata.ExposureInactive || view.Items[0].Routes[0].Kind != exposuredata.RouteKindHTTPPath || view.Items[0].Routes[0].Path != "/api" {
		t.Fatalf("missing inactive recommendation: %#v", view.Items[0])
	}
}

func TestReconcileMarksAmbiguousListenerAndRoute(t *testing.T) {
	target := targetmodel.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}.Normalized()
	listeners := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{
		testListener(targetmodel.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}.Normalized(), "one"),
		testListener(targetmodel.Target{Address: "::", Port: 8080, Protocol: "tcp"}.Normalized(), "two"),
	}}
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}}}
	view := Reconcile(listeners, routes, nil)
	if len(view.Items) != 3 {
		t.Fatalf("expected two listener rows and one route row, got %d", len(view.Items))
	}
	ambiguous := 0
	for _, item := range view.Items {
		if item.State == exposuredata.ExposureAmbiguous {
			ambiguous++
		}
	}
	if ambiguous < 2 {
		t.Fatalf("ambiguous route was not propagated to listeners: %#v", view.Items)
	}
}

func TestReconcileKeepsDistinctExactRoutesActive(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	backendTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	listeners := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "blog")}}
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{
		{ID: "root-route", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "blog-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
	}}

	view := Reconcile(listeners, routes, nil)
	if len(view.Items) != 1 {
		t.Fatalf("expected one listener item, got %#v", view.Items)
	}
	item := view.Items[0]
	if item.State != exposuredata.ExposureActive || item.Mode != exposuredata.ExposureServe {
		t.Fatalf("distinct exact active routes were reported ambiguous: state=%q mode=%q warning=%q", item.State, item.Mode, item.Warning)
	}
	if len(item.Routes) != 2 || item.Routes[0].ID == item.Routes[1].ID {
		t.Fatalf("exact routes were not retained separately: %#v", item.Routes)
	}
	if strings.Contains(item.Warning, "multiple exposure routes match") {
		t.Fatalf("known exact routes retained an ambiguity warning: %q", item.Warning)
	}
}

func TestReconcileMixedExactRouteModesReportMultiple(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	listener := testListener(target, "node")
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{
		{ID: "private-path", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/private", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "public-tcp", ProviderKey: "funnel:tcp=10000", Kind: exposuredata.RouteKindRawTCP, Target: target, Mode: exposuredata.ExposureFunnel, State: exposuredata.ExposureActive},
	}}
	view := Reconcile(discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{listener}}, routes, nil)
	if len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureActive || view.Items[0].Mode != exposuredata.ExposureMultiple {
		t.Fatalf("mixed exact route modes were misreported: %#v", view.Items)
	}
}

func TestReconcileKeepsAmbiguousForIncompleteMultipleRoutes(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	backendTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	listeners := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "blog")}}
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{
		{ID: "root-route", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "unknown-route", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
	}}

	view := Reconcile(listeners, routes, nil)
	if len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureAmbiguous {
		t.Fatalf("route set with an incomplete selector was not kept ambiguous: %#v", view.Items)
	}
}

func TestReconcileKeepsAmbiguousForDuplicateExactSelector(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	backendTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	first := exposuredata.ExposureRoute{ID: "blog-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/blog", Backend: "http://localhost:4321", Target: backendTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	second := first
	second.ID = "duplicate-route-id"
	second.Backend = "http://127.0.0.1:4321"
	routes := exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{first, second}}
	listeners := discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "blog")}}

	view := Reconcile(listeners, routes, nil)
	if len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureAmbiguous {
		t.Fatalf("duplicate exact selectors with different route IDs were not kept ambiguous: %#v", view.Items)
	}
}

func TestRefreshRejectsLateOlderGeneration(t *testing.T) {
	discoverer := &sequencedDiscoverer{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{At: time.Now(), Authoritative: true}}
	controller := NewController(discoverer, provider)
	firstDone := make(chan View, 1)
	go func() {
		view, err := controller.Refresh(context.Background())
		if err != nil {
			t.Errorf("first refresh: %v", err)
		}
		firstDone <- view
	}()
	<-discoverer.firstStarted
	secondDone := make(chan View, 1)
	go func() {
		view, err := controller.Refresh(context.Background())
		if err != nil {
			t.Errorf("second refresh: %v", err)
		}
		secondDone <- view
	}()
	second := <-secondDone
	close(discoverer.releaseFirst)
	first := <-firstDone
	for name, view := range map[string]View{"first": first, "second": second} {
		if len(view.Items) != 1 || view.Items[0].Listener == nil || view.Items[0].Listener.Target.Port != 10002 {
			t.Fatalf("%s refresh published stale listener: %#v", name, view.Items)
		}
	}
}

func TestRefreshReturnsAtContextDeadlineWhenProviderIgnoresCancellation(t *testing.T) {
	release := make(chan struct{})
	provider := &fakeProvider{
		snapshot:  exposuredata.ExposureSnapshot{At: time.Now(), Authoritative: true, Routes: []exposuredata.ExposureRoute{}},
		blockList: release,
	}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []discovery.Listener{testListener(targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}, "web")}}}
	controller := NewController(discoverer, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	view, err := controller.Refresh(ctx)
	if err == nil || fault.AsAppError(err).Code != fault.ErrTimeout {
		t.Fatalf("expected bounded timeout, view=%#v err=%v", view, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("refresh exceeded its context bound: %s", elapsed)
	}
	if len(view.Items) != 1 || view.Listeners.Authoritative == false || view.Exposures.Authoritative {
		t.Fatalf("partial timeout view was not preserved safely: %#v", view)
	}
	close(release)
}

func TestRefreshMarksLastKnownSourcesStaleAfterDegradation(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	controller := NewController(discoverer, provider)
	if _, err := controller.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	discoverer.err = context.DeadlineExceeded
	provider.snapshot.Authoritative = false
	view, err := controller.Refresh(context.Background())
	if err == nil || !view.Listeners.Stale || !view.Exposures.Stale || view.Listeners.Authoritative || view.Exposures.Authoritative {
		t.Fatalf("stale source state not preserved safely: view=%#v err=%v", view, err)
	}
	if len(view.Items) != 1 || view.Items[0].State != exposuredata.ExposureUnknown {
		t.Fatalf("degraded route/listener was presented as known: %#v", view.Items)
	}
}

func TestApplySerializesMutationsForOneTarget(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready(), setStarted: make(chan struct{}), releaseSet: make(chan struct{})}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	firstDone := make(chan error, 1)
	go func() {
		_, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second)
		firstDone <- err
	}()
	<-provider.setStarted
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("concurrent mutation was not rejected")
	}
	close(provider.releaseSet)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	view, err := controller.Refresh(context.Background())
	if err != nil || len(view.Items) != 1 || view.Items[0].DesiredMode != exposuredata.ExposureServe || view.Items[0].OperationState != exposuredata.ExposureSucceeded || view.Items[0].LastOperation == nil || !view.Items[0].LastOperation.Verified || len(view.Events) < 3 {
		t.Fatalf("operation lifecycle was not published: view=%#v err=%v", view, err)
	}
}

func TestApplyRevalidatesListenerBeforeMutation(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &disappearingDiscoverer{target: target}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second)
	if err == nil || fault.AsAppError(err).Code != fault.ErrInvalidInput || len(provider.sets) != 0 {
		t.Fatalf("mutation was not blocked after listener disappeared: err=%v sets=%d calls=%d", err, len(provider.sets), discoverer.calls)
	}
}

func TestApplyDoesNotAcceptTargetOnlyPostWriteMatch(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready(), setProviderKey: "serve:tcp=9999"}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, 3*time.Second)
	if err == nil || fault.AsAppError(err).Code != fault.ErrVerification {
		t.Fatalf("target-only route was accepted: err=%v", err)
	}
}

func TestApplyReportsFailureAfterRestoringPreviousMode(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := exposuredata.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Backend: "tcp://127.0.0.1:8080", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipExternal, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready(), setErr: errors.New("funnel provider failed"), setErrMode: exposuredata.ExposureFunnel}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureFunnel, true, true, time.Second); err == nil {
		t.Fatal("expected replacement failure")
	}
	if len(provider.sets) != 2 || provider.sets[0].Mode != exposuredata.ExposureFunnel || provider.sets[1].Mode != exposuredata.ExposureServe {
		t.Fatalf("expected failed replacement and Serve rollback, sets=%#v", provider.sets)
	}
	provider.mu.Lock()
	routes := append([]exposuredata.ExposureRoute(nil), provider.snapshot.Routes...)
	provider.mu.Unlock()
	if len(routes) != 1 || routes[0].Mode != exposuredata.ExposureServe {
		t.Fatalf("previous route was not restored: %#v", routes)
	}
}

func TestApplyRejectsReplacementWithoutExactRollbackSelector(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := exposuredata.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipExternal, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, exposuredata.ExposureFunnel, true, true, time.Second)
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnsupported {
		t.Fatalf("unsafe rollback selector was accepted: %v", err)
	}
	if len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("replacement mutated before proving rollback safety: removes=%d sets=%d", len(provider.removes), len(provider.sets))
	}
}

func TestDisableRequiresVerifiedRouteRemovalReadiness(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	route := exposuredata.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	readiness := ready()
	readiness.Modes[0].Status = readinessmodel.ReadinessReadOnly
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: readiness}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureDisabled, false, true, time.Second); err == nil {
		t.Fatal("disable was accepted without verified Serve readiness")
	}
	if len(provider.removes) != 0 {
		t.Fatal("route was removed while readiness was read-only")
	}
}

func TestApplyRouteDisablesOneOfMultipleRoutes(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	serve := exposuredata.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	funnel := exposuredata.ExposureRoute{ID: "funnel:api", ProviderKey: "funnel:api", Target: target, Mode: exposuredata.ExposureFunnel, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{serve, funnel}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	receipt, err := controller.ApplyRoute(context.Background(), target, "serve:api", false, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("selected route was not disabled: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.removes) != 1 || provider.removes[0].ID != "serve:api" {
		t.Fatalf("wrong route was removed: %#v", provider.removes)
	}
	if len(provider.snapshot.Routes) != 1 || provider.snapshot.Routes[0].ProviderKey != "funnel:api" {
		t.Fatalf("unselected route was changed: %#v", provider.snapshot.Routes)
	}
}

func TestApprovedBatchMutationsAllowPriorOtherTargetChanges(t *testing.T) {
	firstTarget := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	secondTarget := targetmodel.Target{Address: "127.0.0.1", Port: 8081, Protocol: "tcp"}.Normalized()
	firstRoute := exposuredata.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Target: firstTarget, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	secondRoute := exposuredata.ExposureRoute{ID: "serve:tcp=8081", ProviderKey: "serve:tcp=8081", Target: secondTarget, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{firstRoute, secondRoute}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(firstTarget, "first"), testListener(secondTarget, "second")}}}
	controller := NewController(discoverer, provider)
	allRoutesHash := tailscale.RoutesHash(provider.snapshot.Routes)
	firstApproval := MutationApproval{Target: firstTarget, RouteIDsHash: RouteIDsHash(provider.snapshot.Routes, firstTarget), TargetRoutesHash: RouteIdentityHash(provider.snapshot.Routes, firstTarget), AllRoutesHash: allRoutesHash, AllowOtherRouteChanges: true}
	secondApproval := MutationApproval{Target: secondTarget, RouteIDsHash: RouteIDsHash(provider.snapshot.Routes, secondTarget), TargetRoutesHash: RouteIdentityHash(provider.snapshot.Routes, secondTarget), AllRoutesHash: allRoutesHash, AllowOtherRouteChanges: true}
	first, err := controller.ApplyRouteApproved(context.Background(), firstTarget, firstRoute.ProviderKey, false, time.Second, firstApproval)
	if err != nil || !first.Verified {
		t.Fatalf("first batch disable failed: receipt=%#v err=%v", first, err)
	}
	second, err := controller.ApplyRouteApproved(context.Background(), secondTarget, secondRoute.ProviderKey, false, time.Second, secondApproval)
	if err != nil || !second.Verified {
		t.Fatalf("second batch disable failed after first route changed: receipt=%#v err=%v", second, err)
	}
	if len(provider.removes) != 2 {
		t.Fatalf("batch disabled %d routes, want 2: %#v", len(provider.removes), provider.removes)
	}
}

func TestApprovedBatchMutationRejectsChangedTargetRouteIdentity(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	route := exposuredata.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: target, RouteIDsHash: RouteIDsHash(provider.snapshot.Routes, target), TargetRoutesHash: RouteIdentityHash(provider.snapshot.Routes, target), AllowOtherRouteChanges: true}
	provider.mu.Lock()
	provider.snapshot.Routes[0].Backend = "tcp://127.0.0.1:9999"
	provider.mu.Unlock()
	if _, err := controller.ApplyRouteApproved(context.Background(), target, route.ProviderKey, false, time.Second, approval); err == nil {
		t.Fatal("batch approval accepted a changed target route identity")
	}
	if len(provider.removes) != 0 {
		t.Fatal("changed target route was removed")
	}
}

func TestApplyRouteModeRejectsAmbiguousModeSelection(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	first := exposuredata.ExposureRoute{ID: "serve:https", ProviderKey: "serve:https=443", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	second := exposuredata.ExposureRoute{ID: "serve:tcp", ProviderKey: "serve:tcp=443", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{first, second}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyRouteMode(context.Background(), target, exposuredata.ExposureServe, true, time.Second); err == nil || fault.AsAppError(err).Code != fault.ErrAmbiguous {
		t.Fatalf("ambiguous mode selection was accepted: %v", err)
	}
	if len(provider.removes) != 0 {
		t.Fatal("ambiguous route selection mutated provider state")
	}
}

func TestApprovedMutationRejectsChangedListenerAndRoutes(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8089, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: target, ListenerID: "api", ListenerPID: 10, ListenerProcess: "api", ListenerTarget: target, RouteIDsHash: RouteIDsHash(nil, target), AllRoutesHash: tailscale.RoutesHash(nil)}
	discoverer.snapshot.Listeners[0].Process = "replacement"
	if _, err := controller.ApplyApproved(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second, approval); err == nil {
		t.Fatal("changed listener was accepted after confirmation")
	}
	if len(provider.sets) != 0 {
		t.Fatal("provider mutated after approval fingerprint changed")
	}
}

func TestManagedOwnershipIsRevokedWhenRouteIdentityChanges(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8090, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second); err != nil {
		t.Fatalf("initial managed route failed: %v", err)
	}
	provider.mu.Lock()
	provider.snapshot.Routes[0].Backend = "tcp://127.0.0.1:8090"
	provider.mu.Unlock()
	view, err := controller.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if len(view.Items) != 1 || view.Items[0].Routes[0].Ownership == exposuredata.OwnershipManaged {
		t.Fatalf("changed route retained managed ownership: %#v", view.Items)
	}
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("changed external route was mutated without confirmation")
	}
}

func TestApplyRequiresFunnelConfirmationAndAuthoritativeSnapshots(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureFunnel, false, false, time.Second); err == nil {
		t.Fatal("funnel was accepted without confirmation")
	}
	if len(provider.sets) != 0 {
		t.Fatal("provider was mutated after missing confirmation")
	}
	provider.snapshot.Authoritative = false
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("non-authoritative exposure state was accepted")
	}
}

func TestApplyHTTPPathRejectsEmptyRootInsteadOfFallingBackToRawTCP(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPPath(context.Background(), listenerTarget, "", exposuredata.ExposureServe, false, time.Second); err == nil || !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("empty HTTP path silently fell back to raw TCP: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("provider mutated for the implicit root path: %#v", provider.sets)
	}
}

func TestRawTCPModeChangeCannotReplaceNamedHTTPPath(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	pathRoute := exposuredata.ExposureRoute{ID: "api-handler", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{pathRoute}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true, ServeHTTPS: true, FunnelHTTPS: true, ServePath: true, FunnelPath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), listenerTarget, exposuredata.ExposureFunnel, true, false, time.Second); err == nil || !strings.Contains(err.Error(), "explicit HTTP-path intent") {
		t.Fatalf("raw-TCP mode action replaced a named HTTP path: %v", err)
	}
	if len(provider.sets) != 0 || len(provider.removes) != 0 {
		t.Fatalf("raw-TCP mode action mutated a named HTTP path: sets=%#v removes=%#v", provider.sets, provider.removes)
	}
}

func TestDisableRefusesHTTPHandlerWithoutExactPathIdentity(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	root := exposuredata.ExposureRoute{ID: "root-handler", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Target: listenerTarget, Mode: exposuredata.ExposureServe, Backend: "http://127.0.0.1:4321", Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{root}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyRouteIdentity(context.Background(), listenerTarget, root.ID, false, time.Second); err == nil || !strings.Contains(err.Error(), "no exact named path identity") {
		t.Fatalf("handler without a path was not rejected safely: %v", err)
	}
	if len(provider.removes) != 0 {
		t.Fatalf("ambiguous root handler reached provider removal: %#v", provider.removes)
	}
}

func TestApplyHTTPPathRefusesUnidentifiedExistingProviderRoute(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	unidentified := exposuredata.ExposureRoute{ID: "unidentified", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Target: listenerTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{unidentified}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPPath(context.Background(), listenerTarget, "/api", exposuredata.ExposureServe, false, time.Second); err == nil || !strings.Contains(err.Error(), "no exact provider endpoint identity") {
		t.Fatalf("unidentified existing route was not blocked: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("provider mutated with incomplete existing identity: %#v", provider.sets)
	}
}

func TestVerifyAbsentDetectsHTTPSHandlerRemainingAfterVisibilityChange(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	selected := exposuredata.ExposureRoute{ID: "serve-api", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	remaining := selected
	remaining.ID, remaining.ProviderKey, remaining.Mode = "funnel-api", "funnel:https=443", exposuredata.ExposureFunnel
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{remaining}}}
	dependencies := exactOperationDependencies{provider: provider}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verified, _, err := dependencies.verifyAbsent(ctx, selected)
	if verified || err == nil {
		t.Fatalf("same HTTPS path was reported removed after visibility changed: verified=%t err=%v", verified, err)
	}
}

func TestApplyRouteIdentityRejectsDuplicateRemovalSelector(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	first := exposuredata.ExposureRoute{ID: "api-route-one", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Backend = "api-route-two", "http://127.0.0.1:3000"
	second.Target = targetmodel.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{first, second}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyRouteIdentity(context.Background(), listenerTarget, first.ID, true, time.Second); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("exact route removal proceeded despite duplicate mutation selectors: %v", err)
	}
	if len(provider.removes) != 0 {
		t.Fatalf("duplicate route selector reached provider removal: %#v", provider.removes)
	}
}

func TestApplyHTTPPathRejectsDuplicateExistingMutationSelector(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	first := exposuredata.ExposureRoute{ID: "api-route-one", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Backend = "api-route-two", "http://localhost:4321"
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{first, second}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPPath(context.Background(), listenerTarget, "/health", exposuredata.ExposureServe, false, time.Second); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("path addition proceeded despite duplicate exact provider selectors: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("path addition mutated provider state with ambiguous route identity: %#v", provider.sets)
	}
}

func TestApplyHTTPPathIsIdempotentForExactObservedHandler(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	existing := exposuredata.ExposureRoute{ID: "api-handler", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{existing}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	receipt, err := controller.ApplyHTTPPath(context.Background(), listenerTarget, "/api", exposuredata.ExposureServe, false, time.Second)
	if err != nil || !receipt.Verified || receipt.ID != existing.ID {
		t.Fatalf("exact existing path did not return a verified no-op: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.sets) != 0 || len(provider.removes) != 0 {
		t.Fatalf("idempotent apply mutated provider state: sets=%#v removes=%#v", provider.sets, provider.removes)
	}
}

func TestApplyHTTPPathAddsSiblingRouteWithExactBackend(t *testing.T) {
	target := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	sibling := exposuredata.ExposureRoute{ID: "docs-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Backend: "http://127.0.0.1:3000", Target: targetmodel.Target{Address: "127.0.0.1", Port: 3000, Protocol: "tcp"}.Normalized(), Mode: exposuredata.ExposureServe, URL: "https://dev.example.ts.net/docs", Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{sibling}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true, ServeHTTPS: true, FunnelHTTPS: true, ServePath: true, FunnelPath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	receipt, err := controller.ApplyHTTPPath(context.Background(), target, "/api-4321", exposuredata.ExposureServe, false, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("HTTP path apply failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.sets) != 1 || provider.sets[0].ProviderKey != "serve:https=443" || !provider.sets[0].HTTPPath || provider.sets[0].Path != "/api-4321" || provider.sets[0].Backend != "http://[::1]:4321" {
		t.Fatalf("HTTP path set did not preserve endpoint/path/backend: %#v", provider.sets)
	}
	if len(provider.removes) != 0 || len(provider.snapshot.Routes) != 2 || provider.snapshot.Routes[0].ID != sibling.ID {
		t.Fatalf("adding a path removed or replaced its sibling: routes=%#v removes=%#v", provider.snapshot.Routes, provider.removes)
	}
}

func TestApplyHTTPPathCanExplicitlyUseLocalhostForIPv6Backend(t *testing.T) {
	target := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	options := HTTPPathOptions{LocalhostBackendAlias: true}
	receipt, err := controller.ApplyHTTPPathWithOptions(context.Background(), target, "/node-4321", exposuredata.ExposureServe, false, options, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("localhost-backed named HTTP path failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.sets) != 1 || provider.sets[0].Backend != "http://localhost:4321" || !provider.sets[0].HTTPPath || provider.sets[0].HTTPSRoot {
		t.Fatalf("named path did not use the explicit localhost backend alias: %#v", provider.sets)
	}
	if len(provider.snapshot.Routes) != 1 || provider.snapshot.Routes[0].Path != "/node-4321" || provider.snapshot.Routes[0].Target.Normalized().Address != "127.0.0.1" {
		t.Fatalf("observed route did not retain alias backend identity: %#v", provider.snapshot.Routes)
	}
}

func TestApplyHTTPPathRejectsLocalhostAliasForIPv4Backend(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.ApplyHTTPPathWithOptions(context.Background(), target, "/node-4321", exposuredata.ExposureServe, false, HTTPPathOptions{LocalhostBackendAlias: true}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "only supported for IPv6 listeners") {
		t.Fatalf("IPv4 backend alias was not rejected: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("invalid backend alias reached provider mutation: %#v", provider.sets)
	}
}

func TestApplyHTTPSRootUsesExplicitPrivateCustomPortAndExactRemoval(t *testing.T) {
	target := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{
		snapshot:  exposuredata.ExposureSnapshot{Authoritative: true},
		caps:      tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true},
		readiness: ready(),
	}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	receipt, err := controller.ApplyHTTPSRoot(context.Background(), target, 4321, true, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("HTTPS root apply failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.sets) != 1 || provider.sets[0].ProviderKey != "serve:https=4321" || !provider.sets[0].HTTPSRoot || provider.sets[0].HTTPPath || provider.sets[0].Path != "/" || provider.sets[0].HTTPSPort != 4321 || provider.sets[0].Backend != "http://localhost:4321" {
		t.Fatalf("root handler did not preserve explicit port and backend alias: %#v", provider.sets)
	}
	root := provider.snapshot.Routes[0]
	if root.Kind != exposuredata.RouteKindHTTPSRoot || root.Path != "/" || root.URL != "https://dev.example.ts.net:4321/" {
		t.Fatalf("root handler identity was not verified: %#v", root)
	}
	_, err = controller.ApplyRouteIdentity(context.Background(), target, root.ID, true, time.Second)
	if err != nil {
		t.Fatalf("exact root route removal failed: %v", err)
	}
	if len(provider.removes) != 1 || provider.removes[0].ID != "serve:https=4321" || provider.removes[0].Path != "/" {
		t.Fatalf("root removal did not select exact HTTPS port/path: %#v", provider.removes)
	}
}

func TestApplyHTTPSRootReplacesExactServeTCPRouteOnlyAfterExplicitConfirmation(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	options := HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ExpectedRawTCPRouteID: raw.ID}
	if _, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, options, time.Second); err == nil {
		t.Fatal("unknown-owned raw TCP route was replaced without explicit external confirmation")
	}
	if len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("provider mutated before confirmation: removes=%#v sets=%#v", provider.removes, provider.sets)
	}

	options.ConfirmExternal = true
	receipt, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, options, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("confirmed HTTPS-root conversion failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.removes) != 1 || provider.removes[0].ID != raw.ProviderKey || provider.removes[0].Mode != exposuredata.ExposureServe || provider.removes[0].Backend != raw.Backend {
		t.Fatalf("conversion did not remove the exact prior TCP route: %#v", provider.removes)
	}
	if len(provider.sets) != 1 || provider.sets[0].Mode != exposuredata.ExposureServe || provider.sets[0].ProviderKey != "serve:https=4321" || !provider.sets[0].HTTPSRoot || provider.sets[0].Backend != "http://127.0.0.1:4321" {
		t.Fatalf("conversion did not configure a private same-port HTTPS root: %#v", provider.sets)
	}
	if len(provider.snapshot.Routes) != 1 || provider.snapshot.Routes[0].Kind != exposuredata.RouteKindHTTPSRoot || provider.snapshot.Routes[0].URL != "https://dev.example.ts.net:4321/" {
		t.Fatalf("converted root route was not the sole verified route: %#v", provider.snapshot.Routes)
	}
}

func TestApplyHTTPSRootDoesNotRemoveConcurrentRootBeforeApplying(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	concurrentRoot := exposuredata.ExposureRoute{ID: "concurrent-root", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, URL: "https://dev.example.ts.net:4321/", State: exposuredata.ExposureActive}
	base := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true}, readiness: ready(), afterRemoveRoute: &concurrentRoot}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, base)
	_, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true, ExpectedRawTCPRouteID: raw.ID}, time.Second)
	if err == nil || fault.AsAppError(err).State != "unverified" {
		t.Fatalf("concurrent endpoint change was not surfaced as unverified: %v", err)
	}
	if len(base.removes) != 1 || base.removes[0].ID != raw.ProviderKey || len(base.sets) != 0 {
		t.Fatalf("Tailge mutated a route that appeared before its HTTPS-root apply: removes=%#v sets=%#v", base.removes, base.sets)
	}
	if len(base.snapshot.Routes) != 1 || base.snapshot.Routes[0].ID != concurrentRoot.ID {
		t.Fatalf("concurrent handler was not left untouched: %#v", base.snapshot.Routes)
	}
}

type applyThenFailRootProvider struct {
	*fakeProvider
	mutateBackend string
}

func (p *applyThenFailRootProvider) Set(ctx context.Context, change tailscale.ExposureChange) (exposuredata.OperationReceipt, error) {
	receipt, err := p.fakeProvider.Set(ctx, change)
	if err != nil {
		return receipt, err
	}
	if change.HTTPSRoot {
		if p.mutateBackend != "" {
			p.mu.Lock()
			for i := range p.snapshot.Routes {
				if p.snapshot.Routes[i].Kind == exposuredata.RouteKindHTTPSRoot {
					p.snapshot.Routes[i].Backend = p.mutateBackend
				}
			}
			p.mu.Unlock()
		}
		return receipt, errors.New("simulated post-write failure")
	}
	return receipt, nil
}

func TestApplyHTTPSRootConversionRemovesPartialRootAndRestoresExactTCPRoute(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	base := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	provider := &applyThenFailRootProvider{fakeProvider: base}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true, ExpectedRawTCPRouteID: raw.ID}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "simulated post-write failure") {
		t.Fatalf("expected the requested root operation failure, got %v", err)
	}
	if len(base.removes) != 2 || base.removes[0].ID != raw.ProviderKey || base.removes[1].ID != "serve:https=4321" {
		t.Fatalf("partial HTTPS root was not removed by its exact selector: %#v", base.removes)
	}
	if len(base.sets) != 2 || !base.sets[0].HTTPSRoot || base.sets[1].HTTPSRoot || base.sets[1].ProviderKey != raw.ProviderKey || base.sets[1].Backend != raw.Backend {
		t.Fatalf("rollback did not restore the captured raw TCP route: %#v", base.sets)
	}
	if len(base.snapshot.Routes) != 1 || base.snapshot.Routes[0].ProviderKey != raw.ProviderKey || base.snapshot.Routes[0].Kind != exposuredata.RouteKindRawTCP || base.snapshot.Routes[0].Backend != raw.Backend {
		t.Fatalf("exact previous route was not restored after the partial root: %#v", base.snapshot.Routes)
	}
}

func TestApplyHTTPSRootConversionDoesNotOverwriteUnexpectedPartialRoot(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	base := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	provider := &applyThenFailRootProvider{fakeProvider: base, mutateBackend: "http://127.0.0.1:9999"}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true, ExpectedRawTCPRouteID: raw.ID}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("unexpected partial root state was not reported as an unverified rollback: %v", err)
	}
	if appErr := fault.AsAppError(err); appErr.Code != fault.ErrVerification || appErr.State != "unverified" || controller.operationStates[target.Key()] != exposuredata.ExposureUnverified {
		t.Fatalf("unproven rollback was not surfaced as Unverified: error=%#v operation=%q", appErr, controller.operationStates[target.Key()])
	}
	if len(base.removes) != 1 || base.removes[0].ID != raw.ProviderKey || len(base.sets) != 1 {
		t.Fatalf("rollback touched a route that no longer matched the confirmed request: removes=%#v sets=%#v", base.removes, base.sets)
	}
	if len(base.snapshot.Routes) != 1 || base.snapshot.Routes[0].Backend != "http://127.0.0.1:9999" {
		t.Fatalf("unexpected state was overwritten instead of left for inspection: %#v", base.snapshot.Routes)
	}
}

func TestApplyHTTPSRootRejectsRawRouteOnSamePortWithDifferentTarget(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	other := targetmodel.Target{Address: "127.0.0.2", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.2:4321", Target: other, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true}, time.Second)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "exact selected listener") {
		t.Fatalf("same-port route for a different listener was not rejected: %v", err)
	}
	if len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("different listener route was mutated: removes=%#v sets=%#v", provider.removes, provider.sets)
	}
}

func TestApplyHTTPSRootRejectsUnsafeRawTCPConversionCases(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	cases := []struct {
		name        string
		routes      []exposuredata.ExposureRoute
		options     HTTPSRootOptions
		wantMessage string
	}{
		{name: "no conversion intent", routes: []exposuredata.ExposureRoute{raw}, options: HTTPSRootOptions{HTTPSPort: target.Port, ConfirmExternal: true}, wantMessage: "explicit private HTTPS-root conversion"},
		{name: "unknown owner not confirmed", routes: []exposuredata.ExposureRoute{raw}, options: HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true}, wantMessage: "unknown/external ownership"},
		{name: "preview route changed", routes: []exposuredata.ExposureRoute{raw}, options: HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true, ExpectedRawTCPRouteID: "stale-id"}, wantMessage: "changed after preview"},
		{name: "raw backend targets a different address", routes: []exposuredata.ExposureRoute{{ID: raw.ID, ProviderKey: raw.ProviderKey, Kind: raw.Kind, Backend: "tcp://127.0.0.2:4321", Target: target, Mode: raw.Mode, Ownership: raw.Ownership, State: raw.State}}, options: HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true}, wantMessage: "exact selected listener"},
		{name: "sibling handler shares endpoint", routes: []exposuredata.ExposureRoute{raw, {ID: "sibling", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}}, options: HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true}, wantMessage: "shares its port"},
		{name: "public funnel route shares endpoint", routes: []exposuredata.ExposureRoute{{ID: "public", ProviderKey: "funnel:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureFunnel, Ownership: exposuredata.OwnershipManaged, State: exposuredata.ExposureActive}}, options: HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true}, wantMessage: "cannot be mixed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: test.routes}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true, Funnel: true, ExactFunnel: true}, readiness: ready()}
			discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
			controller := NewController(discoverer, provider)
			_, err := controller.ApplyHTTPSRootWithOptions(context.Background(), target, test.options, time.Second)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantMessage)) {
				t.Fatalf("unsafe conversion case was accepted or unclear: err=%v want=%q", err, test.wantMessage)
			}
			if len(provider.removes) != 0 || len(provider.sets) != 0 {
				t.Fatalf("provider mutated in unsafe conversion case: removes=%#v sets=%#v", provider.removes, provider.sets)
			}
		})
	}
}

func TestApplyHTTPSRootRejectsIncompleteSiblingHandlerIdentity(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	sibling := exposuredata.ExposureRoute{ID: "docs", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{sibling}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPSRoot(context.Background(), target, target.Port, false, time.Second); err == nil || !strings.Contains(err.Error(), "incomplete backend identity") {
		t.Fatalf("incomplete sibling backend was accepted for a new root: %v", err)
	}
	if len(provider.sets) != 0 || len(provider.removes) != 0 {
		t.Fatalf("endpoint was mutated despite incomplete sibling identity: sets=%#v removes=%#v", provider.sets, provider.removes)
	}
}

func TestApplyHTTPSRootRejectsStaleApprovedRouteBeforeConversion(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	raw := exposuredata.ExposureRoute{ID: "raw-id", ProviderKey: "serve:tcp=4321", Kind: exposuredata.RouteKindRawTCP, Backend: "tcp://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipUnknown, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{raw}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: target, RouteIDsHash: RouteIDsHash(provider.snapshot.Routes, target), TargetRoutesHash: RouteIdentityHash(provider.snapshot.Routes, target), AllRoutesHash: tailscale.RoutesHash(provider.snapshot.Routes), ListenerID: "node", ListenerPID: 10, ListenerProcess: "node", ListenerTarget: target}
	provider.snapshot.Routes[0].Backend = "tcp://127.0.0.1:9999"
	_, err := controller.ApplyHTTPSRootWithOptionsApproved(context.Background(), target, HTTPSRootOptions{HTTPSPort: target.Port, ReplaceRawTCP: true, ConfirmExternal: true, ExpectedRawTCPRouteID: raw.ID}, time.Second, approval)
	if err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("stale raw-TCP route approval was accepted: %v", err)
	}
	if len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("provider mutated after stale approval: removes=%#v sets=%#v", provider.removes, provider.sets)
	}
}

func TestApplyHTTPSRootPreservesExactIPv6BackendByDefault(t *testing.T) {
	target := targetmodel.Target{Address: "::1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{
		snapshot:  exposuredata.ExposureSnapshot{Authoritative: true},
		caps:      tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true},
		readiness: ready(),
	}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPSRoot(context.Background(), target, 4321, false, time.Second); err != nil {
		t.Fatalf("exact IPv6 root apply failed: %v", err)
	}
	if len(provider.sets) != 1 || provider.sets[0].Backend != "http://[::1]:4321" {
		t.Fatalf("IPv6 backend was silently changed: %#v", provider.sets)
	}
}

func TestRawApplyRejectsUnclassifiedHTTPPathRoute(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{
		snapshot:  exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{{ID: "unclassified", Path: "/api", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}}},
		caps:      tailscale.Capabilities{Serve: true, ExactServe: true, ServeTCP: true},
		readiness: ready(),
	}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureServe, false, false, time.Second); err == nil || !strings.Contains(err.Error(), "explicit HTTP-path intent") {
		t.Fatalf("raw Apply accepted an unclassified HTTP path route: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("raw Apply attempted to replace an unclassified HTTP path route: %#v", provider.sets)
	}
}

func TestGenericDisableRequiresExplicitHTTPRouteIdentity(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	for _, route := range []exposuredata.ExposureRoute{
		{ID: "named", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "root", ProviderKey: "serve:https=4321", Kind: exposuredata.RouteKindHTTPSRoot, Path: "/", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
		{ID: "unclassified", ProviderKey: "serve:https=443", Path: "/api", Backend: "http://127.0.0.1:4321", Target: target, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive},
	} {
		provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{route}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
		discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
		controller := NewController(discoverer, provider)
		if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureDisabled, false, false, time.Second); err == nil || !strings.Contains(err.Error(), "requires explicit route identity") {
			t.Fatalf("generic disable accepted HTTP route %q: %v", route.ID, err)
		}
		if len(provider.removes) != 0 {
			t.Fatalf("generic disable removed HTTP route %q: %#v", route.ID, provider.removes)
		}
	}
}

func TestApplyHTTPSRootRejectsInvalidPort(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "node")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyHTTPSRoot(context.Background(), target, 0, false, time.Second); err == nil || !strings.Contains(err.Error(), "valid HTTPS port") {
		t.Fatalf("invalid custom HTTPS port was not rejected: %v", err)
	}
	if len(provider.sets) != 0 {
		t.Fatalf("invalid root port triggered a provider mutation: %#v", provider.sets)
	}
}

func TestApplyHTTPPathRejectsCollisionVisibilityConflictAndUnsupportedProvider(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	otherTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4000, Protocol: "tcp"}.Normalized()
	cases := []struct {
		name       string
		mode       exposuredata.ExposureMode
		caps       tailscale.Capabilities
		route      exposuredata.ExposureRoute
		wantReason string
	}{
		{name: "duplicate path", mode: exposuredata.ExposureServe, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, route: exposuredata.ExposureRoute{ID: "existing", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4000", Target: otherTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}, wantReason: "already configured"},
		{name: "shared endpoint visibility", mode: exposuredata.ExposureServe, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, route: exposuredata.ExposureRoute{ID: "public", ProviderKey: "funnel:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/docs", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureFunnel, State: exposuredata.ExposureActive}, wantReason: "cannot be mixed"},
		{name: "raw TCP occupies same port", mode: exposuredata.ExposureServe, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, route: exposuredata.ExposureRoute{ID: "raw", ProviderKey: "serve:tcp=443", Kind: exposuredata.RouteKindRawTCP, Target: listenerTarget, Mode: exposuredata.ExposureServe, State: exposuredata.ExposureActive}, wantReason: "non-http handler"},
		{name: "unsupported path syntax", mode: exposuredata.ExposureServe, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true}, wantReason: "unsupported"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: test.caps, readiness: ready()}
			if test.route.ID != "" {
				provider.snapshot.Routes = append(provider.snapshot.Routes, test.route)
			}
			discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
			controller := NewController(discoverer, provider)
			_, err := controller.ApplyHTTPPath(context.Background(), listenerTarget, "/api", test.mode, false, time.Second)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.wantReason)) {
				t.Fatalf("expected %q refusal, got %v", test.wantReason, err)
			}
			if len(provider.sets) != 0 {
				t.Fatalf("provider mutated despite refusal: %#v", provider.sets)
			}
		})
	}
}

func TestApplyHTTPPathRejectsStaleApproval(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: listenerTarget, RouteIDsHash: RouteIDsHash(nil, listenerTarget), TargetRoutesHash: RouteIdentityHash(nil, listenerTarget), AllRoutesHash: tailscale.RoutesHash(nil), ListenerID: "stale-listener", ListenerTarget: listenerTarget}
	if _, err := controller.ApplyHTTPPathApproved(context.Background(), listenerTarget, "/api", exposuredata.ExposureServe, false, time.Second, approval); err == nil {
		t.Fatal("stale listener approval was accepted")
	}
	if len(provider.sets) != 0 {
		t.Fatal("provider mutated after stale approval")
	}
}

func TestDisableHTTPPathUsesExactRouteIdentityAndPreservesSibling(t *testing.T) {
	listenerTarget := targetmodel.Target{Address: "127.0.0.1", Port: 4321, Protocol: "tcp"}.Normalized()
	first := exposuredata.ExposureRoute{ID: "api-route", ProviderKey: "serve:https=443", Kind: exposuredata.RouteKindHTTPPath, Path: "/api", Backend: "http://127.0.0.1:4321", Target: listenerTarget, Mode: exposuredata.ExposureServe, URL: "https://dev.example.ts.net/api", Ownership: exposuredata.OwnershipExternal, State: exposuredata.ExposureActive}
	second := first
	second.ID, second.Path, second.URL = "docs-route", "/docs", "https://dev.example.ts.net/docs"
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{first, second}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true, ServeHTTPS: true, ServePath: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(listenerTarget, "api")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: listenerTarget, RouteIDsHash: RouteIDsHash(provider.snapshot.Routes, listenerTarget), TargetRoutesHash: RouteIdentityHash(provider.snapshot.Routes, listenerTarget), AllRoutesHash: tailscale.RoutesHash(provider.snapshot.Routes)}
	receipt, err := controller.ApplyRouteIdentityApproved(context.Background(), listenerTarget, first.ID, true, time.Second, approval)
	if err != nil || !receipt.Verified {
		t.Fatalf("exact named path removal failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.removes) != 1 || provider.removes[0].Path != "/api" || len(provider.snapshot.Routes) != 1 || provider.snapshot.Routes[0].ID != second.ID {
		t.Fatalf("removal was not scoped to the selected path: removes=%#v routes=%#v", provider.removes, provider.snapshot.Routes)
	}
}

func TestApplyReplacesExactExternalRouteOnlyAfterConfirmation(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := exposuredata.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Backend: "tcp://127.0.0.1:8080", Target: target, Mode: exposuredata.ExposureServe, Ownership: exposuredata.OwnershipExternal, State: exposuredata.ExposureActive}
	provider := &fakeProvider{snapshot: exposuredata.ExposureSnapshot{Authoritative: true, Routes: []exposuredata.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true, Listeners: []discovery.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, exposuredata.ExposureFunnel, true, false, time.Second); err == nil {
		t.Fatal("external route was replaced without external confirmation")
	}
	if len(provider.removes) != 0 {
		t.Fatal("external route was removed without confirmation")
	}
	receipt, err := controller.Apply(context.Background(), target, exposuredata.ExposureFunnel, true, true, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("replacement failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.removes) != 1 || len(provider.sets) != 1 {
		t.Fatalf("expected one exact remove and set, removes=%d sets=%d", len(provider.removes), len(provider.sets))
	}
}
