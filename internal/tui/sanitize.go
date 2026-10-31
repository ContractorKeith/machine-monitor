package tui

// Everything the TUI draws from another machine (a report, a process list, an alert, a hub
// error) goes through clean first, so a process named "\e]8;;http://evil\a…" can't write
// escape sequences to the user's terminal (skeptic pass 2, B4). Local mode never passes
// through the hub's Normalize, and an upstream hub's cleaning is not ours to rely on.

import (
	"reflect"

	"github.com/ContractorKeith/machine-monitor/internal/textclean"
)

// clean applies the shared textclean policy to s.
func clean(s string) string { return textclean.Clean(s) }

// needsClean is the fast path: true when clean might change s.
func needsClean(s string) bool { return textclean.Needs(s) }

// cleanAll cleans every string reachable from v, which must be a pointer: strings, *string,
// and those inside structs, slices, arrays and maps' values. It edits in place, which is
// safe because the views are the only readers of what the stream delivers.
func cleanAll(v any) {
	walk(reflect.ValueOf(v))
}

func walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walk(v.Elem())
		}
	case reflect.String:
		if v.CanSet() {
			if s := v.String(); needsClean(s) {
				v.SetString(clean(s))
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				walk(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		if et := v.Type().Elem().Kind(); et == reflect.Uint8 || et == reflect.Float64 || et == reflect.Bool {
			return // byte and number series are the bulk of a report and hold no text
		}
		for i := range v.Len() {
			walk(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() == reflect.String {
				if s := e.String(); needsClean(s) {
					v.SetMapIndex(k, reflect.ValueOf(clean(s)).Convert(e.Type()))
				}
			}
		}
	}
}
