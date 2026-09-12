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

// Test-only views of the error mapper.
var (
	ConnectError    = connectError
	InvalidArgument = invalidArgument
)

// Test-only views of call rendering.
var (
	RenderCall = renderCall
	ValidUTF8  = validUTF8
)

// StubEnvelope is a test-only view of the stub envelope conversion.
var StubEnvelope = stubEnvelope

// StreamCalls is a test-only view of WatchCalls' send loop.
var StreamCalls = streamCalls
