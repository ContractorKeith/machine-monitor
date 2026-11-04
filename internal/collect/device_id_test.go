package collect

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"
)

func TestSourceDeviceIDWithoutMetrics(t *testing.T) {
	ctx := context.Background()
	mac := macFixture(t, nil, macFiles)
	recordedRun := mac.run
	mac.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "/usr/sbin/ioreg" {
			t.Fatalf("identity ran unrelated command: %s", name)
		}
		return recordedRun(ctx, name, args...)
	}
	win := windowsFixture(t, nil)
	win.ps = nil
	for name, tc := range map[string]struct {
		src  source
		want string
	}{
		"linux":          {linuxFixture(nil), "4c4c4544004e3510804ab4c04f4d4e32"},
		"linux dbus":     {linuxSource{fsys: fstest.MapFS{"var/lib/dbus/machine-id": {Data: []byte("dbus-id\n")}}, hostname: func() (string, error) { return "host", nil }}, "dbus-id"},
		"linux hostname": {linuxSource{fsys: fstest.MapFS{}, hostname: func() (string, error) { return "host", nil }}, "host"},
		"macOS":          {mac, "1B2C3D4E-5F60-7182-93A4-B5C6D7E8F901"},
		"macOS hostname": {macFixture(t, nil, nil), "kmbp"},
		"windows":        {win, "6f1c2b3a-1111-2222-3333-444455556666"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.src.deviceID(ctx); got != tc.want {
				t.Fatalf("ID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeviceIDCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DeviceID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
