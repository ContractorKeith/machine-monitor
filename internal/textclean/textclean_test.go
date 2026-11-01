package textclean

import "testing"

func TestClean(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "omarchy-41", "omarchy-41"},
		{"tab and newline", "a\tb\nc\r\nd", "a b c  d"},
		{"c0 and del", "a\x00b\x07c\x7fd", "abcd"},
		{"c1", "a\u0080b\u009bc\u009fd", "abcd"},
		{"osc8", "\x1b]8;;http://evil.example/\x07LINK\x1b]8;;\x07pwn", "LINKpwn"},
		{"osc8 st", "\x1b]8;;http://evil.example/\x1b\\LINK\x1b]8;;\x1b\\", "LINK"},
		{"csi", "\x1b[31mred\x1b[0m", "red"},
		{"dcs", "a\x1bP1;2|data\x1b\\b", "ab"},
		{"apc", "a\x1b_Gf=24;AAAA\x1b\\b", "ab"},
		{"bidi", "a\u202ecod.exe\u200e\u200f\u202a\u202b\u202c\u202d\u2066\u2067\u2068\u2069b", "acod.exeb"},
		{"arabic mark", "a\u061cb", "ab"},
		{"zero width", "o\u200bm\u2060a\ufeffr", "omar"},
		{"invisible math", "a\u2061\u2062\u2063\u2064\u206a\u206f⁥b", "ab"},
		{"hangul fillers", "aㅤᅟᅠﾠb", "ab"},
		{"cgj and soft hyphen", "a͏\u00adb", "ab"},
		{"variation selectors", "a︀︎b\U000e0100c", "abc"},
		{"tags", "a\U000e0001\U000e0041\U000e007fb", "ab"},
		{"line separators", "a b c", "a b c"},
		{"mongolian", "a\u180e᠋b", "ab"},
		{"invalid utf8", "a\xffb\xc3", "ab"},
		{"neighbours stay", "\u2064x\u202f\u00a1\u00a0\u061b", "x ¡ ؛"},
		{"family zwj", "👨\u200d👩\u200d👧", "👨\u200d👩\u200d👧"},
		{"rainbow flag", "🏳️\u200d🌈", "🏳️\u200d🌈"},
		{"persian zwnj", "می\u200cخواهم", "می\u200cخواهم"},
		{"malayalam chillu", "ന്\u200d", "ന്\u200d"},
		{"leading joiner", "\u200dab", "ab"},
		{"trailing joiner", "ab\u200d", "ab"},
		{"repeated joiners", "a\u200d\u200d\u200cb", "ab"},
		{"joiner by space", "a \u200d b", "a  b"},
		{"joiner after bidi", "a\u202e\u200db", "ab"},
		{"stray fe0f", "a️b", "ab"},
		{"keycap", "1️⃣", "1️⃣"},
		{"heart", "❤️", "❤️"},
		{"unterminated osc", "x\x1b]8;;http://evil", "x"},
	}
	for _, c := range cases {
		if got := Clean(c.in); got != c.want {
			t.Errorf("%s: Clean(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Text that needs no change is returned without a copy.
func TestCleanPlainIsFree(t *testing.T) {
	if Needs("Ünïcode ✓ 日本") {
		t.Error("plain text flagged")
	}
	if n := testing.AllocsPerRun(10, func() { _ = Clean("hello world") }); n != 0 {
		t.Errorf("allocs = %v", n)
	}
}

// Strip differs from Clean only in deleting line breaks and tabs, as upstream's cleanText does.
func TestStrip(t *testing.T) {
	if got := Strip("a\tb\nc d\x1b[1me"); got != "abcde" {
		t.Errorf("Strip = %q", got)
	}
}

func TestCleanHiddenJoinerNeighbor(t *testing.T) {
	for _, s := range []string{"a\u200d\u115f", "a\u200c\u3164", "a\u200d\ufe0f"} {
		if got := Clean(s); got != "a" {
			t.Errorf("Clean(%q) = %q", s, got)
		}
	}
}
func TestCleanBlankLookalikes(t *testing.T) {
	for _, r := range []rune{'\u2800', '\u00a0', '\u1680', '\u2000', '\u2007', '\u202f', '\u205f', '\u3000'} {
		s := "a" + string(r) + "b"
		if Clean(s) != "a b" || Strip(s) != "a b" {
			t.Errorf("blank U+%04X not normalized", r)
		}
	}
}
func FuzzCleanIdempotent(f *testing.F) {
	for _, s := range []string{"a\u200d\u115f", "a\u200d\ufe0f", "ന്\u200d", "👨\u200d👩", "a\u2800b", "a\xff\u200d\u3164"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, clean := range []func(string) string{Clean, Strip} {
			once := clean(s)
			if twice := clean(once); once != twice {
				t.Fatalf("%q -> %q -> %q", s, once, twice)
			}
		}
	})
}

func TestCleanLatinJoiners(t *testing.T) {
	for _, s := range []string{"a\u200db", "a\u200cb", "y\u200d4"} {
		want := "ab"
		if s[0] == 'y' {
			want = "y4"
		}
		if got := Clean(s); got != want {
			t.Errorf("Clean(%q)=%q, want %q", s, got, want)
		}
	}
}
