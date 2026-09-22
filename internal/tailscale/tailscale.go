package tailscale

import (
	"context"
	"time"

	"github.com/arrokh/tailge/internal/model"
	"github.com/arrokh/tailge/internal/runner"
)

type Exposer interface {
	Capabilities(context.Context) (Capabilities, error)
	List(context.Context) (model.ExposureSnapshot, error)
	Set(context.Context, ExposureChange) (model.OperationReceipt, error)
	Remove(context.Context, RouteSelector, string) (model.OperationReceipt, error)
}

type Adapter struct {
	Runner runner.Runner
	Binary string
	Now    func() time.Time
}

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func New(r runner.Runner) *Adapter {
	return &Adapter{Runner: r, Binary: findBinary(), Now: time.Now}
}

func ptr[T any](v T) *T { return &v }
