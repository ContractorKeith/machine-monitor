//go:build windows

package wmiq

import (
	"context"
	"errors"

	"github.com/yusufpapurcu/wmi"
)

// shared is the one queue every WMI query in the probes goes through.
var shared = NewQueue()

// Query runs a WMI query in namespace (e.g. `root\cimv2`) on the shared worker and stores
// the rows in dst. dst is untouched unless Query returns nil. A struct field the class
// lacks is not an error; that field stays zero.
func Query[T any](ctx context.Context, query, namespace string, dst *[]T) error {
	var rows []T // written by the worker; read here only after it reports done
	err := shared.Do(ctx, func() error { return wmi.QueryNamespace(query, &rows, namespace) })
	var mismatch *wmi.ErrFieldMismatch
	if err != nil && !errors.As(err, &mismatch) {
		return err
	}
	*dst = rows
	return nil
}
