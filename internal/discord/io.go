package discord

import (
	"io"
)

// readAll is a tiny wrapper so we can swap implementations without
// dragging io.ReadAll's error-shape quirks into call sites. io.ReadAll
// already returns ([]byte, error); this indirection exists purely to
// keep handler.go readable.
func readAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
