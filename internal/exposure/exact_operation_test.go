package exposure

import (
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/model"
)

func TestExactExposureOperationValidationTable(t *testing.T) {
	target := model.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}
	deps := exactOperationDependencies{
		discoverer: &fakeDiscoverer{},
		provider:   &fakeProvider{},
	}
	tests := []struct {
		name string
		op   exactExposureOperation
		code model.ErrorCode
	}{
		{name: "invalid target", op: exactExposureOperation{dependencies: deps, target: model.Target{Port: -1}, mode: model.ExposureServe}, code: model.ErrInvalidInput},
		{name: "unsupported mode", op: exactExposureOperation{dependencies: deps, target: target, mode: model.ExposureMode("unknown")}, code: model.ErrInvalidInput},
		{name: "funnel confirmation", op: exactExposureOperation{dependencies: deps, target: target, mode: model.ExposureFunnel}, code: model.ErrUnsafe},
		{name: "missing provider", op: exactExposureOperation{dependencies: exactOperationDependencies{discoverer: &fakeDiscoverer{}}, target: target, mode: model.ExposureServe}, code: model.ErrDependency},
		{name: "missing observer", op: exactExposureOperation{dependencies: exactOperationDependencies{provider: &fakeProvider{}}, target: target, mode: model.ExposureServe}, code: model.ErrDependency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op.validate(); err == nil || model.AsAppError(err).Code != tt.code {
				t.Fatalf("validate() = %v, want %s", err, tt.code)
			}
		})
	}

	valid := exactExposureOperation{dependencies: deps, target: target, mode: model.ExposureServe, timeout: time.Second}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid operation rejected: %v", err)
	}
	if valid.target.Key() != target.Normalized().Key() {
		t.Fatalf("target was not normalized: got %q want %q", valid.target.Key(), target.Normalized().Key())
	}
}
