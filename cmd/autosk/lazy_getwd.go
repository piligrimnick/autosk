package main

import "os"

// getwdImpl is the actual os.Getwd call. Lives in its own file so the
// test build can override it via a stub (currently unused, but
// trivial to flip on).
func getwdImpl() (string, error) { return os.Getwd() }
