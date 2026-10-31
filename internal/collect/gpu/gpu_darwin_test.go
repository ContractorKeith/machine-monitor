//go:build darwin

package gpu

import (
	"context"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func TestRunMacOS(t *testing.T) {
	fakeTools(t)
	apply, err := runMacOS(context.Background(), findTool("ioreg"))
	if err != nil {
		t.Fatal(err)
	}
	r := &report.Report{CPU: report.CPU{Name: report.Ptr("Apple M3 Max")}}
	apply(r)
	if len(r.GPUs) != 2 || *r.GPUs[0].Name != "Apple M3 Max" || *r.GPUs[1].Name != "Apple M3 Max GPU" || *r.GPUs[0].Usage != 33 {
		t.Errorf("GPUs = %s", asJSON(t, r.GPUs))
	}
	if apply, err := runMacOS(context.Background(), ""); apply != nil || err != nil {
		t.Errorf("missing ioreg = %v, %v; want nil, nil", apply != nil, err)
	}
}
