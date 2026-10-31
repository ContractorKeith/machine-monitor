package tui

// Everything the TUI draws from another machine (a report, a process list, an alert, a hub
// error) goes through clean first, so a process named "\e]8;;http://evil\a…" can't write
// escape sequences to the user's terminal (skeptic pass 2, B4). Local mode never passes
// through the hub's Normalize, and an upstream hub's cleaning is not ours to rely on.

import (
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// clean removes terminal escape sequences (CSI, OSC, DCS and the like), C0/C1 control
// characters and bidi/zero-width format characters from s. Tabs and line breaks become
// spaces so a field stays on one row. Plain text comes back unchanged and uncopied.
func clean(s string) string {
	if !needsClean(s) {
		return s
	}
	s = ansi.Strip(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r == utf8.RuneError, unicode.IsControl(r), hiddenFormat(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// needsClean is the fast path: true when s holds anything clean would change.
func needsClean(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) || hiddenFormat(r) {
			return true
		}
	}
	return false
}

// hiddenFormat reports invisible format characters: bidi marks and overrides, zero-width
// characters, the word joiner and the byte order mark.
func hiddenFormat(r rune) bool {
	switch {
	case r == 0x061c, r == 0xfeff, r == 0x180e:
		return true
	case r >= 0x200b && r <= 0x200f, r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

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
