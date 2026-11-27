# 0001. No code comments; decisions live in ADRs

Status: Accepted (2026-11-27)

## Context

The codebase was mostly written by agents and carried about 4,600 lines of comments in 3,094
groups. An audit sorted every one into four kinds. About 74% restated the code (Tautology).
Under 1% described code that had since changed (Stale). Under 1% justified a shortcut
(Excuse). About 24% recorded a real, non-obvious reason (Decision Record). The other 1% were
upstream attribution headers. The Decision Records
were spread across hundreds of files. Excuses let the same shortcut be copied without anyone
looking at it again.

## Decision

- Go source carries no comments. Allowed exceptions are directives the toolchain reads:
  `//go:`, `//nolint`, `//lint:`, `// +build`, `// Code generated `, `// Output:` and
  `// Unordered output:`.
- `internal/lint/nocomment` runs in `go test ./...` on every CI platform. It compares each
  file's comment lines with `baseline.json`. A file may never gain comments. When a file loses
  them, the baseline must be lowered in the same change (`go test ./internal/lint/nocomment
  -update`), so the count only goes down. The baseline after this change is empty.
- The non-obvious reasons go into area ADRs in this directory. Excuses become GitHub issues
  labelled `comment-excuse`.
- Upstream attribution moves from per-file header comments to `NOTICE.md`, which ships in
  every release archive (PLAN §11).
- The `delete-comment` skill (`.agents/skills/delete-comment/`) repeats the audit.

## Consequences

- Readers start from names, tests and the ADR for the package. Before changing a package,
  read its ADR. When a change makes a recorded decision wrong, update that ADR in the same PR.
- Exported identifiers have no doc comments, so `go doc` shows bare signatures. That is
  acceptable for an internal-only module with no library users.
- A reason that seems to need a comment needs a clearer name, a test whose name states the
  rule, or an ADR section instead.
- Shell and PowerShell installers (`internal/hub/install.sh`, `install.ps1`) were cleared by
  hand in this change. The lint does not cover them.

## Where

`internal/lint/nocomment/`, `docs/adr/`, `NOTICE.md`, `.agents/skills/delete-comment/`
