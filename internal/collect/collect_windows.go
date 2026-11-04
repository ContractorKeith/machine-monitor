//go:build windows

package collect

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/yusufpapurcu/wmi"
	"golang.org/x/sys/windows/registry"
)

func newOS() Collector { return newCore(newSource(), gopsutil{}) }

func newSource() source {
	return &windowsSource{
		ps:          gopsutil{},
		wmi:         wmiClient{},
		machineGUID: machineGUID,
		hostname:    func() string { h, _ := os.Hostname(); return cmp.Or(os.Getenv("COMPUTERNAME"), h) },
	}
}

// wmiTimeout bounds one WMI query; WMI calls take no context.
const wmiTimeout = 3 * time.Second

// wmiClient is the real wmiQuerier.
type wmiClient struct{}

// Query runs q on its own goroutine so ctx and wmiTimeout can bound it. A query that
// outlives them finishes in the background into a result nobody reads.
func (wmiClient) Query(ctx context.Context, q string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, wmiTimeout)
	defer cancel()
	done := make(chan error, 1)
	tmp := reflect.New(reflect.TypeOf(dst).Elem()) // so a late query never writes dst
	go func() {
		// wmi panics when a property's type doesn't match the field; fail soft instead.
		defer func() {
			if v := recover(); v != nil {
				done <- fmt.Errorf("wmi: %s: panic: %v", q, v)
			}
		}()
		done <- wmi.Query(q, tmp.Interface())
	}()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		reflect.ValueOf(dst).Elem().Set(tmp.Elem())
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wmi: %w", ctx.Err())
	}
}

func machineGUID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue("MachineGuid")
	return v, err
}
