//go:build race

package tui

// raceDetector is true under go test -race, whose instrumentation makes frame times
// meaningless; the plain test run checks the frame budget.
const raceDetector = true
