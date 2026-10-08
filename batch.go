package decisionnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxAddBytes is the most a batch add call may carry; Add splits a
	// larger list into several calls, in order.
	maxAddBytes = 10 * 1024 * 1024
	// MinPollInterval is the shortest poll interval RunBatch accepts.
	MinPollInterval = 10 * time.Second
	// DefaultPollInterval is RunBatch's poll interval when none is given.
	DefaultPollInterval = 60 * time.Second
	// DefaultBatchTimeout is how long RunBatch waits for a batch to end when
	// no timeout is given: the 24 hours a batch may run, and two more.
	DefaultBatchTimeout = 26 * time.Hour

	ndjson = "application/x-ndjson"
)

// BatchRequest is one line of a batch: a decide request and its custom id,
// unique within the batch. Its images follow the inline threshold per line;
// uploaded ones are held by the batch.
type BatchRequest struct {
	CustomID string
	Request
}

// Batch is a batch object: every field the API sends.
type Batch struct {
	// ID is "batch_" and 10 characters.
	ID string
	// State is draft, queued, running, completed, failed, cancelled or
	// expired.
	State    string
	Counters BatchCounters
	// The times are as sent (RFC 3339, UTC); "" where the API sent null.
	CreatedAt       string
	FinalizedAt     string
	ExpiresAt       string
	EndedAt         string
	ResultsURL      string
	ResultsExpireAt string
	PriceFactor     float64
	Usage           Usage
	// Error is set when the batch failed.
	Error *BatchError
	// Raw is the batch object as sent.
	Raw map[string]any
}

// Ended reports whether the batch has reached completed, failed, cancelled
// or expired.
func (b *Batch) Ended() bool { return b.EndedAt != "" }

// BatchCounters counts a batch's lines by state.
type BatchCounters struct {
	Requests   int `json:"requests"`
	Queued     int `json:"queued"`
	InProgress int `json:"in_progress"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Expired    int `json:"expired"`
	Cancelled  int `json:"cancelled"`
}

// BatchError says why a batch failed.
type BatchError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

// BatchPage is one page of List.
type BatchPage struct {
	Batches []Batch
	Offset  int
	Limit   int
	Total   int
	// NextOffset is the offset of the next page, nil on the last.
	NextOffset *int
	Raw        map[string]any
}

// BatchListOptions pages List. A zero field is left to the server: offset
// 0, limit 20.
type BatchListOptions struct {
	Offset int
	Limit  int
}

// BatchResult is one line of a batch's results, in upload order.
type BatchResult struct {
	CustomID string
	// Status is the line's HTTP status, or 0 for a line that never ran.
	Status int
	// StatusText is "expired" or "cancelled" for a line that never ran, else
	// "".
	StatusText string
	// Decision is the line's decision when Status is 200, parsed as Decide
	// parses a response, numbers as sent.
	Decision *Decision
	// Err is the error the line's status and body give, as a value (the
	// same type Decide would return), for any other status. It is never
	// returned by Results.
	Err error
	// Headers holds the line's headers, by lower-case name.
	Headers map[string]string
	// Raw is the line as sent.
	Raw map[string]any
}

// BatchService is the Batch API: decide requests run asynchronously at
// half the price, ended within 24 hours of finalize. Get it from
// Client.Batches.
type BatchService struct{ c *Client }

// Batches returns the client's Batch API.
func (c *Client) Batches() *BatchService { return &BatchService{c: c} }

// Create creates a batch in state draft with POST /v1/batches, its lines as
// JSON Lines. With no requests the body is empty: an empty draft to Add to.
// Every line carries its custom id, a model (the request's, else the
// client's) and a state ("" when none). Every request is checked locally
// before anything is sent, as for Decide. Create is retried only on 429 and
// 529, never after a timeout or a lost answer, since a repeat could create a
// second draft.
func (b *BatchService) Create(ctx context.Context, reqs []BatchRequest) (*Batch, error) {
	if b.c.err != nil {
		return nil, b.c.err
	}
	lines, err := b.c.batchLines(ctx, reqs)
	if err != nil {
		return nil, err
	}
	payload := bytes.Join(lines, nil)
	if payload == nil {
		payload = []byte{}
	}
	return b.mutate(ctx, "/v1/batches", payload, ndjson)
}

// Add adds requests to a draft with POST /v1/batches/{id}/requests, in calls
// of at most 10 MiB each, in order, and returns the batch after the last.
// Like Create it is retried only on 429 and 529.
func (b *BatchService) Add(ctx context.Context, batchID string, reqs []BatchRequest) (*Batch, error) {
	if b.c.err != nil {
		return nil, b.c.err
	}
	if len(reqs) == 0 {
		return nil, errors.New("decisionnode: add needs at least one request")
	}
	lines, err := b.c.batchLines(ctx, reqs)
	if err != nil {
		return nil, err
	}
	return b.addLines(ctx, batchID, lines)
}

func (b *BatchService) addLines(ctx context.Context, batchID string, lines [][]byte) (*Batch, error) {
	var out *Batch
	path := "/v1/batches/" + url.PathEscape(batchID) + "/requests"
	for len(lines) > 0 {
		n, size := 0, 0
		for n < len(lines) && (n == 0 || size+len(lines[n]) <= maxAddBytes) {
			size += len(lines[n])
			n++
		}
		batch, err := b.mutate(ctx, path, bytes.Join(lines[:n], nil), ndjson)
		if err != nil {
			return nil, err
		}
		out, lines = batch, lines[n:]
	}
	return out, nil
}

// Finalize queues a draft with POST /v1/batches/{id}/finalize. It is retried
// only on 429 and 529. A 409 invalid_batch_state is resolved with Get: when
// the batch is no longer a draft (an earlier finalize went through) that
// batch is returned, else the 409 is.
func (b *BatchService) Finalize(ctx context.Context, batchID string) (*Batch, error) {
	batch, _, err := b.finalize(ctx, batchID)
	return batch, err
}

func (b *BatchService) finalize(ctx context.Context, batchID string) (*Batch, http.Header, error) {
	if b.c.err != nil {
		return nil, nil, b.c.err
	}
	resp, err := b.c.call(ctx, http.MethodPost, "/v1/batches/"+url.PathEscape(batchID)+"/finalize", nil, callOptions{policy: retryMutation})
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusConflict && e.ErrorType == "invalid_batch_state" {
			if batch, header, gerr := b.get(ctx, batchID); gerr == nil && batch.State != "draft" {
				return batch, header, nil
			}
		}
		return nil, nil, err
	}
	batch, err := parseBatchResponse(resp)
	return batch, resp.header, err
}

// Get reads a batch with GET /v1/batches/{id}.
func (b *BatchService) Get(ctx context.Context, batchID string) (*Batch, error) {
	batch, _, err := b.get(ctx, batchID)
	return batch, err
}

func (b *BatchService) get(ctx context.Context, batchID string) (*Batch, http.Header, error) {
	if b.c.err != nil {
		return nil, nil, b.c.err
	}
	resp, err := b.c.doPayload(ctx, http.MethodGet, "/v1/batches/"+url.PathEscape(batchID), nil)
	if err != nil {
		return nil, nil, err
	}
	batch, err := parseBatchResponse(resp)
	return batch, resp.header, err
}

// List reads one page of the key's batches, newest first, with GET
// /v1/batches.
func (b *BatchService) List(ctx context.Context, opts BatchListOptions) (*BatchPage, error) {
	if b.c.err != nil {
		return nil, b.c.err
	}
	q := url.Values{}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	path := "/v1/batches"
	if len(q) > 0 {
		// offset first, as the API documents it.
		path += "?" + encodeOrdered(q, "offset", "limit")
	}
	resp, err := b.c.doPayload(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	var shape struct {
		Batches    []json.RawMessage `json:"batches"`
		Offset     int               `json:"offset"`
		Limit      int               `json:"limit"`
		Total      int               `json:"total"`
		NextOffset *int              `json:"next_offset"`
	}
	_ = json.Unmarshal(resp.data, &shape)
	page := &BatchPage{Offset: shape.Offset, Limit: shape.Limit, Total: shape.Total, NextOffset: shape.NextOffset, Raw: raw}
	for _, data := range shape.Batches {
		batch, err := parseBatch(data)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		page.Batches = append(page.Batches, *batch)
	}
	return page, nil
}

// Results reads an ended batch's results with GET /v1/batches/{id}/results,
// as JSON Lines, one BatchResult per line in upload order. Before the batch
// has ended the API answers 409 results_not_ready; 7 days after, 410
// results_expired (a bare *Error with that status).
func (b *BatchService) Results(ctx context.Context, batchID string) ([]BatchResult, error) {
	if b.c.err != nil {
		return nil, b.c.err
	}
	resp, err := b.c.doPayload(ctx, http.MethodGet, "/v1/batches/"+url.PathEscape(batchID)+"/results", nil)
	if err != nil {
		return nil, err
	}
	var out []BatchResult
	for _, line := range bytes.Split(resp.data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		r, err := parseBatchResult(line)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Cancel cancels a batch with POST /v1/batches/{id}/cancel. A draft, or a
// batch with nothing running, ends cancelled at once; one with lines running
// winds down and ends cancelled when they finish. Safe to repeat.
func (b *BatchService) Cancel(ctx context.Context, batchID string) (*Batch, error) {
	if b.c.err != nil {
		return nil, b.c.err
	}
	resp, err := b.c.doPayload(ctx, http.MethodPost, "/v1/batches/"+url.PathEscape(batchID)+"/cancel", nil)
	if err != nil {
		return nil, err
	}
	return parseBatchResponse(resp)
}

func (b *BatchService) mutate(ctx context.Context, path string, payload []byte, contentType string) (*Batch, error) {
	resp, err := b.c.call(ctx, http.MethodPost, path, payload, callOptions{policy: retryMutation, contentType: contentType})
	if err != nil {
		return nil, err
	}
	return parseBatchResponse(resp)
}

// RunBatchOptions are the settings of RunBatch.
type RunBatchOptions struct {
	// PollInterval is the fixed wait between reads of the batch: at least
	// MinPollInterval (10 s); 0 means DefaultPollInterval (60 s). When the
	// server's Retry-After is longer, it is waited instead.
	PollInterval time.Duration
	// Timeout bounds the waiting; 0 means DefaultBatchTimeout (26 hours).
	// Past it RunBatch returns a *TimeoutError.
	Timeout time.Duration
}

// RunBatch runs requests through the whole batch lifecycle: it checks and
// encodes every request, creates an empty draft, adds the lines in calls of
// at most 10 MiB, finalizes, reads the batch every PollInterval until it
// ends, and returns the ended batch and its results. If an add fails, the
// draft is cancelled and the add's error returned. A PollInterval below
// 10 s is an argument error, before anything is sent.
func (c *Client) RunBatch(ctx context.Context, reqs []BatchRequest, opts RunBatchOptions) (*Batch, []BatchResult, error) {
	poll := opts.PollInterval
	if poll == 0 {
		poll = DefaultPollInterval
	}
	if poll < MinPollInterval {
		return nil, nil, fmt.Errorf("decisionnode: the poll interval must be at least %s, got %s", MinPollInterval, opts.PollInterval)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultBatchTimeout
	}
	if timeout < 0 {
		return nil, nil, fmt.Errorf("decisionnode: the timeout must be above 0, got %s", opts.Timeout)
	}
	if c.err != nil {
		return nil, nil, c.err
	}
	b := c.Batches()
	lines, err := c.batchLines(ctx, reqs)
	if err != nil {
		return nil, nil, err
	}
	draft, err := b.mutate(ctx, "/v1/batches", []byte{}, ndjson)
	if err != nil {
		return nil, nil, err
	}
	if len(lines) > 0 {
		if _, err := b.addLines(ctx, draft.ID, lines); err != nil {
			_, _ = b.Cancel(ctx, draft.ID)
			return nil, nil, err
		}
	}
	batch, header, err := b.finalize(ctx, draft.ID)
	if err != nil {
		return nil, nil, err
	}
	var waited time.Duration
	for !batch.Ended() {
		if waited >= timeout {
			return nil, nil, &TimeoutError{&Error{msg: fmt.Sprintf("the batch %s did not end within %s", batch.ID, timeout), Err: context.DeadlineExceeded}}
		}
		wait := poll
		if d, ok := retryAfter(header); ok && d > wait {
			wait = d
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, nil, &Error{msg: "the request was canceled", Err: err}
		}
		waited += wait
		if batch, header, err = b.get(ctx, batch.ID); err != nil {
			return nil, nil, err
		}
	}
	results, err := b.Results(ctx, batch.ID)
	if err != nil {
		return nil, nil, err
	}
	return batch, results, nil
}

// batchLines checks every request, then places each line's images (inline
// or uploaded, per line) and encodes the lines, each ending in a newline.
// Nothing is sent when a request is invalid.
func (c *Client) batchLines(ctx context.Context, reqs []BatchRequest) ([][]byte, error) {
	preps := make([]*prepared, len(reqs))
	withImages := false
	for i := range reqs {
		id := reqs[i].CustomID
		p, err := c.prepare(reqs[i].Request, false, &id)
		if err != nil {
			var apiErr *Error
			if errors.As(err, &apiErr) {
				return nil, err
			}
			return nil, fmt.Errorf("decisionnode: batch request %d: %w", i, err)
		}
		preps[i] = p
		withImages = withImages || len(p.images) > 0
	}
	lines := make([][]byte, len(preps))
	encode := func(i int) error {
		pl, err := c.placeImages(ctx, preps[i].images, true)
		if err != nil {
			return err
		}
		lines[i] = append(preps[i].body(pl.wires), '\n')
		return nil
	}
	if !withImages {
		for i := range preps {
			if err := encode(i); err != nil {
				return nil, err
			}
		}
		return lines, nil
	}
	// Lines with images may upload: place them a few at a time.
	errs := make([]error, len(preps))
	sem := make(chan struct{}, DefaultConcurrency)
	var wg sync.WaitGroup
	for i := range preps {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = encode(i)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func parseBatchResponse(resp *response) (*Batch, error) {
	batch, err := parseBatch(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	return batch, nil
}

func parseBatch(data []byte) (*Batch, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		ID              string        `json:"id"`
		State           string        `json:"state"`
		Counters        BatchCounters `json:"counters"`
		CreatedAt       *string       `json:"created_at"`
		FinalizedAt     *string       `json:"finalized_at"`
		ExpiresAt       *string       `json:"expires_at"`
		EndedAt         *string       `json:"ended_at"`
		ResultsURL      *string       `json:"results_url"`
		ResultsExpireAt *string       `json:"results_expire_at"`
		PriceFactor     float64       `json:"price_factor"`
		Usage           Usage         `json:"usage"`
		Error           *BatchError   `json:"error"`
	}
	// A field of an unexpected type is left empty; Raw holds it as sent.
	_ = json.Unmarshal(data, &shape)
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return &Batch{
		ID: shape.ID, State: shape.State, Counters: shape.Counters, CreatedAt: str(shape.CreatedAt),
		FinalizedAt: str(shape.FinalizedAt), ExpiresAt: str(shape.ExpiresAt), EndedAt: str(shape.EndedAt),
		ResultsURL: str(shape.ResultsURL), ResultsExpireAt: str(shape.ResultsExpireAt), PriceFactor: shape.PriceFactor,
		Usage: shape.Usage, Error: shape.Error, Raw: raw,
	}, nil
}

func parseBatchResult(line []byte) (BatchResult, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return BatchResult{}, err
	}
	var shape struct {
		CustomID string            `json:"custom_id"`
		Status   json.RawMessage   `json:"status"`
		Body     json.RawMessage   `json:"body"`
		Headers  map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(line, &shape); err != nil {
		return BatchResult{}, err
	}
	r := BatchResult{CustomID: shape.CustomID, Headers: map[string]string{}, Raw: raw}
	header := http.Header{}
	for k, v := range shape.Headers {
		r.Headers[strings.ToLower(k)] = v
		header.Set(k, v)
	}
	if json.Unmarshal(shape.Status, &r.StatusText) == nil {
		return r, nil
	}
	if err := json.Unmarshal(shape.Status, &r.Status); err != nil {
		return BatchResult{}, fmt.Errorf("line %q: status %s", shape.CustomID, shape.Status)
	}
	if r.Status == http.StatusOK && bytes.HasPrefix(bytes.TrimSpace(shape.Body), []byte("{")) {
		d, err := parseDecision(shape.Body, header)
		if err != nil {
			return BatchResult{}, err
		}
		r.Decision = d
		return r, nil
	}
	r.Err = newError(r.Status, header, shape.Body)
	return r, nil
}

// encodeOrdered writes query parameters in the order given.
func encodeOrdered(q url.Values, keys ...string) string {
	var buf bytes.Buffer
	for _, k := range keys {
		if v, ok := q[k]; ok {
			if buf.Len() > 0 {
				buf.WriteByte('&')
			}
			buf.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(v[0]))
		}
	}
	return buf.String()
}
