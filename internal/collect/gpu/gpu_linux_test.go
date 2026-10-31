//go:build linux

package gpu

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/collect"
	"github.com/ContractorKeith/machine-monitor/internal/report"
)

func TestLinuxProbeRegistered(t *testing.T) {
	for _, p := range collect.Probes() {
		if p.Name == "gpu/linux" {
			return
		}
	}
	t.Error("gpu/linux not registered")
}

func TestRunLinux(t *testing.T) {
	fakeTools(t)
	apply, err := runLinux(context.Background(), findTool("nvidia-smi"), filepath.Join("testdata", "sys"))
	if err != nil {
		t.Fatal(err)
	}
	r := &report.Report{}
	apply(r)
	var names []string
	for _, g := range r.GPUs {
		names = append(names, *g.Name)
	}
	// Biggest VRAM first: 4090 (24 GiB), AMD 16 GiB, 3060 (12 GiB), AMD 4 GiB.
	want := []string{"NVIDIA GeForce RTX 4090", "AMD GPU", "NVIDIA GeForce RTX 3060", "AMD GPU"}
	if len(names) != len(want) {
		t.Fatalf("GPUs = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("GPUs = %v, want %v", names, want)
		}
	}
}

func TestRunLinuxNothingFound(t *testing.T) {
	apply, err := runLinux(context.Background(), "", t.TempDir())
	if apply != nil || err != nil {
		t.Errorf("no GPUs = %v, %v; want nil, nil", apply != nil, err)
	}
	// A broken nvidia-smi with no AMD cards reports its error.
	if _, err := runLinux(context.Background(), filepath.Join(t.TempDir(), "nvidia-smi"), t.TempDir()); err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("broken nvidia-smi err = %v", err)
	}
}
