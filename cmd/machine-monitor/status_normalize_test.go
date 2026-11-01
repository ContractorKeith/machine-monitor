package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

type statusFixture struct{}

func (statusFixture) Collect(context.Context) (*report.Report, error) {
	return &report.Report{ID: "fixture", Hostname: report.Ptr("a\x1b[31m\u202eb"), CPU: report.CPU{Usage: report.Ptr(999.0)}, GPUs: []report.GPU{{Usage: report.Ptr(999.0), Temp: report.Ptr(999.0)}}}, nil
}
func TestStatusNormalizesCollectorOutput(t *testing.T) {
	old := statusCollector
	statusCollector = func() collect.Collector { return statusFixture{} }
	t.Cleanup(func() { statusCollector = old })
	var out, errOut bytes.Buffer
	if code := runStatus([]string{"--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, &errOut)
	}
	var r report.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if *r.CPU.Usage != 100 || *r.GPUs[0].Usage != 100 || *r.GPUs[0].Temp != 250 || *r.Hostname != "ab" {
		t.Fatalf("unnormalized report: %s", &out)
	}
	if r.CPU.Temp != nil || r.Memory.Total != nil {
		t.Fatal("unknown metrics became numbers")
	}
}
