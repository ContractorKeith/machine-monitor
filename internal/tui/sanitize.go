package tui

import (
	"reflect"

	"github.com/ContractorKeith/machine-monitor/internal/textclean"
)

func clean(s string) string { return textclean.Clean(s) }

func needsClean(s string) bool { return textclean.Needs(s) }

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
			return
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
