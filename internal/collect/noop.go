package collect

import (
	"context"

	"github.com/ContractorKeith/machine-monitor/internal/report"
)

// noop is the placeholder collector until the per-OS collectors land (M1). It knows the
// platform and nothing else; every metric is nil.
type noop struct{ platform string }

func (n noop) Collect(ctx context.Context) (*report.Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &report.Report{Platform: n.platform}, nil
}
