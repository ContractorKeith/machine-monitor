package nocomment

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var (
	update = flag.Bool("update", false, "lower baseline.json to the current comment counts")
	list   = flag.String("list", "", "write every remaining comment to this file")
	base   = flag.String("base", "", "git ref; fail if any .go file differs from it in more than comments")
)

const repoRoot = "../../.."

func readBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile("baseline.json")
	if err != nil {
		t.Fatal(err)
	}
	baseline := map[string]int{}
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	return baseline
}

func TestNoNewComments(t *testing.T) {
	comments, err := Scan(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	counts := Counts(comments)
	baseline := readBaseline(t)
	grew, shrank := Compare(counts, baseline)

	for _, v := range grew {
		t.Errorf("%s: %d comment lines, baseline allows %d. Comments are banned; put the why in a test, a name or docs/adr.", v.File, v.Have, v.Baseline)
		for _, c := range comments {
			if c.File == v.File {
				t.Logf("  %s:%d %s", c.File, c.Line, c.Text)
			}
		}
	}
	if len(grew) > 0 {
		return
	}
	if *update {
		data, err := json.MarshalIndent(counts, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("baseline.json", append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, v := range shrank {
		t.Errorf("%s: %d comment lines, below baseline %d. Lock it in: go test ./internal/lint/nocomment -update", v.File, v.Have, v.Baseline)
	}
}

func TestScanFlagsCommentsAndAllowsDirectives(t *testing.T) {
	dir := t.TempDir()
	src := "//go:build linux\n\npackage x\n\n// Doc says hi.\nfunc F() {}\n\nvar y = 1 // trailing\n\n/*\nblock\n*/\nvar z = 2\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "y.go"), []byte("package y\n// ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	comments, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := Counts(comments)["x.go"]; got != 5 {
		t.Fatalf("x.go comment lines = %d, want 5 (doc 1 + trailing 1 + block 3): %+v", got, comments)
	}
	if _, ok := Counts(comments)["testdata/y.go"]; ok {
		t.Fatal("testdata must be skipped")
	}
}

func TestCompareRatchetsBothWays(t *testing.T) {
	grew, shrank := Compare(map[string]int{"a.go": 3, "new.go": 1, "b.go": 1}, map[string]int{"a.go": 3, "b.go": 2, "gone.go": 4})
	if len(grew) != 1 || grew[0].File != "new.go" {
		t.Fatalf("grew = %+v, want new.go only", grew)
	}
	if len(shrank) != 2 || shrank[0].File != "b.go" || shrank[1].File != "gone.go" {
		t.Fatalf("shrank = %+v, want b.go and gone.go", shrank)
	}
}

func TestList(t *testing.T) {
	if *list == "" {
		t.Skip("set -list=<file> to write the comment inventory")
	}
	comments, err := Scan(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range comments {
		fmt.Fprintf(&b, "=== %s:%d\n%s\n", c.File, c.Line, c.Text)
	}
	if err := os.WriteFile(*list, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d comments in %d files written to %s", len(comments), len(Counts(comments)), *list)
}

func codeTokens(src []byte) string {
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile("", fset.Base(), len(src)), src, nil, 0)
	var b strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return b.String()
		}
		if tok == token.SEMICOLON {
			lit = ";"
		}
		fmt.Fprintf(&b, "%d %q\n", tok, lit)
	}
}

func TestOnlyCommentsChanged(t *testing.T) {
	if *base == "" {
		t.Skip("set -base=<git ref> to check that only comments changed")
	}
	out, err := exec.Command("git", "-C", repoRoot, "diff", "--name-only", *base, "--", "*.go").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "internal/lint/nocomment/") {
			continue
		}
		cur, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		old, err := exec.Command("git", "-C", repoRoot, "show", *base+":"+f).Output()
		if err != nil {
			t.Errorf("%s: new file, not a comment-only change", f)
			continue
		}
		if codeTokens(old) != codeTokens(cur) {
			t.Errorf("%s: code changed, not just comments", f)
		}
	}
}
