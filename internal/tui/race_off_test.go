//go:build !race

package tui

// raceDetector is true under go test -race (see race_on_test.go).
const raceDetector = false
