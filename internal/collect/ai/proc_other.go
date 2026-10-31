//go:build !linux

package ai

// procInfo is Linux-only; elsewhere GPU processes aren't matched by cmdline.
func procInfo(int) (argv []string, cwd string) { return nil, "" }
