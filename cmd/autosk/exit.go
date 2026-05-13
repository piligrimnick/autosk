package main

import "errors"

// errSilentExit1 makes a command exit with status 1 without printing the
// error message again (we've already printed something appropriate).
//
// main.go inspects this sentinel before calling os.Exit.
var errSilentExit1 = errors.New("silent exit 1")
