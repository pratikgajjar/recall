//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockIndexWriter takes an exclusive lock that serialises `recall index` runs
// against one index, and returns its release func.
//
// The pi extension starts an incremental index after every agent turn, so with
// several pi sessions open two runs regularly overlap. Both read the same
// checkpoint, and both then append the same new messages: a real index had 191
// duplicated messages across 11 live pi sessions, with msg_count and token
// totals counted twice. SQLite's write lock does not prevent it, because the
// checkpoint is read before the write transactions begin.
//
// Waiting is right rather than skipping: once the first run finishes, the second
// reads the checkpoint it committed and only does the remaining work, which is
// usually none. The lock is polled so Ctrl+C still cancels a waiting run.
func lockIndexWriter(ctx context.Context, indexPath string) (func(), error) {
	f, err := os.OpenFile(indexPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	warned := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		if !warned {
			fmt.Fprintln(os.Stderr, "  waiting for another `recall index` to finish…")
			warned = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
