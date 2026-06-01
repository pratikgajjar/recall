package main

import "context"

// StreamingAdapter is an optional capability for adapters that can emit results
// in batches. It lets indexing commit checkpoints as it goes — bounding memory
// and, crucially, resuming mid-provider after an interruption instead of
// re-reading the whole source. Adapters that don't implement it fall back to
// Scan (one batch, checkpoint committed once at the end).
type StreamingAdapter interface {
	ScanStream(ctx context.Context, prev string, emit EmitFunc) error
}

// EmitFunc ingests a batch of sessions/messages and persists the checkpoint
// that is valid as of that batch. After emit returns, everything up to and
// including this batch is durably indexed and safe to resume from.
type EmitFunc func(sessions []Session, msgs []Message, checkpoint string) error

// Flush thresholds: emit a batch once it crosses either bound. Small enough to
// keep memory flat and lose little work on interrupt, large enough that the
// per-batch checkpoint write (and its re-encode) stays negligible.
var (
	streamBatchMessages = 10000
	streamBatchSessions = 1000
)

// collectScan adapts a StreamingAdapter to the one-shot Scan signature by
// accumulating every emitted batch. Lets Scan keep working for callers that
// want everything at once (tests, the MCP background refresh).
func collectScan(ctx context.Context, a StreamingAdapter, prev string) ([]Session, []Message, string, error) {
	var ss []Session
	var mm []Message
	var ck string
	err := a.ScanStream(ctx, prev, func(sessions []Session, msgs []Message, checkpoint string) error {
		ss = append(ss, sessions...)
		mm = append(mm, msgs...)
		ck = checkpoint
		return nil
	})
	return ss, mm, ck, err
}

// batchEmitter accumulates sessions/messages and flushes them via emit once a
// size threshold is crossed, tagging each flush with the current checkpoint.
type batchEmitter struct {
	emit     EmitFunc
	ckpt     func() string // current cumulative checkpoint
	sessions []Session
	msgs     []Message
}

func newBatchEmitter(emit EmitFunc, ckpt func() string) *batchEmitter {
	return &batchEmitter{emit: emit, ckpt: ckpt}
}

func (b *batchEmitter) add(s Session, msgs []Message) error {
	b.sessions = append(b.sessions, s)
	b.msgs = append(b.msgs, msgs...)
	if len(b.msgs) >= streamBatchMessages || len(b.sessions) >= streamBatchSessions {
		return b.flush()
	}
	return nil
}

// flush emits the accumulated batch plus the current checkpoint. It is always
// safe to call, including with an empty batch — a no-op ingest still advances
// the checkpoint so trailing unchanged files persist their watermark.
func (b *batchEmitter) flush() error {
	if err := b.emit(b.sessions, b.msgs, b.ckpt()); err != nil {
		return err
	}
	b.sessions = nil
	b.msgs = nil
	return nil
}
