package exposure

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

type fakeDiscoverer struct {
	snapshot model.ListenerSnapshot
	err      error
}

func (f *fakeDiscoverer) List(context.Context) (model.ListenerSnapshot, error) {
	return f.snapshot, f.err
}

type disappearingDiscoverer struct {
	target model.Target
	calls  int
}

func (d *disappearingDiscoverer) List(context.Context) (model.ListenerSnapshot, error) {
	d.calls++
	if d.calls == 1 {
		return model.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []model.Listener{testListener(d.target, "api")}}, nil
	}
	return model.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []model.Listener{}}, nil
}

type sequencedDiscoverer struct {
	mu           sync.Mutex
	calls        int
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func (d *sequencedDiscoverer) List(context.Context) (model.ListenerSnapshot, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()
	if call == 1 {
		close(d.firstStarted)
		<-d.releaseFirst
	}
	target := model.Target{Address: "127.0.0.1", Port: 10000 + call, Protocol: "tcp"}.Normalized()
	return model.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}, nil
}

type fakeProvider struct {
	mu             sync.Mutex
	snapshot       model.ExposureSnapshot
	caps           tailscale.Capabilities
	readiness      model.Readiness
	readinessErr   error
	sets           []tailscale.ExposureChange
	removes        []tailscale.RouteSelector
	setErr         error
	setErrMode     model.ExposureMode
	setProviderKey string
	removeErr      error
	setStarted     chan struct{}
	releaseSet     chan struct{}
	setOnce        sync.Once
	blockList      <-chan struct{}
}

func (f *fakeProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return f.caps, nil
}
func (f *fakeProvider) Readiness(context.Context, tailscale.ReadinessOptions) (model.Readiness, error) {
	return f.readiness, f.readinessErr
}
func (f *fakeProvider) List(context.Context) (model.ExposureSnapshot, error) {
	if f.blockList != nil {
		<-f.blockList
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	copySnapshot := f.snapshot
	copySnapshot.Routes = append([]model.ExposureRoute(nil), f.snapshot.Routes...)
	return copySnapshot, nil
}
func (f *fakeProvider) Set(_ context.Context, change tailscale.ExposureChange) (model.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, change)
	if f.setStarted != nil {
		f.setOnce.Do(func() { close(f.setStarted) })
		<-f.releaseSet
	}
	if f.setErr != nil && (f.setErrMode == "" || f.setErrMode == change.Mode) {
		return model.OperationReceipt{}, f.setErr
	}
	providerKey := tailscale.ProviderKeyForTargetWithCapabilities(change.Mode, change.Target, f.caps)
	id := providerKey
	if f.setProviderKey != "" {
		providerKey = f.setProviderKey
	}
	f.snapshot.Routes = append(f.snapshot.Routes, model.ExposureRoute{ID: id, ProviderKey: providerKey, Service: change.Service, Path: change.Path, Backend: change.Backend, Target: change.Target, Mode: change.Mode, Ownership: model.OwnershipUnknown, State: model.ExposureActive, LastSeen: time.Now()})
	return model.OperationReceipt{ID: id, Verified: false}, nil
}
func (f *fakeProvider) Remove(_ context.Context, selector tailscale.RouteSelector, _ string) (model.OperationReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, selector)
	if f.removeErr != nil {
		return model.OperationReceipt{}, f.removeErr
	}
	for i, route := range f.snapshot.Routes {
		if route.ID == selector.ID || route.ProviderKey == selector.ID {
			f.snapshot.Routes = append(f.snapshot.Routes[:i], f.snapshot.Routes[i+1:]...)
			break
		}
	}
	return model.OperationReceipt{ID: "remove-" + selector.ID}, nil
}

func testListener(target model.Target, name string) model.Listener {
	return model.Listener{ID: name, Target: target, Name: name, Process: name, PID: 10, Scope: model.ScopeForAddress(target.Address), Metadata: model.MetadataComplete}
}

func ready() model.Readiness {
	return model.Readiness{Status: model.ReadinessReady, Modes: []model.ModeReadiness{
		{Mode: model.ExposureServe, Status: model.ReadinessReady},
		{Mode: model.ExposureFunnel, Status: model.ReadinessReady},
	}}
}

func TestMatchesDoesNotTreatWildcardRouteAsEverySpecificListener(t *testing.T) {
	wildcard := model.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}
	loopback := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}
	otherLocal := model.Target{Address: "192.168.1.2", Port: 8080, Protocol: "tcp"}
	if Matches(wildcard, loopback) || Matches(wildcard, otherLocal) {
		t.Fatal("wildcard route matched a specific listener")
	}
	if !Matches(loopback, wildcard) {
		t.Fatal("loopback route did not match wildcard listener")
	}
}

func TestMatchesTreatsIPv4AndIPv6LoopbackAsLogicalLoopback(t *testing.T) {
	ipv4 := model.Target{Address: "127.0.0.1", Port: 4323, Protocol: "tcp"}
	ipv6 := model.Target{Address: "::1", Port: 4323, Protocol: "tcp"}
	if !Matches(ipv4, ipv6) || !Matches(ipv6, ipv4) {
		t.Fatal("loopback families were not correlated")
	}
}

func TestReconcileTreatsErrorBearingSnapshotsAsUnknown(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	snapshot := model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}, Error: &model.SafeError{Code: model.ErrUnknown, Message: "partial"}}
	view := Reconcile(snapshot, model.ExposureSnapshot{Authoritative: true}, nil)
	if view.Listeners.Authoritative || len(view.Items) != 1 || view.Items[0].State != model.ExposureUnknown {
		t.Fatalf("error-bearing listener snapshot was treated as safe: %#v", view)
	}
}

func TestReconcileWarnsForNetworkAndSensitiveListeners(t *testing.T) {
	target := model.Target{Address: "0.0.0.0", Port: 5432, Protocol: "tcp"}.Normalized()
	listener := testListener(target, "postgres")
	view := Reconcile(model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{listener}}, model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{}}, nil)
	if len(view.Items) != 1 || !strings.Contains(view.Items[0].Warning, "local network") || !strings.Contains(view.Items[0].Warning, "sensitive") {
		t.Fatalf("missing listener warnings: %#v", view.Items)
	}
}

func TestReconcileShowsInactiveConfiguredRouteRecommendation(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	route := model.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipUnknown, State: model.ExposureActive}
	view := Reconcile(model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{}}, model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{route}}, nil)
	if len(view.Items) != 1 || view.Items[0].State != model.ExposureInactive {
		t.Fatalf("unexpected view: %#v", view.Items)
	}
	if view.Items[0].Recommendation == "" || view.Items[0].Routes[0].State != model.ExposureInactive {
		t.Fatalf("missing inactive recommendation: %#v", view.Items[0])
	}
}

func TestReconcileMarksAmbiguousListenerAndRoute(t *testing.T) {
	target := model.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}.Normalized()
	listeners := model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{
		testListener(model.Target{Address: "0.0.0.0", Port: 8080, Protocol: "tcp"}.Normalized(), "one"),
		testListener(model.Target{Address: "::", Port: 8080, Protocol: "tcp"}.Normalized(), "two"),
	}}
	routes := model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: model.ExposureServe, State: model.ExposureActive}}}
	view := Reconcile(listeners, routes, nil)
	if len(view.Items) != 3 {
		t.Fatalf("expected two listener rows and one route row, got %d", len(view.Items))
	}
	ambiguous := 0
	for _, item := range view.Items {
		if item.State == model.ExposureAmbiguous {
			ambiguous++
		}
	}
	if ambiguous < 2 {
		t.Fatalf("ambiguous route was not propagated to listeners: %#v", view.Items)
	}
}

func TestRefreshRejectsLateOlderGeneration(t *testing.T) {
	discoverer := &sequencedDiscoverer{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{At: time.Now(), Authoritative: true}}
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
		snapshot:  model.ExposureSnapshot{At: time.Now(), Authoritative: true, Routes: []model.ExposureRoute{}},
		blockList: release,
	}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{At: time.Now(), Authoritative: true, Listeners: []model.Listener{testListener(model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}, "web")}}}
	controller := NewController(discoverer, provider)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	view, err := controller.Refresh(ctx)
	if err == nil || model.AsAppError(err).Code != model.ErrTimeout {
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
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
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
	if len(view.Items) != 1 || view.Items[0].State != model.ExposureUnknown {
		t.Fatalf("degraded route/listener was presented as known: %#v", view.Items)
	}
}

func TestApplySerializesMutationsForOneTarget(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready(), setStarted: make(chan struct{}), releaseSet: make(chan struct{})}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	firstDone := make(chan error, 1)
	go func() {
		_, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second)
		firstDone <- err
	}()
	<-provider.setStarted
	if _, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("concurrent mutation was not rejected")
	}
	close(provider.releaseSet)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	view, err := controller.Refresh(context.Background())
	if err != nil || len(view.Items) != 1 || view.Items[0].DesiredMode != model.ExposureServe || view.Items[0].OperationState != model.ExposureSucceeded || view.Items[0].LastOperation == nil || !view.Items[0].LastOperation.Verified || len(view.Events) < 3 {
		t.Fatalf("operation lifecycle was not published: view=%#v err=%v", view, err)
	}
}

func TestApplyRevalidatesListenerBeforeMutation(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &disappearingDiscoverer{target: target}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second)
	if err == nil || model.AsAppError(err).Code != model.ErrInvalidInput || len(provider.sets) != 0 {
		t.Fatalf("mutation was not blocked after listener disappeared: err=%v sets=%d calls=%d", err, len(provider.sets), discoverer.calls)
	}
}

func TestApplyDoesNotAcceptTargetOnlyPostWriteMatch(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready(), setProviderKey: "serve:tcp=9999"}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, 3*time.Second)
	if err == nil || model.AsAppError(err).Code != model.ErrVerification {
		t.Fatalf("target-only route was accepted: err=%v", err)
	}
}

func TestApplyReportsFailureAfterRestoringPreviousMode(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := model.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Backend: "tcp://127.0.0.1:8080", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipExternal, State: model.ExposureActive}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready(), setErr: errors.New("funnel provider failed"), setErrMode: model.ExposureFunnel}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, model.ExposureFunnel, true, true, time.Second); err == nil {
		t.Fatal("expected replacement failure")
	}
	if len(provider.sets) != 2 || provider.sets[0].Mode != model.ExposureFunnel || provider.sets[1].Mode != model.ExposureServe {
		t.Fatalf("expected failed replacement and Serve rollback, sets=%#v", provider.sets)
	}
	provider.mu.Lock()
	routes := append([]model.ExposureRoute(nil), provider.snapshot.Routes...)
	provider.mu.Unlock()
	if len(routes) != 1 || routes[0].Mode != model.ExposureServe {
		t.Fatalf("previous route was not restored: %#v", routes)
	}
}

func TestApplyRejectsReplacementWithoutExactRollbackSelector(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := model.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipExternal, State: model.ExposureActive}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	_, err := controller.Apply(context.Background(), target, model.ExposureFunnel, true, true, time.Second)
	if err == nil || model.AsAppError(err).Code != model.ErrUnsupported {
		t.Fatalf("unsafe rollback selector was accepted: %v", err)
	}
	if len(provider.removes) != 0 || len(provider.sets) != 0 {
		t.Fatalf("replacement mutated before proving rollback safety: removes=%d sets=%d", len(provider.removes), len(provider.sets))
	}
}

func TestDisableRequiresVerifiedRouteRemovalReadiness(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	route := model.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipManaged, State: model.ExposureActive}
	readiness := ready()
	readiness.Modes[0].Status = model.ReadinessReadOnly
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{route}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: readiness}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, model.ExposureDisabled, false, true, time.Second); err == nil {
		t.Fatal("disable was accepted without verified Serve readiness")
	}
	if len(provider.removes) != 0 {
		t.Fatal("route was removed while readiness was read-only")
	}
}

func TestApplyRouteDisablesOneOfMultipleRoutes(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	serve := model.ExposureRoute{ID: "serve:api", ProviderKey: "serve:api", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipManaged, State: model.ExposureActive}
	funnel := model.ExposureRoute{ID: "funnel:api", ProviderKey: "funnel:api", Target: target, Mode: model.ExposureFunnel, Ownership: model.OwnershipManaged, State: model.ExposureActive}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{serve, funnel}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
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

func TestApplyRouteModeRejectsAmbiguousModeSelection(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	first := model.ExposureRoute{ID: "serve:https", ProviderKey: "serve:https=443", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipManaged, State: model.ExposureActive}
	second := model.ExposureRoute{ID: "serve:tcp", ProviderKey: "serve:tcp=443", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipManaged, State: model.ExposureActive}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{first, second}}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.ApplyRouteMode(context.Background(), target, model.ExposureServe, true, time.Second); err == nil || model.AsAppError(err).Code != model.ErrAmbiguous {
		t.Fatalf("ambiguous mode selection was accepted: %v", err)
	}
	if len(provider.removes) != 0 {
		t.Fatal("ambiguous route selection mutated provider state")
	}
}

func TestApprovedMutationRejectsChangedListenerAndRoutes(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8089, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	approval := MutationApproval{Target: target, ListenerID: "api", ListenerPID: 10, ListenerProcess: "api", ListenerTarget: target, RouteIDsHash: RouteIDsHash(nil, target), AllRoutesHash: tailscale.RoutesHash(nil)}
	discoverer.snapshot.Listeners[0].Process = "replacement"
	if _, err := controller.ApplyApproved(context.Background(), target, model.ExposureServe, false, false, time.Second, approval); err == nil {
		t.Fatal("changed listener was accepted after confirmation")
	}
	if len(provider.sets) != 0 {
		t.Fatal("provider mutated after approval fingerprint changed")
	}
}

func TestManagedOwnershipIsRevokedWhenRouteIdentityChanges(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8090, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true}, caps: tailscale.Capabilities{Serve: true, ExactServe: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second); err != nil {
		t.Fatalf("initial managed route failed: %v", err)
	}
	provider.mu.Lock()
	provider.snapshot.Routes[0].Backend = "tcp://127.0.0.1:8090"
	provider.mu.Unlock()
	view, err := controller.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if len(view.Items) != 1 || view.Items[0].Routes[0].Ownership == model.OwnershipManaged {
		t.Fatalf("changed route retained managed ownership: %#v", view.Items)
	}
	if _, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("changed external route was mutated without confirmation")
	}
}

func TestApplyRequiresFunnelConfirmationAndAuthoritativeSnapshots(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, model.ExposureFunnel, false, false, time.Second); err == nil {
		t.Fatal("funnel was accepted without confirmation")
	}
	if len(provider.sets) != 0 {
		t.Fatal("provider was mutated after missing confirmation")
	}
	provider.snapshot.Authoritative = false
	if _, err := controller.Apply(context.Background(), target, model.ExposureServe, false, false, time.Second); err == nil {
		t.Fatal("non-authoritative exposure state was accepted")
	}
}

func TestApplyReplacesExactExternalRouteOnlyAfterConfirmation(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}.Normalized()
	old := model.ExposureRoute{ID: "serve:tcp=8080", ProviderKey: "serve:tcp=8080", Backend: "tcp://127.0.0.1:8080", Target: target, Mode: model.ExposureServe, Ownership: model.OwnershipExternal, State: model.ExposureActive}
	provider := &fakeProvider{snapshot: model.ExposureSnapshot{Authoritative: true, Routes: []model.ExposureRoute{old}}, caps: tailscale.Capabilities{Serve: true, Funnel: true, ExactServe: true, ExactFunnel: true}, readiness: ready()}
	discoverer := &fakeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true, Listeners: []model.Listener{testListener(target, "api")}}}
	controller := NewController(discoverer, provider)
	if _, err := controller.Apply(context.Background(), target, model.ExposureFunnel, true, false, time.Second); err == nil {
		t.Fatal("external route was replaced without external confirmation")
	}
	if len(provider.removes) != 0 {
		t.Fatal("external route was removed without confirmation")
	}
	receipt, err := controller.Apply(context.Background(), target, model.ExposureFunnel, true, true, time.Second)
	if err != nil || !receipt.Verified {
		t.Fatalf("replacement failed: receipt=%#v err=%v", receipt, err)
	}
	if len(provider.removes) != 1 || len(provider.sets) != 1 {
		t.Fatalf("expected one exact remove and set, removes=%d sets=%d", len(provider.removes), len(provider.sets))
	}
}
