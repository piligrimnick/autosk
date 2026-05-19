//go:build !race

package main

// raceEnabled is true only when the binary was built with -race.
// Used by the lazy e2e tests to skip the fixtures that read
// gocui.Screen directly (a known fixture race; the production
// internal/lazy/... code is race-clean). The honest band-aid:
// under -race we skip those tests until the fixture is refactored
// to drive reads through g.Update closures.
const raceEnabled = false
