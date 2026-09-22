package probe

import (
	"context"
	"errors"
	"testing"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/discovery"
	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	"github.com/arrokh/tailge/internal/tailscale"
	"github.com/arrokh/tailge/internal/target"
)

type probeDiscoverer struct {
	snapshot discovery.ListenerSnapshot
}

func (d probeDiscoverer) List(context.Context) (discovery.ListenerSnapshot, error) {
	return d.snapshot, nil
}

type probeProvider struct {
	sets int
}

func (p *probeProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return tailscale.Capabilities{}, nil
}
func (p *probeProvider) List(context.Context) (exposuredata.ExposureSnapshot, error) {
	return exposuredata.ExposureSnapshot{Authoritative: true}, nil
}
func (p *probeProvider) Set(context.Context, tailscale.ExposureChange) (exposuredata.OperationReceipt, error) {
	p.sets++
	return exposuredata.OperationReceipt{}, errors.New("unexpected mutation")
}
func (p *probeProvider) Remove(context.Context, tailscale.RouteSelector, string) (exposuredata.OperationReceipt, error) {
	return exposuredata.OperationReceipt{}, errors.New("unexpected removal")
}
func (p *probeProvider) Version(context.Context) (string, error) {
	return "test", nil
}

type probeConfigStore struct{}

func (probeConfigStore) Save(context.Context, config.Config) error { return nil }

func TestRunnerRejectsNonLoopbackBeforeMutation(t *testing.T) {
	provider := &probeProvider{}
	runner := Runner{
		Discoverer: probeDiscoverer{snapshot: discovery.ListenerSnapshot{Authoritative: true}},
		Provider:   provider,
		Config:     probeConfigStore{},
	}
	cfg := config.Defaults()
	err := runner.Run(context.Background(), &cfg, target.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, exposuredata.ExposureServe, false)
	if err == nil || fault.AsAppError(err).Code != fault.ErrUnsafe {
		t.Fatalf("non-loopback probe was accepted: %v", err)
	}
	if provider.sets != 0 {
		t.Fatal("provider mutated before target safety validation")
	}
}

func TestRouteIdentityCompleteRequiresExactSelector(t *testing.T) {
	base := exposuredata.ExposureRoute{Mode: exposuredata.ExposureServe, URL: "https://dev.ts.net:443"}
	if RouteIdentityComplete(base) {
		t.Fatal("route without provider selector was treated as exact")
	}
	base.ProviderKey = "serve:https=443"
	if !RouteIdentityComplete(base) {
		t.Fatal("complete HTTPS route was rejected")
	}
	base.ProviderKey = "serve:svc:api"
	if RouteIdentityComplete(base) {
		t.Fatal("service route was treated as deterministic listener")
	}
}
