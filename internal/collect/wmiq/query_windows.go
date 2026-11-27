//go:build windows

package wmiq

import (
	"context"
	"errors"

	"github.com/yusufpapurcu/wmi"
)

var shared = NewQueue()

func Query[T any](ctx context.Context, query, namespace string, dst *[]T) error {
	var rows []T
	err := shared.Do(ctx, func() error { return wmi.QueryNamespace(query, &rows, namespace) })
	var mismatch *wmi.ErrFieldMismatch
	if err != nil && !errors.As(err, &mismatch) {
		return err
	}
	*dst = rows
	return nil
}
