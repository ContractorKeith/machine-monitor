package client

import (
	"context"
	"errors"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/config"
)

func TestProcessesAndAction(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	withProcs := func(m map[string]any) {
		m["features"] = []string{"procs"}
		m["actions"] = []string{"restart"}
		m["processes"] = []map[string]any{{"pid": 42, "name": "ollama"}}
	}
	th.report("dev-a", withProcs)
	c := th.client(th.h.Token())
	ctx := context.Background()

	p, err := c.Processes(ctx, "dev-a")
	if err != nil || !p.Supported || p.Procs != nil {
		t.Fatalf("first Processes = %+v, %v; want supported, no list yet", p, err)
	}
	th.report("dev-a", withProcs) // now watched: the hub keeps it
	if p, err = c.Processes(ctx, "dev-a"); err != nil || p.Procs == nil || len(p.Procs.List) != 1 {
		t.Fatalf("watched Processes = %+v, %v", p, err)
	}

	if err := c.Action(ctx, "dev-a", "restart"); err != nil {
		t.Errorf("restart: %v", err)
	}
	var se *StatusError
	if err := c.Action(ctx, "dev-a", "shutdown"); !errors.As(err, &se) || se.Code != 409 {
		t.Errorf("shutdown (not allowed) = %v, want 409", err)
	}
	if err := c.Action(ctx, "dev-a", "wake"); !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("wake without a MAC = %v, want 400", err)
	}
	if _, err := c.Processes(ctx, "nope"); !errors.As(err, &se) || se.Code != 404 {
		t.Errorf("unknown device = %v, want 404", err)
	}
}
