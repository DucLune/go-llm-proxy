package handler

import (
	"context"
	"time"
)

// requestTimeout returns a context for one proxied upstream exchange,
// its cancel func, and a disarm func.
//
// The context is cancelled `seconds` after arming UNLESS disarm is
// called first. Callers disarm the moment they know the exchange is a
// streaming response: model.Timeout is a guard against hung backends,
// but applied to a live stream it becomes a wall-clock cap that severs
// healthy long generations mid-flight — a reasoning model can
// legitimately stream for longer than any fixed budget, and the kill
// manifests downstream as an abrupt connection reset with no error
// event (observed 2026-07-15: every generation exceeding the 300s
// default died at exactly 300s, sending resume-capable clients into a
// re-think/re-issue loop).
//
// A disarmed exchange is still bounded: the shared transport's
// DialContext (10s) / TLSHandshake (10s) / ResponseHeaderTimeout (30s)
// cover the pre-stream phases, and the parent context (the client's
// own connection) cancels the upstream read when the client goes away.
func requestTimeout(parent context.Context, seconds int) (ctx context.Context, cancel context.CancelFunc, disarm func()) {
	ctx, cancel = context.WithCancel(parent)
	t := time.AfterFunc(time.Duration(seconds)*time.Second, cancel)
	return ctx, cancel, func() { t.Stop() }
}
