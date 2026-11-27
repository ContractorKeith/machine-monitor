package textclean

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func Clean(s string) string { return apply(s, true) }

func Strip(s string) string { return apply(s, false) }

func apply(s string, spaces bool) string {
	if !Needs(s) {
		return s
	}

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
	var prev, base rune
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

func decode(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "")
}

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

func isJoiner(r rune) bool { return r == 0x200c || r == 0x200d }

func hidden(r rune) bool {
	switch {
	case r == 0x00ad, r == 0x034f, r == 0x061c, r == 0x115f, r == 0x1160, r == 0x17b4, r == 0x17b5:
		return true
	case r >= 0x180b && r <= 0x180f:
		return true
	case r == 0x200b, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2060 && r <= 0x206f:
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

func emojiBase(r rune) bool {
	return unicode.IsSymbol(r) || r >= 0x1f000 && r <= 0x1faff || unicode.IsDigit(r) || r == '#' || r == '*' || r == 0xa9 || r == 0xae
}

func blank(r rune) bool { return r != ' ' && (unicode.IsSpace(r) || r == 0x2800) }
