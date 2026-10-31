package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ContractorKeith/machine-monitor/internal/alerts"
	"github.com/ContractorKeith/machine-monitor/internal/config"
)

// TestAlertsAgainstHub drives every alerts call against a real hub and checks the settings
// change and the test alert arrive on the stream.
func TestAlertsAgainstHub(t *testing.T) {
	th := newTestHub(t, config.At(t.TempDir()), "")
	th.report("dev-a", nil)
	c := th.client(th.h.Token())
	ctx := context.Background()

	a, err := c.Alerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Settings.Rules) != 4 || !a.Settings.Channels.Desktop || a.Active == nil || a.PriceSet {
		t.Fatalf("defaults = %+v", a)
	}

	ev := stream(t, c, fastConfig())
	ev.next(EventSnapshot)

	s := a.Settings
	s.Rules = append(s.Rules, alerts.Rule{Type: alerts.TypeCPU, Device: "dev-a", Threshold: ptrTo(1.0), Enabled: true})
	got, active, err := c.SetAlerts(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 5 || got.Rules[4].ID == "" || got.Rules[4].Device != "dev-a" || active == nil {
		t.Fatalf("saved = %+v", got.Rules)
	}
	var msg alerts.Message
	if e := ev.next(EventAlertSettings); json.Unmarshal(e.Raw, &msg) != nil || msg.Settings == nil || len(msg.Settings.Rules) != 5 {
		t.Fatalf("alerts event = %s", e.Raw)
	}

	// Invalid settings: a 400 with the hub's message.
	bad := got
	bad.Rules = append([]alerts.Rule(nil), got.Rules...)
	bad.Rules[4].Threshold = ptrTo(500.0)
	var se *StatusError
	if _, _, err := c.SetAlerts(ctx, bad); !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("invalid settings: %v", err)
	}

	// The next report fires the CPU rule; the log has it.
	th.report("dev-a", nil)
	if e := ev.next(EventAlert); json.Unmarshal(e.Raw, &msg) != nil || msg.Event == nil || msg.Event.State != alerts.Firing {
		t.Fatalf("alert event = %s", e.Raw)
	}
	if a, err = c.Alerts(ctx); err != nil || len(a.Active) != 1 || len(a.Events) != 1 {
		t.Fatalf("after firing: %+v, %v", a, err)
	}

	d, err := c.TestAlerts(ctx)
	if err != nil || d.Ntfy != nil || d.Webhook != nil {
		t.Fatalf("test = %+v, %v", d, err)
	}
	if e := ev.next(EventAlert); json.Unmarshal(e.Raw, &msg) != nil || msg.Event == nil || !msg.Event.Test || !msg.Notify {
		t.Fatalf("test alert event = %s", e.Raw)
	}

	if err := c.ClearAlertLog(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err = c.Alerts(ctx); err != nil || len(a.Events) != 0 || len(a.Active) != 1 {
		t.Fatalf("after clear: %+v, %v", a, err)
	}
}

func ptrTo[T any](v T) *T { return &v }
