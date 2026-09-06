package admin

import "time"

// SetHandshakeTimeout swaps the h2c preface bound and returns a func that
// restores it. It lives in export_test.go, so it exists only in test builds and
// is not part of the package's real surface.
func SetHandshakeTimeout(d time.Duration) func() {
	previous := handshakeTimeout
	handshakeTimeout = d
	return func() { handshakeTimeout = previous }
}
