package decisionnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the hosted API.
	DefaultBaseURL = "https://api.decisionnode.com"
	// DefaultModel is the model used when none is set.
	DefaultModel = "decisionnode-latest"
	// DefaultTimeout is the time allowed for each attempt, from sending the
	// request to reading the last byte of the response body.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxRetries is the number of retries after the first attempt.
	DefaultMaxRetries = 2
	// DefaultConcurrency is the number of requests DecideMany runs at a time.
	DefaultConcurrency = 8
)

// Client calls the DecisionNode API. It is safe for concurrent use and reuses
// its connections; create one and share it.
type Client struct {
	apiKey     string
	baseURL    string
	model      string
	timeout    time.Duration
	maxRetries int
	headers    http.Header
	httpClient *http.Client
	logger     *slog.Logger
	onResponse func(ctx context.Context, r ResponseInfo)
	// sleep waits between attempts. Tests replace it to record the waits.
	sleep func(ctx context.Context, d time.Duration) error
	// err is the first configuration problem found by NewClient. Every call
	// returns it before anything is sent.
	err error
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key. Without it, or when it is empty, the client
// reads DECISIONNODE_API_KEY.
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithBaseURL sets the API's base URL. Without it, or when it is empty, the
// client reads DECISIONNODE_BASE_URL, else uses DefaultBaseURL. The URL must
// be https; http is accepted only for localhost, 127.0.0.1 and ::1, so the API
// key never travels unencrypted. Any other URL is an argument error, returned
// by Err and by every call.
func WithBaseURL(url string) Option { return func(c *Client) { c.baseURL = url } }

// WithModel sets the default model. Without it, or when it is empty, the
// client reads DECISIONNODE_MODEL, else uses DefaultModel.
func WithModel(model string) Option { return func(c *Client) { c.model = model } }

// WithTimeout sets the time allowed for each attempt (default 10 s), from
// sending the request to reading the last byte of the response body. It must
// be above 0; anything else is an argument error, returned by Err and by
// every call.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d <= 0 {
			c.fail(fmt.Errorf("decisionnode: the timeout must be above 0, got %s", d))
			return
		}
		c.timeout = d
	}
}

// WithMaxRetries sets the number of retries after the first attempt (default
// 2). It must be 0 or more; anything else is an argument error, returned by
// Err and by every call.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			c.fail(fmt.Errorf("decisionnode: max retries must be 0 or more, got %d", n))
			return
		}
		c.maxRetries = n
	}
}

// WithHeaders adds headers to every request, for example
// "x-decisionnode-safety-body: true".
func WithHeaders(h map[string]string) Option {
	return func(c *Client) {
		for k, v := range h {
			c.headers.Set(k, v)
		}
	}
}

// WithHTTPClient sets the HTTP client used to send requests. The client is
// copied and its copy never follows redirects, whatever its CheckRedirect.
// The client's own Timeout, if set, still applies on top of the client's.
//
// Its Transport must be nil (http.DefaultTransport) or an *http.Transport,
// so the client can be shown never to follow a redirect and never to skip
// TLS checks. A transport of any other type, an *http.Transport with
// TLSClientConfig.InsecureSkipVerify set, or one with its own DialTLS or
// DialTLSContext is refused for an https base URL: that is an argument error,
// returned by Err and by every call.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithLogger switches on debug logging: the method, path, status, request id
// and timing of every attempt, at debug level. State, questions and keys are
// never logged.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.logger = l } }

// ResponseInfo describes one HTTP response the client received, as passed to
// the function given to WithResponseHook.
type ResponseInfo struct {
	// Method and Path are the request's, such as "POST" and "/v1/decide".
	Method string
	Path   string
	// Attempt is 1 for the first attempt, 2 for the first retry, and so on.
	Attempt int
	// Status is the HTTP status code.
	Status int
	// Header is a copy of the response headers.
	Header http.Header
	// Body is a copy of the response body, exactly as received.
	Body []byte
}

// WithResponseHook calls f once for every response the client receives,
// including error responses and those that are retried, after the whole body
// has been read and before it is parsed. ctx is the context of that attempt,
// so it carries the caller's values. f gets copies of the headers and body;
// nothing it does changes what the client returns. It is not called for an
// attempt that got no response (a timeout or connection error). f may be
// called from several goroutines at once, as DecideMany sends concurrently.
// State, questions and keys are never passed to it.
func WithResponseHook(f func(ctx context.Context, r ResponseInfo)) Option {
	return func(c *Client) { c.onResponse = f }
}

// NewClient returns a client. Settings not given as options, or given as
// empty strings, are read from DECISIONNODE_API_KEY, DECISIONNODE_BASE_URL
// and DECISIONNODE_MODEL, then fall back to the defaults. A missing API key is
// reported by the first call, before anything is sent. An invalid setting (a
// base URL that is not https, a timeout not above 0, negative retries) is an
// argument error: Err returns it, and so does every call, before anything is
// sent.
func NewClient(opts ...Option) *Client {
	c := &Client{
		timeout:    DefaultTimeout,
		maxRetries: DefaultMaxRetries,
		headers:    http.Header{},
		sleep:      sleep,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.apiKey == "" {
		c.apiKey = os.Getenv("DECISIONNODE_API_KEY")
	}
	if c.baseURL == "" {
		c.baseURL = os.Getenv("DECISIONNODE_BASE_URL")
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	if err := checkBaseURL(c.baseURL); err != nil {
		c.fail(err)
	}
	if c.model == "" {
		c.model = os.Getenv("DECISIONNODE_MODEL")
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	var hc http.Client
	if c.httpClient != nil {
		hc = *c.httpClient
		if err := checkTransport(hc.Transport, c.baseURL); err != nil {
			c.fail(err)
		}
	} else {
		t := defaultTransport()
		if err := checkTransport(t, c.baseURL); err != nil {
			c.fail(err)
		}
		hc.Transport = t
	}
	// Never follow a redirect: the API key must not go to another URL or over
	// plain HTTP. A 3xx is returned to the caller as an error.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.httpClient = &hc
	return c
}

// Err returns the client's configuration problem, or nil when its settings
// are valid. It is a plain error, not an *Error: nothing was sent.
func (c *Client) Err() error { return c.err }

// fail records the first configuration problem.
func (c *Client) fail(err error) {
	if c.err == nil {
		c.err = err
	}
}

// checkBaseURL accepts an https URL, or an http URL on the local machine.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("decisionnode: the base URL %q is not a valid URL: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("decisionnode: the base URL %q has no host", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		switch strings.ToLower(u.Hostname()) {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return fmt.Errorf("decisionnode: the base URL %q uses plain http; use https (http is accepted only for localhost, 127.0.0.1 and ::1)", raw)
	}
	return fmt.Errorf("decisionnode: the base URL %q must use https", raw)
}

// defaultTransport is the transport of a client without one of its own: a
// copy of http.DefaultTransport when that is an *http.Transport, with enough
// idle connections for DecideMany's default concurrency.
func defaultTransport() *http.Transport {
	var t *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t = dt.Clone()
	} else {
		t = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	t.MaxIdleConnsPerHost = 2 * DefaultConcurrency
	return t
}

// checkTransport accepts a transport that can be shown to keep TLS checks on
// for an https base URL. Redirects are the http.Client's job, and the copy the
// client sends with never follows one. A transport of a type this package
// cannot inspect may do anything, so it is refused, as are TLS settings that
// skip certificate or hostname checks and a TLS dialer of the caller's own.
func checkTransport(rt http.RoundTripper, baseURL string) error {
	if rt == nil {
		rt = http.DefaultTransport
	}
	t, ok := rt.(*http.Transport)
	if !ok {
		return fmt.Errorf("decisionnode: the HTTP client's transport is a %T; only an *http.Transport (or nil) can be shown never to follow redirects or skip TLS checks", rt)
	}
	u, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	if t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
		return errors.New("decisionnode: the HTTP client's transport turns off certificate checks (InsecureSkipVerify); this is refused for an https base URL")
	}
	if t.DialTLS != nil || t.DialTLSContext != nil { //nolint:staticcheck // DialTLS is deprecated but still honoured by net/http.
		return errors.New("decisionnode: the HTTP client's transport has its own TLS dialer, so its certificate checks cannot be shown; this is refused for an https base URL")
	}
	return nil
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Decide sends one request and returns its decision.
func (c *Client) Decide(ctx context.Context, req Request) (*Decision, error) {
	d, _, err := c.decide(ctx, req)
	return d, err
}

// decide sends one request and returns its decision and the response it was
// read from.
func (c *Client) decide(ctx context.Context, req Request) (*Decision, *response, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	payload, err := c.prepare(req)
	if err != nil {
		return nil, nil, err
	}
	return c.send(ctx, payload)
}

// prepare checks a request and encodes it, without sending anything. Every
// problem it finds is an argument error.
func (c *Client) prepare(req Request) ([]byte, error) {
	body, err := buildBody(req, c.model)
	if err != nil {
		return nil, err
	}
	payload, err := marshal(body)
	if err != nil {
		return nil, fmt.Errorf("decisionnode: the request cannot be encoded as JSON: %w", err)
	}
	return payload, nil
}

// send posts an encoded decide request and parses the decision.
func (c *Client) send(ctx context.Context, payload []byte) (*Decision, *response, error) {
	resp, err := c.doPayload(ctx, http.MethodPost, "/v1/decide", payload)
	if err != nil {
		return nil, nil, err
	}
	d, err := parseDecision(resp.data, resp.header)
	if err != nil {
		return nil, resp, invalidResponse(resp, err)
	}
	return d, resp, nil
}

// DecisionResult is the outcome of one request of DecideMany: a decision or
// the error that request ended with.
type DecisionResult struct {
	Decision *Decision
	Err      error
}

// DecideMany sends each request with Decide, at most concurrency at a time,
// and returns one result per request in input order. One failure does not
// stop the others. Pass DefaultConcurrency (8) for the default.
//
// Every request is checked before any is sent: a concurrency below 1 or an
// invalid request (an empty question list, image bytes of an unknown format,
// a value that cannot be encoded) is an argument error, returned with no
// results and nothing sent.
func (c *Client) DecideMany(ctx context.Context, reqs []Request, concurrency int) ([]DecisionResult, error) {
	if concurrency < 1 {
		return nil, fmt.Errorf("decisionnode: concurrency must be at least 1, got %d", concurrency)
	}
	if c.err != nil {
		return nil, c.err
	}
	payloads := make([][]byte, len(reqs))
	for i, req := range reqs {
		p, err := c.prepare(req)
		if err != nil {
			return nil, fmt.Errorf("decisionnode: request %d: %w", i, err)
		}
		payloads[i] = p
	}
	results := make([]DecisionResult, len(reqs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			d, _, err := c.send(ctx, payloads[i])
			results[i] = DecisionResult{Decision: d, Err: err}
		}(i)
	}
	wg.Wait()
	return results, nil
}

// CheckRequest is the input of Check.
type CheckRequest struct {
	State        any
	Instructions any
	// IfTrue and IfFalse describe what a yes and a no mean. Either may be nil.
	IfTrue  any
	IfFalse any
	Images  []Image
	Model   string
}

// Check asks one truth question.
func (c *Client) Check(ctx context.Context, r CheckRequest) (*TruthAnswer, error) {
	d, resp, err := c.decide(ctx, Request{
		State:     r.State,
		Questions: Questions{Q("check", TruthWith(r.Instructions, r.IfTrue, r.IfFalse))},
		Images:    r.Images,
		Model:     r.Model,
	})
	if err != nil {
		return nil, err
	}
	if a := d.Truth("check"); a != nil {
		return a, nil
	}
	return nil, malformedAnswer(resp, d, "check", "truth")
}

// ClassifyRequest is the input of Classify.
type ClassifyRequest struct {
	State        any
	Instructions any
	// Options is Options, a map of name to description, or a slice of names.
	Options any
	Images  []Image
	Model   string
}

// Classify asks one choice question.
func (c *Client) Classify(ctx context.Context, r ClassifyRequest) (*ChoiceAnswer, error) {
	d, resp, err := c.decide(ctx, Request{
		State:     r.State,
		Questions: Questions{Q("classify", Choice(r.Instructions, r.Options))},
		Images:    r.Images,
		Model:     r.Model,
	})
	if err != nil {
		return nil, err
	}
	if a := d.Choice("classify"); a != nil {
		return a, nil
	}
	return nil, malformedAnswer(resp, d, "classify", "choice")
}

// ScoreRequest is the input of Score.
type ScoreRequest struct {
	State        any
	Instructions any
	// Levels is a slice of levels, low to high.
	Levels any
	Images []Image
	Model  string
}

// Score asks one score question.
func (c *Client) Score(ctx context.Context, r ScoreRequest) (*ScoreAnswer, error) {
	d, resp, err := c.decide(ctx, Request{
		State:     r.State,
		Questions: Questions{Q("score", Score(r.Instructions, r.Levels))},
		Images:    r.Images,
		Model:     r.Model,
	})
	if err != nil {
		return nil, err
	}
	if a := d.Score("score"); a != nil {
		return a, nil
	}
	return nil, malformedAnswer(resp, d, "score", "score")
}

// GateRequest is the input of Gate.
type GateRequest struct {
	// Action is the action to judge, such as a command or a description of a change.
	Action any
	// Context is anything that helps judge it. Nil leaves it out.
	Context any
	Images  []Image
	Model   string
}

// Gate judges an action before it runs with three questions: "risk" (a
// score from none to severe), "touches_production" (a truth) and "verdict"
// (a choice of allow, confirm or block).
func (c *Client) Gate(ctx context.Context, r GateRequest) (*Decision, error) {
	state := gateState{Action: r.Action, Context: r.Context}
	return c.Decide(ctx, Request{State: state, Questions: GateQuestions(), Images: r.Images, Model: r.Model})
}

type gateState struct {
	Action  any `json:"action"`
	Context any `json:"context,omitempty"`
}

// GateQuestions returns the three questions Gate sends.
func GateQuestions() Questions {
	return Questions{
		Q("risk", Score(
			"How much harm could this action cause if it is wrong?",
			[]string{"none", "low", "moderate", "high", "severe"},
		)),
		Q("touches_production", Truth(
			"Does this action change a production system, production data or anything customers use?",
		)),
		Q("verdict", Choice(
			"Should this action run as it is, wait for a person to confirm it, or be blocked?",
			Options{
				{Name: "allow", Description: "safe to run without asking"},
				{Name: "confirm", Description: "a person should confirm it first"},
				{Name: "block", Description: "should not run"},
			},
		)),
	}
}

// Models lists the models the key may use, with their published limits.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	models, err := parseModels(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	return models, nil
}

// DecideRaw sends body to POST /v1/decide exactly as given and returns the
// parsed response body. Use it for API fields this version of the package
// does not know yet. body is any value that encodes to JSON, such as a
// map[string]any or a json.RawMessage.
func (c *Client) DecideRaw(ctx context.Context, body any) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/decide", body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp.data, &out); err != nil {
		return nil, invalidResponse(resp, err)
	}
	return out, nil
}

// malformedAnswer is the error of Check, Classify and Score when their own
// answer is missing or is not the typed answer they promise: a ServerError
// carrying the response's status, request id and the server's reason.
func malformedAnswer(resp *response, d *Decision, id, typ string) error {
	return &ServerError{&Error{
		Status:    resp.status,
		RequestID: d.RequestID,
		Reason:    resp.header.Get("x-decisionnode-error"),
		Header:    resp.header,
		Body:      d.Raw,
		msg:       "the response has no valid " + typ + " answer for question " + `"` + id + `"`,
	}}
}

// invalidResponse is the error for a 2xx response whose body is not the JSON
// expected: a ServerError carrying the response's status, request id and the
// server's reason.
func invalidResponse(resp *response, err error) error {
	return &ServerError{&Error{
		Status:    resp.status,
		RequestID: resp.header.Get("x-request-id"),
		Reason:    resp.header.Get("x-decisionnode-error"),
		Header:    resp.header,
		msg:       "the response body is not valid JSON",
		Err:       err,
	}}
}

// response is a successful (2xx) response.
type response struct {
	status int
	header http.Header
	data   []byte
}

// do sends one request with retries and returns the successful response.
func (c *Client) do(ctx context.Context, method, path string, body any) (*response, error) {
	if c.err != nil {
		return nil, c.err
	}
	var payload []byte
	if body != nil {
		b, err := marshal(body)
		if err != nil {
			return nil, fmt.Errorf("decisionnode: the request cannot be encoded as JSON: %w", err)
		}
		payload = b
	}
	return c.doPayload(ctx, method, path, payload)
}

// doPayload sends an encoded body (nil for none) with retries and returns the
// successful response.
func (c *Client) doPayload(ctx context.Context, method, path string, payload []byte) (*response, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.apiKey == "" {
		return nil, &AuthenticationError{&Error{msg: "no API key: pass WithAPIKey or set DECISIONNODE_API_KEY"}}
	}
	for attempt := 0; ; attempt++ {
		resp, header, err := c.attempt(ctx, method, path, payload, attempt)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil || attempt >= c.maxRetries || !retryable(err) {
			return nil, err
		}
		wait := backoff(attempt)
		if header != nil {
			if d, ok := retryAfter(header); ok {
				wait = d
			}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, &Error{msg: "the request was canceled", Err: err}
		}
	}
}

func (c *Client) attempt(ctx context.Context, method, path string, payload []byte, attempt int) (*response, http.Header, error) {
	// One deadline for the whole attempt: connecting, sending, the response
	// headers and every byte of the body.
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(actx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, &Error{msg: "invalid request: " + err.Error(), Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range c.headers {
		req.Header[k] = append([]string(nil), vs...)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	var data []byte
	if err == nil {
		data, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)
	if err != nil {
		c.log(ctx, method, path, 0, "", elapsed, attempt, err)
		return nil, nil, c.transportError(ctx, actx, err)
	}
	c.log(ctx, method, path, resp.StatusCode, resp.Header.Get("x-request-id"), elapsed, attempt, nil)
	if c.onResponse != nil {
		c.onResponse(actx, ResponseInfo{
			Method:  method,
			Path:    path,
			Attempt: attempt + 1,
			Status:  resp.StatusCode,
			Header:  resp.Header.Clone(),
			Body:    bytes.Clone(data),
		})
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &response{status: resp.StatusCode, header: resp.Header, data: data}, resp.Header, nil
	}
	return nil, resp.Header, newError(resp.StatusCode, resp.Header, data)
}

func (c *Client) transportError(ctx, actx context.Context, err error) error {
	if ctx.Err() != nil {
		return &Error{msg: "the request was canceled", Err: ctx.Err()}
	}
	if errors.Is(actx.Err(), context.DeadlineExceeded) || isTimeout(err) {
		return &TimeoutError{&Error{msg: "the request timed out after " + c.timeout.String(), Err: err}}
	}
	return &ConnectionError{&Error{msg: "connection error: " + err.Error(), Err: err}}
}

func isTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func (c *Client) log(ctx context.Context, method, path string, status int, requestID string, elapsed time.Duration, attempt int, err error) {
	if c.logger == nil {
		return
	}
	attrs := []any{
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", status),
		slog.String("request_id", requestID),
		slog.Duration("elapsed", elapsed),
		slog.Int("attempt", attempt+1),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	c.logger.DebugContext(ctx, "decisionnode request", attrs...)
}

// retryable reports whether an error is worth another attempt: 408, 429,
// every 5xx, timeouts and connection errors.
func retryable(err error) bool {
	var te *TimeoutError
	var ce *ConnectionError
	if errors.As(err, &te) || errors.As(err, &ce) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status == 408 || e.Status == 429 || (e.Status >= 500 && e.Status <= 599)
	}
	return false
}

var (
	jitterMu sync.Mutex
	jitter   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// backoff is the wait before retry number attempt+1 when the server named
// none: exponential from 0.5 s, with jitter, capped at 8 s.
func backoff(attempt int) time.Duration {
	base := 0.5 * float64(int(1)<<min(attempt, 10))
	if base > 8 {
		base = 8
	}
	jitterMu.Lock()
	f := 0.75 + jitter.Float64()*0.25
	jitterMu.Unlock()
	return time.Duration(base * f * float64(time.Second))
}
