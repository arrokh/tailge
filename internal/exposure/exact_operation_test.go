package exposure

import (
	"testing"
	"time"

	"github.com/arrokh/tailge/internal/exposuredata"
	"github.com/arrokh/tailge/internal/fault"
	targetmodel "github.com/arrokh/tailge/internal/target"
)

func TestExactExposureOperationValidationTable(t *testing.T) {
	target := targetmodel.Target{Address: "127.0.0.1", Port: 8080, Protocol: "tcp"}
	deps := exactOperationDependencies{
		discoverer: &fakeDiscoverer{},
		provider:   &fakeProvider{},
	}
	tests := []struct {
		name string
		op   exactExposureOperation
		code fault.ErrorCode
	}{
		{name: "invalid target", op: exactExposureOperation{dependencies: deps, target: targetmodel.Target{Port: -1}, mode: exposuredata.ExposureServe}, code: fault.ErrInvalidInput},
		{name: "unsupported mode", op: exactExposureOperation{dependencies: deps, target: target, mode: exposuredata.ExposureMode("unknown")}, code: fault.ErrInvalidInput},
		{name: "funnel confirmation", op: exactExposureOperation{dependencies: deps, target: target, mode: exposuredata.ExposureFunnel}, code: fault.ErrUnsafe},
		{name: "missing provider", op: exactExposureOperation{dependencies: exactOperationDependencies{discoverer: &fakeDiscoverer{}}, target: target, mode: exposuredata.ExposureServe}, code: fault.ErrDependency},
		{name: "missing observer", op: exactExposureOperation{dependencies: exactOperationDependencies{provider: &fakeProvider{}}, target: target, mode: exposuredata.ExposureServe}, code: fault.ErrDependency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op.validate(); err == nil || fault.AsAppError(err).Code != tt.code {
				t.Fatalf("validate() = %v, want %s", err, tt.code)
			}
		})
	}

	valid := exactExposureOperation{dependencies: deps, target: target, mode: exposuredata.ExposureServe, timeout: time.Second}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid operation rejected: %v", err)
	}
	if valid.target.Key() != target.Normalized().Key() {
		t.Fatalf("target was not normalized: got %q want %q", valid.target.Key(), target.Normalized().Key())
	}
}
