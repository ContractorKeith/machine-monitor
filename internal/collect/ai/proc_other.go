//go:build !linux

package ai

func procInfo(int) (argv []string, cwd string) { return nil, "" }
