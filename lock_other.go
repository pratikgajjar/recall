//go:build !unix

package main

import "context"

// lockIndexWriter is a no-op where flock is unavailable; see lock_unix.go.
// Releases ship for darwin and linux only.
func lockIndexWriter(context.Context, string) (func(), error) { return func() {}, nil }
