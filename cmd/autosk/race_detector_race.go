//go:build race

package main

// raceEnabled is true only when the binary was built with -race.
// See race_detector.go for the rationale (test-fixture race in
// gocui.Screen reads).
const raceEnabled = true
