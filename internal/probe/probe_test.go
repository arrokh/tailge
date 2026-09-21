package probe

import (
	"context"
	"errors"
	"testing"

	"github.com/arrokh/tailge/internal/config"
	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/tailscale"
)

type probeDiscoverer struct {
	snapshot model.ListenerSnapshot
}

func (d probeDiscoverer) List(context.Context) (model.ListenerSnapshot, error) {
	return d.snapshot, nil
}

type probeProvider struct {
	sets int
}

func (p *probeProvider) Capabilities(context.Context) (tailscale.Capabilities, error) {
	return tailscale.Capabilities{}, nil
}
func (p *probeProvider) List(context.Context) (model.ExposureSnapshot, error) {
	return model.ExposureSnapshot{Authoritative: true}, nil
}
func (p *probeProvider) Set(context.Context, tailscale.ExposureChange) (model.OperationReceipt, error) {
	p.sets++
	return model.OperationReceipt{}, errors.New("unexpected mutation")
}
func (p *probeProvider) Remove(context.Context, tailscale.RouteSelector, string) (model.OperationReceipt, error) {
	return model.OperationReceipt{}, errors.New("unexpected removal")
}
func (p *probeProvider) Version(context.Context) (string, error) {
	return "test", nil
}

type probeConfigStore struct{}

func (probeConfigStore) Save(context.Context, config.Config) error { return nil }

func TestRunnerRejectsNonLoopbackBeforeMutation(t *testing.T) {
	provider := &probeProvider{}
	runner := Runner{
		Discoverer: probeDiscoverer{snapshot: model.ListenerSnapshot{Authoritative: true}},
		Provider:   provider,
		Config:     probeConfigStore{},
	}
	cfg := config.Defaults()
	err := runner.Run(context.Background(), &cfg, model.Target{Address: "192.168.1.20", Port: 3000, Protocol: "tcp"}, model.ExposureServe, false)
	if err == nil || model.AsAppError(err).Code != model.ErrUnsafe {
		t.Fatalf("non-loopback probe was accepted: %v", err)
	}
	if provider.sets != 0 {
		t.Fatal("provider mutated before target safety validation")
	}
}

func TestRouteIdentityCompleteRequiresExactSelector(t *testing.T) {
	base := model.ExposureRoute{Mode: model.ExposureServe, URL: "https://dev.ts.net:443"}
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
