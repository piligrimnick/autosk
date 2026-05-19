package tui

import "fmt"

// sscanfShim isolates the one fmt.Sscanf call this package needs so
// refresh.go can keep its imports tight.
func sscanfShim(s, format string, a ...any) (int, error) {
	return fmt.Sscanf(s, format, a...)
}
