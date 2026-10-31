//go:build !windows

package main

// detachConsole is Windows-only; services elsewhere have no console window.
func detachConsole() {}
