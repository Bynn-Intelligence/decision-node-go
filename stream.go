package decisionnode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// openStream sends a GET with the default retries and returns the body of a
// successful response unread, for a caller that reads it as it arrives. The
// client's timeout bounds the wait for the response's headers and then each
// pause between bytes of the body, not the whole body, so a large body of
// any length can be read. Close the body to free the connection.
func (c *Client) openStream(ctx context.Context, path string) (*response, io.ReadCloser, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	if c.apiKey == "" {
		return nil, nil, &AuthenticationError{&Error{msg: "no API key: pass WithAPIKey or set DECISIONNODE_API_KEY"}}
	}
	for attempt := 0; ; attempt++ {
		resp, body, header, err := c.streamAttempt(ctx, path, attempt)
		if err == nil {
			return resp, body, nil
		}
		if ctx.Err() != nil || attempt >= c.maxRetries || !retryable(err, retryDefault) {
			return nil, nil, err
		}
		wait := backoff(attempt)
		if header != nil {
			if d, ok := retryAfter(header); ok {
				wait = d
			}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, nil, &Error{msg: "the request was canceled", Err: err}
		}
	}
}

func (c *Client) streamAttempt(ctx context.Context, path string, attempt int) (*response, io.ReadCloser, http.Header, error) {
	actx, cancel := context.WithCancel(ctx)
	var fired atomic.Bool
	timer := time.AfterFunc(c.timeout, func() { fired.Store(true); cancel() })
	fail := func() {
		timer.Stop()
		cancel()
	}
	req, err := http.NewRequestWithContext(actx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		fail()
		return nil, nil, nil, &Error{msg: "invalid request: " + err.Error(), Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/x-ndjson, application/json")
	for k, vs := range c.headers {
		req.Header[k] = append([]string(nil), vs...)
	}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		fail()
		c.log(ctx, http.MethodGet, path, 0, "", time.Since(start), attempt, err)
		if ctx.Err() == nil && fired.Load() {
			return nil, nil, nil, &TimeoutError{&Error{msg: "the request timed out after " + c.timeout.String(), Err: err}}
		}
		return nil, nil, nil, c.transportError(ctx, actx, err)
	}
	c.log(ctx, http.MethodGet, path, resp.StatusCode, resp.Header.Get("x-request-id"), time.Since(start), attempt, nil)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fail()
		if rerr != nil {
			return nil, nil, nil, c.transportError(ctx, actx, rerr)
		}
		if c.onResponse != nil {
			c.onResponse(actx, ResponseInfo{Method: http.MethodGet, Path: path, Attempt: attempt + 1, Status: resp.StatusCode,
				Header: resp.Header.Clone(), Body: bytes.Clone(data)})
		}
		return nil, nil, resp.Header, newError(resp.StatusCode, resp.Header, data)
	}
	if c.onResponse != nil {
		// The body is read by the caller as it arrives, so the hook gets none.
		c.onResponse(actx, ResponseInfo{Method: http.MethodGet, Path: path, Attempt: attempt + 1, Status: resp.StatusCode,
			Header: resp.Header.Clone()})
	}
	timer.Reset(c.timeout)
	body := &idleBody{body: resp.Body, timer: timer, idle: c.timeout, cancel: cancel, fired: &fired}
	return &response{status: resp.StatusCode, header: resp.Header}, body, nil, nil
}

// idleBody is a response body whose read is abandoned when no byte arrives
// for the idle time.
type idleBody struct {
	body   io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
	fired  *atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 && !b.fired.Load() {
		b.timer.Reset(b.idle)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		if b.fired.Load() {
			return n, &TimeoutError{&Error{msg: "no byte of the response arrived for " + b.idle.String(), Err: err}}
		}
		return n, &ConnectionError{&Error{msg: "connection error: " + err.Error(), Err: err}}
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.body.Close()
	b.cancel()
	return err
}
