---
name: delete-comment
description: Audit and delete code comments in machine-monitor, sort each one into Tautology, Stale, Excuse or Decision Record, file issues for Excuses, move Decision Records into docs/adr, and lower the nocomment lint baseline. Use for /delete-comment, "delete comments", "comment audit", or when the nocomment test fails because a file gained comments.
---

# delete-comment

Comments are banned in this repo. `internal/lint/nocomment` (run by `go test ./...` in CI)
holds a per-file baseline in `baseline.json`: a file may never gain comment lines, and when
one loses them the baseline must be lowered in the same PR. This skill drives the count to zero
without losing the knowledge some comments carry.

Allowed and never counted: `//go:`, `//nolint`, `//lint:`, `// +build`, `// Code generated `,
`// Output:` and `// Unordered output:`. Files under `testdata/` and dot dirs are skipped.

## 1. Inventory

```bash
go test -count=1 ./internal/lint/nocomment -run TestList -args -list=/tmp/comments.txt
```
Pick a scope (a package, or the files a PR touches). More than ~400 comments: split by package
and run parallel workers in their own worktrees, each with a disjoint scope; one integrator
writes ADRs, files issues and updates the baseline.

## 2. Categorise every comment

| Category | Test | Action |
|---|---|---|
| Tautology | Restates the code, name or signature; section banners | Delete |
| Stale | Names code, issues, milestones or behaviour that no longer match (check the code) | Delete |
| Excuse | Justifies a hack, TODO, "for now", "best effort", a known gap, and the code really is hacky | Delete, file an issue |
| Decision Record | A non-obvious why: protocol or upstream-compat rule, security property, platform quirk, trade-off, a bug the odd code prevents | Delete, record in an ADR |
| Attribution | Upstream port notice | Delete, add the file to `NOTICE.md` |

Rule of thumb: if deleting it would let the next person "simplify" the code back into a bug,
it is a Decision Record. A sentence of real why inside a tautological doc comment is still one;
keep just that sentence's knowledge.

Keep a per-comment ledger (`file start end category action note`) outside the repo so the
integrator can check coverage against the inventory.

## 3. Delete

Remove whole comment lines; for trailing comments drop the comment and the space before it.
Run `gofmt -w`. Change nothing else, not even for Excuses; fixes go through their issue.
Prove it:
```bash
go test -count=1 ./internal/lint/nocomment -run TestOnlyCommentsChanged -args -base=origin/main
```

## 4. Decision Records → ADRs

`docs/adr/NNNN-kebab-title.md`, numbered after the highest existing one, listed in
`docs/adr/README.md`. Group by topic, not one ADR per comment. Before adding, read the existing
ADRs: extend the matching one instead of creating a near-duplicate. Format:
`# NNNN. Title`, `Status: Accepted (YYYY-MM-DD)`, then `## Context`, `## Decision`,
`## Consequences`, `## Where` (package or file paths, not line numbers).

## 5. Excuses → issues

One issue per root cause, label `comment-excuse`. Search first:
`gh issue list --label comment-excuse --state all`. Body: where, the quoted comment, what is
hacky (verified against the code), suggested fix.

## 6. Ratchet and gate

```bash
go test -count=1 ./internal/lint/nocomment -update     # lowers baseline.json; refuses if a file grew
test -z "$(gofmt -l .)" && go vet ./... && staticcheck ./... && go test ./...
```
Commit the code, ADRs, NOTICE and `baseline.json` together:
`chore: delete comments in <scope>`. Never raise a baseline number by hand; if a comment
seems essential, it belongs in an ADR, a test name or an identifier instead.
