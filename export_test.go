package decisionnode

import (
	"context"
	"time"
)

// WithSleep replaces the wait between attempts, so tests can record the
// waits the client schedules without waiting for them.
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// WithClock replaces the clock uploads, upload URLs and cache entries are
// judged by.
func WithClock(now func() time.Time) Option {
	return func(c *Client) { c.now = now }
}
