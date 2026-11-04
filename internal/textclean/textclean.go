// Package textclean is the one policy for text that came from another machine and ends up on
// a screen, in a log or in an alert: report.CleanText, the TUI and the alerts all call it, so
// they can't disagree about what is hidden or dangerous (skeptic pass 3, N15 and N18).
//
// Clean removes:
//   - terminal escape sequences: ESC and C1 CSI, OSC (so OSC 8 links), DCS, APC, PM and SOS;
//   - C0 and C1 control characters (tab, line feed and carriage return become a space, as do
//     U+2028 and U+2029, so a field stays on one row);
//   - bidi controls (U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069);
//   - zero-width and invisible characters: U+00AD, U+034F, U+115F, U+1160, U+17B4, U+17B5,
//     U+180B–U+180F, U+200B, U+2060–U+206F, U+3164, U+FE00–U+FE0E, U+FEFF, U+FFA0,
//     U+FFF9–U+FFFB, the tag characters and variation selectors in U+E0000–U+E0FFF;
//   - invalid UTF-8.
//
// Kept on purpose: U+200D (ZWJ) in emoji sequences; ZWJ and U+200C (ZWNJ) between
// joining-script letters and after a combining mark (Malayalam chillu, Persian words); U+FE0F after a symbol or digit (emoji
// presentation). A joiner that leads, repeats or follows anything else goes.
package textclean

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Clean applies the policy to s, turning line breaks and tabs into spaces so a field stays
// on one row. Text that needs no change comes back as is, uncopied.
func Clean(s string) string { return apply(s, true) }

// Strip is Clean for callers that must match upstream's cleanText, which deletes tabs and
// line breaks instead of spacing them (report.CleanText). Everything else is identical.
func Strip(s string) string { return apply(s, false) }

func apply(s string, spaces bool) string {
	if !Needs(s) {
		return s
	}
	// Resolve unconditional removals first: joiners must see their final neighbors.
	var filtered []rune
	for _, r := range decode(ansi.Strip(s)) {
		if r == '\t' || r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029 {
			if spaces {
				filtered = append(filtered, ' ')
			}
		} else if !unicode.IsControl(r) && !hidden(r) {
			if blank(r) {
				r = ' '
			}
			filtered = append(filtered, r)
		}
	}
	rs := filtered
	var b strings.Builder
	b.Grow(len(s))
	var prev, base rune // the last rune written, and the last one that isn't a mark
	for i, r := range rs {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029:
			if !spaces {
				continue
			}
			r = ' '
		case unicode.IsControl(r), hidden(r):
			continue
		case isJoiner(r) && !joinerOK(prev, base, rs, i):
			continue
		case r == 0xfe0f && !emojiBase(prev):
			continue
		}
		b.WriteRune(r)
		prev = r
		if !unicode.IsMark(r) {
			base = r
		}
	}
	return b.String()
}

// decode drops invalid UTF-8 bytes; a real U+FFFD stays.
func decode(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "")
}

// Needs is the fast path: true when Clean might change s. Joiners and U+FE0F always take
// the slow path, where Clean decides.
func Needs(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f:
			return true
		case r < 0x80:
		case blank(r), hidden(r), isJoiner(r), r == 0xfe0f, r == 0x2028, r == 0x2029:
			return true
		}
	}
	return false
}

// isJoiner is the zero-width (non-)joiner.
func isJoiner(r rune) bool { return r == 0x200c || r == 0x200d }

// hidden reports invisible characters that are always removed (the joiners and U+FE0F are
// conditional, see Clean).
func hidden(r rune) bool {
	switch {
	case r == 0x00ad, r == 0x034f, r == 0x061c, r == 0x115f, r == 0x1160, r == 0x17b4, r == 0x17b5:
		return true
	case r >= 0x180b && r <= 0x180f:
		return true
	case r == 0x200b, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2060 && r <= 0x206f: // includes the unassigned U+2065
		return true
	case r == 0x3164, r == 0xfeff, r == 0xffa0, r >= 0xfff9 && r <= 0xfffb:
		return true
	case r >= 0xfe00 && r <= 0xfe0e:
		return true
	case r >= 0xe0000 && r <= 0xe0fff:
		return true
	}
	return false
}

// joinerOK preserves shaping marks and joining-script letters, and ZWJ emoji
// sequences. Latin letters and digits never need an invisible joiner. After a mark,
// the letter the mark sits on (base) decides: "é" + ZWJ is a lookalike, a virama is not.
func joinerOK(prev, base rune, rs []rune, i int) bool {
	if unicode.IsMark(prev) && prev != 0xfe0f {
		return joiningLetter(base) && (i+1 == len(rs) || !isJoiner(rs[i+1]))
	}
	if i+1 == len(rs) {
		return false
	}
	next := rs[i+1]
	if rs[i] == 0x200d && (emojiRune(prev) || prev == 0xfe0f) && emojiRune(next) {
		return true
	}
	return joiningLetter(prev) && joiningLetter(next)
}

func emojiRune(r rune) bool {
	return r >= 0x1f000 && r <= 0x1faff || r >= 0x2600 && r <= 0x27bf
}

func joiningLetter(r rune) bool {
	if !unicode.IsLetter(r) {
		return false
	}
	return unicode.In(r, unicode.Arabic, unicode.Syriac, unicode.Thaana,
		unicode.Nko, unicode.Mandaic, unicode.Mongolian, unicode.Devanagari,
		unicode.Bengali, unicode.Gurmukhi, unicode.Gujarati, unicode.Oriya,
		unicode.Tamil, unicode.Telugu, unicode.Kannada, unicode.Malayalam,
		unicode.Sinhala, unicode.Myanmar, unicode.Khmer)
}

// emojiBase reports whether U+FE0F may follow r: a symbol or a digit, #, * (keycaps).
func emojiBase(r rune) bool {
	return unicode.IsSymbol(r) || r >= 0x1f000 && r <= 0x1faff || unicode.IsDigit(r) || r == '#' || r == '*' || r == 0xa9 || r == 0xae
}

// blank normalizes Unicode spacing and blank glyphs to an ordinary space.
func blank(r rune) bool { return r != ' ' && (unicode.IsSpace(r) || r == 0x2800) }
