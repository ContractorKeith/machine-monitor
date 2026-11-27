package nocomment

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var allowedPrefixes = []string{
	"//go:",
	"//nolint",
	"//lint:",
	"// +build",
	"// Code generated ",
	"// Output:",
	"// Unordered output:",
}

func allowed(text string) bool {
	for _, p := range allowedPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

type Comment struct {
	File  string
	Line  int
	Lines int
	Text  string
}

func Scan(root string) ([]Comment, error) {
	var out []Comment
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, g := range f.Comments {
			for _, c := range g.List {
				if allowed(c.Text) {
					continue
				}
				start := fset.Position(c.Pos()).Line
				end := fset.Position(c.End()).Line
				out = append(out, Comment{File: rel, Line: start, Lines: end - start + 1, Text: c.Text})
			}
		}
		return nil
	})
	return out, err
}

func Counts(comments []Comment) map[string]int {
	counts := map[string]int{}
	for _, c := range comments {
		counts[c.File] += c.Lines
	}
	return counts
}

type Violation struct {
	File     string
	Have     int
	Baseline int
}

func Compare(counts, baseline map[string]int) (grew, shrank []Violation) {
	files := map[string]bool{}
	for f := range counts {
		files[f] = true
	}
	for f := range baseline {
		files[f] = true
	}
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		v := Violation{File: f, Have: counts[f], Baseline: baseline[f]}
		switch {
		case v.Have > v.Baseline:
			grew = append(grew, v)
		case v.Have < v.Baseline:
			shrank = append(shrank, v)
		}
	}
	return grew, shrank
}
