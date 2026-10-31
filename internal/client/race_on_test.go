//go:build race

package client

// raceDetector is true under go test -race, whose instrumentation makes link timings
// meaningless; the plain test run checks them.
const raceDetector = true
