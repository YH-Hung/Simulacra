package simulacratest

import "time"

// SetRPCTimeout overrides the per-RPC deadline so a test can bound a hanging
// server in milliseconds instead of waiting out defaultRPCTimeout.
func SetRPCTimeout(c *Client, d time.Duration) { c.timeout = d }

// WrapAdmin replaces the Client's admin clients with wrap's result, so a test
// can inject a failure or a counter a healthy server cannot produce.
func WrapAdmin(c *Client, wrap func(Admin) Admin) { c.admin = wrap(c.admin) }
