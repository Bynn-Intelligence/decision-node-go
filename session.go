package decisionnode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var errSessionWithoutClient = errors.New("decisionnode: the session was not opened by a client")

// maxSessionMessage bounds a message read from a session's socket.
const maxSessionMessage = 16 << 20

// reopenCodes are the close codes after which a supervisor opens a new
// session with the same body.
var reopenCodes = map[int]bool{1001: true, 1006: true, 1011: true, 4404: true, 4408: true, 4409: true, 4410: true}

// SessionRequest opens a session: the fixed part of a decision request, sent
// once, then answered frame by frame.
type SessionRequest struct {
	// State is the static context: a string or any JSON value. Nil leaves
	// it out.
	State any
	// Questions are asked of every frame unless a frame names a subset. At
	// least one.
	Questions Questions
	// Instructions say who the model decides as. Nil leaves them out.
	Instructions any
	// Model overrides the client's model. A model is always sent.
	Model string
	// Window is the number of recent frames each frame is answered with,
	// itself included (1 to 64). Nil leaves it to the server (8). Set it with
	// Ptr(16).
	Window *int
	// TTLSeconds is the session's lifetime from the open call (1 to 3,600).
	// Nil leaves it to the server (300).
	TTLSeconds *int
}

// SessionLimits are a session's own limits, from its open response.
type SessionLimits struct {
	MaxFrameTokens     int `json:"max_frame_tokens"`
	MaxFrameImages     int `json:"max_frame_images"`
	WindowTokens       int `json:"window_tokens"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
}

// Session is an open session: every field of the open response, and the
// socket that carries its frames. Connect it, Send frames, read events with
// Next or Replies, and End it. A Session is safe for concurrent use.
type Session struct {
	// ID is the session id, "ses_" and 24 hex digits.
	ID string
	// Model is the pinned model version that answers every frame.
	Model string
	// StreamURL is the WebSocket URL Connect opens.
	StreamURL  string
	Window     int
	TTLSeconds int
	// ExpiresAt is the end of the session's life, as sent (RFC 3339).
	ExpiresAt string
	Limits    SessionLimits
	// Questions are the session's question ids, in request order.
	Questions []string
	// PrefixTokens is the token count of the instructions and the state.
	PrefixTokens *int
	// Usage is what the open billed.
	Usage Usage
	// Answers holds every question answered on the fixed part alone.
	Answers map[string]Answer
	// Safety is the safety check of the fixed part, from the body.
	Safety *Safety
	// RequestID is the open response's x-request-id.
	RequestID string
	// Raw is the open response body.
	Raw map[string]any

	client *Client
	mu     sync.Mutex // guards conn, seq, events and closing
	conn   *websocket.Conn
	seq    int64
	events *eventQueue
	// closing is set by Close, so the close that follows is reported as the
	// client's own 1000.
	closing bool
}

// Frame is one message on a session's socket.
type Frame struct {
	// Frame is the observation: text or any JSON value. Beside an image it
	// is an optional note.
	Frame any
	// Image is one image, sent inline under the inline threshold and by
	// upload_id at or above it (or when this client uploaded the same bytes
	// before). A URL image is refused locally: frames take no URL.
	Image *Image
	// Questions asks only these of the session's questions. Nil asks all.
	Questions []string
	// Reset empties the window before this frame is answered.
	Reset bool
	// Seq numbers the frame. Nil numbers it from the session's counter: 1
	// for the first frame, then one more than the last frame sent.
	Seq *int64
}

// SessionEvent is one event of a session: a *Reply, a *FrameError, or a
// *SessionClosed, which is always the last.
type SessionEvent interface{ sessionEvent() }

// Reply answers one frame.
type Reply struct {
	// Seq is the seq of the frame answered.
	Seq int64
	// Answers holds one answer per question asked, typed as in a Decision.
	Answers map[string]Answer
	// Dropped lists the frames this frame superseded under latest wins,
	// as the server reported them. Nil when none were.
	Dropped []int64
	// Usage is what this frame billed.
	Usage Usage
	// LatencyMs is the server's time for this frame. Nil when not sent.
	LatencyMs *float64
	// Safety is the safety check of this frame, from the body.
	Safety *Safety
	// Raw is the message as parsed.
	Raw map[string]any
	// RawJSON is the message exactly as the server sent it.
	RawJSON json.RawMessage
}

// FrameError is a frame the server could not answer. It is a value, never
// returned as an error: the session goes on.
type FrameError struct {
	// Seq is the frame's seq, or nil when the message could not be read as a
	// frame.
	Seq *int64
	// ErrorType is api_usage_error, rate_limit_error, overloaded_error,
	// api_error, refusal_error or an upload error type.
	ErrorType string
	// Message is the server's message.
	Message string
	// Category is the refused category of a refusal_error.
	Category string
	// RetryAfterMs is the wait the server names for a rate_limit_error or an
	// overloaded_error. Nil when not sent.
	RetryAfterMs *int64
	// Usage is what a refused frame billed. Nil otherwise.
	Usage *Usage
	// Safety is the safety check of a refused frame, with Blocked set. Nil
	// otherwise.
	Safety *Safety
	// Raw is the message as parsed.
	Raw map[string]any
	// RawJSON is the message exactly as the server sent it.
	RawJSON json.RawMessage
}

// SessionClosed ends a session's events: the socket closed with Code.
type SessionClosed struct {
	// Code is the close code; 1006 when the connection was lost without a
	// close frame.
	Code int
	// Reason is the close reason, for logs only.
	Reason string
	// Reopen is true for the codes after which a new session with the same
	// body should be opened: 1001, 1006, 1011, 4404, 4408, 4409 and 4410.
	// It is false for 1013, after which the client connects again to the
	// same session, and for every code that should stop the client.
	Reopen bool
	// RawJSON is the close as one JSON object, {"code", "reason", "reopen"},
	// so every event prints the same way. A close frame itself carries no
	// JSON.
	RawJSON json.RawMessage
}

func newSessionClosed(code int, reason string) *SessionClosed {
	e := &SessionClosed{Code: code, Reason: reason, Reopen: reopenCodes[code]}
	e.RawJSON, _ = marshal(map[string]any{"code": code, "reason": reason, "reopen": e.Reopen})
	return e
}

func (*Reply) sessionEvent()         {}
func (*FrameError) sessionEvent()    {}
func (*SessionClosed) sessionEvent() {}

// Error lets a SessionClosed be returned as an error.
func (e *SessionClosed) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("decisionnode: the session closed with %d: %s", e.Code, e.Reason)
	}
	return fmt.Sprintf("decisionnode: the session closed with %d", e.Code)
}

// SessionSummary is the answer to ending a session.
type SessionSummary struct {
	SessionID      string
	Model          string
	Status         string
	ExpiresAt      string
	FramesAnswered int
	FramesDropped  int
	FramesCut      int
	// LastSeq is the seq of the last answered frame, nil when none was.
	LastSeq      *int64
	TokensBilled int64
	Raw          map[string]any
}

// OpenSession opens a session with POST /v1/sessions. A model is always
// sent. The questions are checked locally first, as for Decide. A 429
// session_limit_exceeded is never retried: it is a *SessionLimitError (also
// a *RateLimitError).
func (c *Client) OpenSession(ctx context.Context, r SessionRequest) (*Session, error) {
	if c.err != nil {
		return nil, c.err
	}
	payload, err := c.sessionBody(r)
	if err != nil {
		return nil, err
	}
	resp, err := c.doPayload(ctx, http.MethodPost, "/v1/sessions", payload)
	if err != nil {
		return nil, err
	}
	s, err := parseSession(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	s.client = c
	s.RequestID = resp.header.Get("x-request-id")
	return s, nil
}

// sessionBody checks an open request against the API's fixed rules and
// encodes it, without sending anything.
func (c *Client) sessionBody(r SessionRequest) ([]byte, error) {
	if len(r.Questions) == 0 {
		return nil, errors.New("decisionnode: questions must hold at least one question")
	}
	model := c.model
	if r.Model != "" {
		model = r.Model
	}
	lim := c.limitsFor(model)
	var qc questionCheck
	var buf bytes.Buffer
	buf.WriteByte('{')
	if err := writeMember(&buf, "model", model); err != nil {
		return nil, encodeError(err)
	}
	depth := 1
	for _, f := range []struct {
		name  string
		value any
	}{{"instructions", r.Instructions}, {"state", r.State}} {
		if f.value == nil {
			continue
		}
		v, err := marshal(f.value)
		if err != nil {
			return nil, encodeError(err)
		}
		if !isSDL(v) && !isNull(v) {
			qc.sdl([]any{"body", f.name}, v, f.name+" must be a string, an object or an array")
		}
		depth = max(depth, jsonDepth(v)+1)
		buf.WriteString(`,"` + f.name + `":`)
		buf.Write(v)
	}
	maxWindow, maxTTL := 64, 3600
	if lim.maxWindow > 0 {
		maxWindow = lim.maxWindow
	}
	if lim.maxTTLSeconds > 0 {
		maxTTL = lim.maxTTLSeconds
	}
	for _, f := range []struct {
		name  string
		value *int
		most  int
	}{{"window", r.Window, maxWindow}, {"ttl_seconds", r.TTLSeconds, maxTTL}} {
		switch {
		case f.value == nil:
		case *f.value < 1:
			qc.entry("greater_than_equal", []any{"body", f.name}, "Input should be greater than or equal to 1", *f.value,
				map[string]any{"ge": 1}, fmt.Sprintf("%s must be from 1 to %d, got %d", f.name, f.most, *f.value))
		case *f.value > f.most:
			qc.entry("less_than_equal", []any{"body", f.name}, fmt.Sprintf("Input should be less than or equal to %d", f.most), *f.value,
				map[string]any{"le": f.most}, fmt.Sprintf("%s must be from 1 to %d, got %d", f.name, f.most, *f.value))
		}
	}
	questions, err := encodeQuestions(r.Questions, lim, &qc)
	if err != nil {
		return nil, err
	}
	if err := checkNesting(max(depth, jsonDepth(questions)+1)); err != nil {
		return nil, err
	}
	if err := qc.err(); err != nil {
		return nil, err
	}
	if lim.sessionsHeld && !lim.sessionsEnabled {
		msg := fmt.Sprintf("Model %s does not serve sessions.", model)
		return nil, localBadRequest("api_usage_error", msg, msg)
	}
	buf.WriteString(`,"questions":`)
	buf.Write(questions)
	if r.Window != nil {
		fmt.Fprintf(&buf, `,"window":%d`, *r.Window)
	}
	if r.TTLSeconds != nil {
		fmt.Fprintf(&buf, `,"ttl_seconds":%d`, *r.TTLSeconds)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseSession(data []byte) (*Session, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		SessionID    string                     `json:"session_id"`
		Model        string                     `json:"model"`
		StreamURL    string                     `json:"stream_url"`
		Window       int                        `json:"window"`
		TTLSeconds   int                        `json:"ttl_seconds"`
		ExpiresAt    string                     `json:"expires_at"`
		Limits       SessionLimits              `json:"limits"`
		Questions    []string                   `json:"questions"`
		PrefixTokens *int                       `json:"prefix_tokens"`
		Usage        Usage                      `json:"usage"`
		Answers      map[string]json.RawMessage `json:"answers"`
	}
	// Fields of an unexpected type are left empty; Raw holds them as sent.
	_ = json.Unmarshal(data, &shape)
	s := &Session{
		ID: shape.SessionID, Model: shape.Model, StreamURL: shape.StreamURL, Window: shape.Window,
		TTLSeconds: shape.TTLSeconds, ExpiresAt: shape.ExpiresAt, Limits: shape.Limits, Questions: shape.Questions,
		PrefixTokens: shape.PrefixTokens, Usage: shape.Usage, Answers: parseAnswers(shape.Answers),
		Safety: parseSafetyBody(raw["safety"]), Raw: raw,
	}
	return s, nil
}

func parseAnswers(in map[string]json.RawMessage) map[string]Answer {
	out := make(map[string]Answer, len(in))
	for id, data := range in {
		out[id] = parseAnswer(data)
	}
	return out
}

// Connect opens the session's socket. The key goes in the handshake's
// Authorization header, never in the URL, with the client's own headers. A
// handshake the server refuses with an HTTP status is an *Error; a session
// the server refuses after the upgrade (4401, 4404, 4409, 1011, 1013) comes
// as a *SessionClosed event. Connecting again after a close (for example
// after 1013) opens a new socket to the same session.
func (s *Session) Connect(ctx context.Context) error {
	c := s.client
	if c == nil {
		return errSessionWithoutClient
	}
	if c.apiKey == "" {
		return &AuthenticationError{&Error{msg: "no API key: pass WithAPIKey or set DECISIONNODE_API_KEY"}}
	}
	if err := checkStreamURL(s.StreamURL); err != nil {
		return err
	}
	s.mu.Lock()
	if s.conn != nil && !s.events.isClosed() {
		s.mu.Unlock()
		return errors.New("decisionnode: the session is already connected")
	}
	s.mu.Unlock()

	header := http.Header{}
	for k, vs := range c.headers {
		header[k] = append([]string(nil), vs...)
	}
	header.Set("Authorization", "Bearer "+c.apiKey)
	header.Set("User-Agent", UserAgent)
	dialer := c.wsDialer()
	start := time.Now()
	conn, resp, err := dialer.DialContext(ctx, s.StreamURL, header)
	path := streamPath(s.StreamURL)
	if err != nil {
		if resp != nil {
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
			}
			c.log(ctx, http.MethodGet, path, resp.StatusCode, resp.Header.Get("x-request-id"), time.Since(start), 0, nil)
			return newError(resp.StatusCode, resp.Header, body)
		}
		c.log(ctx, http.MethodGet, path, 0, "", time.Since(start), 0, err)
		if ctx.Err() != nil {
			return &Error{msg: "the request was canceled", Err: ctx.Err()}
		}
		if isTimeout(err) {
			return &TimeoutError{&Error{msg: "the connection timed out after " + c.timeout.String(), Err: err}}
		}
		return &ConnectionError{&Error{msg: "connection error: " + err.Error(), Err: err}}
	}
	c.log(ctx, http.MethodGet, path, http.StatusSwitchingProtocols, resp.Header.Get("x-request-id"), time.Since(start), 0, nil)
	conn.SetReadLimit(maxSessionMessage)
	q := newEventQueue()
	s.mu.Lock()
	s.conn, s.events, s.closing = conn, q, false
	s.mu.Unlock()
	go s.read(conn, q)
	return nil
}

// wsDialer dials a session's socket the way the client's HTTP transport
// dials: its proxy, its dialer and its TLS settings (certificates, roots,
// session cache). The TLS offer is HTTP/1.1 only, since a WebSocket upgrade
// is an HTTP/1.1 request and the transport's own offer may include h2.
func (c *Client) wsDialer() *websocket.Dialer {
	d := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: c.timeout,
		ReadBufferSize:   16 << 10,
		WriteBufferSize:  16 << 10,
	}
	rt := c.httpClient.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	t, ok := rt.(*http.Transport)
	if !ok {
		return d
	}
	d.Proxy = t.Proxy
	d.NetDialContext = t.DialContext
	if t.TLSClientConfig != nil {
		cfg := t.TLSClientConfig.Clone()
		cfg.NextProtos = []string{"http/1.1"}
		d.TLSClientConfig = cfg
	}
	return d
}

// read turns the socket's messages into events until it closes. Pings are
// answered by the WebSocket layer while it reads.
func (s *Session) read(conn *websocket.Conn, q *eventQueue) {
	defer func() { _ = conn.Close() }()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			code, reason := websocket.CloseAbnormalClosure, ""
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				code, reason = ce.Code, ce.Text
			}
			s.mu.Lock()
			if s.closing && code == websocket.CloseAbnormalClosure {
				code = websocket.CloseNormalClosure
			}
			s.mu.Unlock()
			q.close(newSessionClosed(code, reason))
			return
		}
		if ev := parseEvent(msg); ev != nil {
			q.push(ev)
		}
	}
}

// parseEvent reads one server message: a reply, or a frame error when it
// carries "error". A message that is not a JSON object is skipped.
func parseEvent(msg []byte) SessionEvent {
	var raw map[string]any
	if json.Unmarshal(msg, &raw) != nil {
		return nil
	}
	if _, isErr := raw["error"]; isErr {
		var shape struct {
			Seq   *int64 `json:"seq"`
			Error struct {
				ErrorType string `json:"error_type"`
				Message   string `json:"message"`
				Category  string `json:"category"`
			} `json:"error"`
			RetryAfterMs *int64 `json:"retry_after_ms"`
			Usage        *Usage `json:"usage"`
		}
		_ = json.Unmarshal(msg, &shape)
		return &FrameError{
			Seq: shape.Seq, ErrorType: shape.Error.ErrorType, Message: shape.Error.Message, Category: shape.Error.Category,
			RetryAfterMs: shape.RetryAfterMs, Usage: shape.Usage, Safety: parseSafetyBody(raw["safety"]), Raw: raw,
			RawJSON: json.RawMessage(bytes.Clone(msg)),
		}
	}
	var shape struct {
		Seq       int64                      `json:"seq"`
		Answers   map[string]json.RawMessage `json:"answers"`
		Dropped   []int64                    `json:"dropped"`
		Usage     Usage                      `json:"usage"`
		LatencyMs *float64                   `json:"latency_ms"`
	}
	_ = json.Unmarshal(msg, &shape)
	return &Reply{
		Seq: shape.Seq, Answers: parseAnswers(shape.Answers), Dropped: shape.Dropped, Usage: shape.Usage,
		LatencyMs: shape.LatencyMs, Safety: parseSafetyBody(raw["safety"]), Raw: raw,
		RawJSON: json.RawMessage(bytes.Clone(msg)),
	}
}

// Send sends one frame and returns its seq. It never waits for the reply:
// read it with Next or Replies.
//
// The frame is checked first against the rules the server checks a frame by:
// seq from 0 to 2^53-1, a frame value or an image, a frame that is a
// non-empty string, an object or an array nested at most 128 deep, questions
// that are a non-empty list of the session's ids, and an image by bytes or
// upload (never a URL) within the image rules. A fault returns a
// *BadRequestError with error_type api_usage_error, the message the server's
// frame error would carry, and nothing is written: the refused frame takes no
// seq. The frame's image is sent inline or uploaded first, as the inline
// threshold says.
func (s *Session) Send(ctx context.Context, f Frame) (int64, error) {
	if s.client == nil {
		return 0, errSessionWithoutClient
	}
	frame, questions, err := s.checkFrame(f)
	if err != nil {
		return 0, err
	}
	var image []byte
	if f.Image != nil {
		pl, err := s.client.placeImages(ctx, []Image{*f.Image}, false)
		if err != nil {
			return 0, err
		}
		image = pl.wires[0]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.events.isClosed() {
		return 0, errors.New("decisionnode: the session is not connected")
	}
	seq := s.seq + 1
	if f.Seq != nil {
		seq = *f.Seq
	}
	s.seq = seq
	msg := make([]byte, 0, 64+len(frame)+len(image)+len(questions))
	msg = append(msg, `{"seq":`...)
	msg = append(msg, fmt.Sprint(seq)...)
	if frame != nil {
		msg = append(append(msg, `,"frame":`...), frame...)
	}
	if image != nil {
		msg = append(append(msg, `,"image":`...), image...)
	}
	if questions != nil {
		msg = append(append(msg, `,"questions":`...), questions...)
	}
	if f.Reset {
		msg = append(msg, `,"reset":true`...)
	}
	msg = append(msg, '}')
	deadline := time.Now().Add(s.client.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.conn.SetWriteDeadline(deadline)
	if err := s.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return 0, &ConnectionError{&Error{msg: "the frame could not be sent: " + err.Error(), Err: err}}
	}
	return seq, nil
}

const maxSeq = 1<<53 - 1

// checkFrame applies a frame's fixed rules and encodes its frame value and
// question list.
func (s *Session) checkFrame(f Frame) (frame, questions []byte, err error) {
	if f.Seq != nil && (*f.Seq < 0 || *f.Seq > maxSeq) {
		return nil, nil, usageError(fmt.Sprintf("seq: must be an integer from 0 to %d.", int64(maxSeq)))
	}
	if f.Frame == nil && f.Image == nil {
		return nil, nil, usageError("frame: a frame needs a frame value or an image.")
	}
	if f.Frame != nil {
		if frame, err = marshal(f.Frame); err != nil {
			return nil, nil, encodeError(err)
		}
		if !isSDL(frame) || string(frame) == `""` {
			return nil, nil, usageError("frame: must be a string, an object or an array, not empty.")
		}
	}
	if f.Questions != nil {
		if len(f.Questions) == 0 {
			return nil, nil, usageError("questions: must be a non-empty list of the session's question ids.")
		}
		if s.Questions != nil {
			for _, id := range f.Questions {
				if !containsString(s.Questions, id) {
					return nil, nil, usageError(fmt.Sprintf("questions: unknown question id '%s'", id))
				}
			}
		}
		if questions, err = marshal(f.Questions); err != nil {
			return nil, nil, encodeError(err)
		}
	}
	if frame != nil && jsonDepth(frame)+1 > maxNesting {
		return nil, nil, usageError(fmt.Sprintf("The frame nests more than %d objects and arrays inside each other.", maxNesting))
	}
	if f.Image != nil {
		if f.Image.url != "" {
			return nil, nil, usageError("image: a frame takes bytes or an upload, not a url.")
		}
		if err := checkImage(*f.Image, s.client.limitsFor(s.Model)); err != nil {
			return nil, nil, err
		}
	}
	return frame, questions, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Next returns the session's next event: a *Reply, a *FrameError or, last,
// a *SessionClosed, which every later call returns again. It waits until an
// event arrives or ctx is done.
func (s *Session) Next(ctx context.Context) (SessionEvent, error) {
	s.mu.Lock()
	q := s.events
	s.mu.Unlock()
	if q == nil {
		return nil, errors.New("decisionnode: the session is not connected")
	}
	return q.next(ctx)
}

// Replies returns the events of the current connection as a channel: each
// *Reply and *FrameError, then the *SessionClosed, after which the channel
// is closed. Frame errors are values, never errors. Use either Replies or
// Next, not both.
func (s *Session) Replies() <-chan SessionEvent {
	s.mu.Lock()
	q := s.events
	s.mu.Unlock()
	if q == nil {
		ch := make(chan SessionEvent)
		close(ch)
		return ch
	}
	return q.channel()
}

// End ends the session with DELETE /v1/sessions/{id}: the server closes an
// attached socket with 1000. A session that has already ended (404) is
// success, with a nil summary.
func (s *Session) End(ctx context.Context) (*SessionSummary, error) {
	if s.client == nil {
		return nil, errSessionWithoutClient
	}
	return s.client.endSession(ctx, s.ID, retryDefault)
}

// EndSession ends a session by its id with DELETE /v1/sessions/{id}, for a
// session this client did not open (another process's, or one whose id was
// kept). It is sent once, never retried. A session that has already ended
// (404) is success, with a nil summary.
func (c *Client) EndSession(ctx context.Context, sessionID string) (*SessionSummary, error) {
	if c.err != nil {
		return nil, c.err
	}
	if sessionID == "" {
		return nil, errors.New("decisionnode: the session id is empty")
	}
	return c.endSession(ctx, sessionID, retryNever)
}

func (c *Client) endSession(ctx context.Context, sessionID string, policy retryPolicy) (*SessionSummary, error) {
	resp, err := c.call(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), nil, callOptions{policy: policy})
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	var shape struct {
		SessionID      string `json:"session_id"`
		Model          string `json:"model"`
		Status         string `json:"status"`
		ExpiresAt      string `json:"expires_at"`
		FramesAnswered int    `json:"frames_answered"`
		FramesDropped  int    `json:"frames_dropped"`
		FramesCut      int    `json:"frames_cut"`
		LastSeq        *int64 `json:"last_seq"`
		TokensBilled   int64  `json:"tokens_billed"`
	}
	_ = json.Unmarshal(resp.data, &shape)
	return &SessionSummary{
		SessionID: shape.SessionID, Model: shape.Model, Status: shape.Status, ExpiresAt: shape.ExpiresAt,
		FramesAnswered: shape.FramesAnswered, FramesDropped: shape.FramesDropped, FramesCut: shape.FramesCut,
		LastSeq: shape.LastSeq, TokensBilled: shape.TokensBilled, Raw: raw,
	}, nil
}

// Close closes the socket from the client's side with 1000, without ending
// the session on the server (a closed socket ends it there too). The last
// event is then a *SessionClosed with code 1000.
func (s *Session) Close() error {
	s.mu.Lock()
	conn, q := s.conn, s.events
	if conn == nil || q.isClosed() {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	s.mu.Unlock()
	deadline := time.Now().Add(s.client.timeout)
	err := conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), deadline)
	// The reader stops at the server's echo, or at this deadline.
	_ = conn.SetReadDeadline(deadline)
	if err != nil {
		_ = conn.Close()
	}
	return nil
}

func checkStreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		switch u.Scheme {
		case "wss":
			return nil
		case "ws":
			switch strings.ToLower(u.Hostname()) {
			case "localhost", "127.0.0.1", "::1":
				return nil
			}
		}
	}
	return fmt.Errorf("decisionnode: the session's stream URL %q must be wss (ws is accepted only for localhost, 127.0.0.1 and ::1)", raw)
}

func streamPath(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Path
	}
	return ""
}

// eventQueue holds a connection's events without bound, so the socket is
// always read (and pings answered) whatever the consumer does.
type eventQueue struct {
	mu     sync.Mutex
	items  []SessionEvent
	closed *SessionClosed
	signal chan struct{}
	once   sync.Once
	ch     chan SessionEvent
}

func newEventQueue() *eventQueue { return &eventQueue{signal: make(chan struct{}, 1)} }

func (q *eventQueue) push(ev SessionEvent) {
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

func (q *eventQueue) close(ev *SessionClosed) {
	q.mu.Lock()
	q.items = append(q.items, ev)
	q.closed = ev
	q.mu.Unlock()
	q.wake()
}

func (q *eventQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *eventQueue) isClosed() bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed != nil
}

// tryNext returns the next event without waiting, or nil.
func (q *eventQueue) tryNext() SessionEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	ev := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	if len(q.items) > 0 {
		q.wake()
	}
	return ev
}

func (q *eventQueue) next(ctx context.Context) (SessionEvent, error) {
	for {
		if ev := q.tryNext(); ev != nil {
			return ev, nil
		}
		q.mu.Lock()
		closed := q.closed
		q.mu.Unlock()
		if closed != nil {
			return closed, nil
		}
		select {
		case <-q.signal:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (q *eventQueue) channel() <-chan SessionEvent {
	q.once.Do(func() {
		q.ch = make(chan SessionEvent, 16)
		go func() {
			defer close(q.ch)
			for {
				ev, _ := q.next(context.Background())
				q.ch <- ev
				if _, done := ev.(*SessionClosed); done {
					return
				}
			}
		}()
	})
	return q.ch
}

// Supervise keeps a feed of frames answered across sessions, as a long-lived
// supervisor needs: it opens a session with body, connects, and sends each
// frame the feed yields, handing every *Reply and *FrameError to onEvent as
// it arrives. When the socket closes it opens a new session with the same
// body on 1001, 1006, 1011, 4404 and 4409 after a capped backoff (1 s to 30 s
// with jitter, reset after a session that answered frames), on 4408 and 4410
// at once, and connects again to the same session on 1013; on any other code
// it stops and returns that *SessionClosed.
//
// When the feed is closed it waits for the outstanding replies (at most the
// client's timeout), ends the session and returns the last *SessionClosed
// seen, or nil when none was. An error opening a session, a local fault in a
// frame, or ctx being done stops it with that error.
func (c *Client) Supervise(ctx context.Context, body SessionRequest, feed <-chan Frame, onEvent func(SessionEvent)) (*SessionClosed, error) {
	if c.err != nil {
		return nil, c.err
	}
	sv := &supervisor{c: c, body: body, onEvent: onEvent}
	for {
		var f Frame
		var ok bool
		select {
		case f, ok = <-feed:
		case <-ctx.Done():
			sv.finish()
			return sv.last, ctx.Err()
		}
		if !ok {
			break
		}
		sv.drain(ctx, 0)
		if stop, err := sv.sendFrame(ctx, f); stop || err != nil {
			sv.finish()
			return sv.last, err
		}
	}
	sv.drain(ctx, c.timeout)
	last := sv.last
	sv.finish()
	return last, nil
}

type supervisor struct {
	c         *Client
	body      SessionRequest
	onEvent   func(SessionEvent)
	session   *Session
	connected bool
	last      *SessionClosed
	failures  int
	answered  int
	pending   int
}

// sendFrame sends one frame, opening or connecting a session first when
// needed. stop is true when a close code says to stop.
func (sv *supervisor) sendFrame(ctx context.Context, f Frame) (stop bool, err error) {
	for {
		for !sv.connected {
			if sv.last != nil && !sv.last.Reopen && sv.last.Code != 1013 {
				return true, nil
			}
			if sv.last != nil && sv.last.Code != 4408 && sv.last.Code != 4410 {
				if sv.answered > 0 {
					sv.failures = 0
				}
				wait := time.Duration(float64(min(30*time.Second, time.Second<<min(sv.failures, 5))) * (0.5 + jitterFloat()*0.5))
				sv.failures++
				if err := sv.c.sleep(ctx, wait); err != nil {
					return false, &Error{msg: "the request was canceled", Err: err}
				}
			}
			if sv.session == nil || sv.last == nil || sv.last.Code != 1013 {
				s, err := sv.c.OpenSession(ctx, sv.body)
				if err != nil {
					return false, err
				}
				sv.session, sv.answered = s, 0
			}
			sv.last = nil
			if err := sv.session.Connect(ctx); err != nil {
				var ce *ConnectionError
				var te *TimeoutError
				if errors.As(err, &ce) || errors.As(err, &te) {
					sv.last = newSessionClosed(websocket.CloseAbnormalClosure, err.Error())
					continue
				}
				return false, err
			}
			sv.connected = true
		}
		_, err := sv.session.Send(ctx, f)
		if err == nil {
			sv.pending++
			return false, nil
		}
		var ce *ConnectionError
		if !errors.As(err, &ce) && !sv.session.events.isClosed() {
			return false, err
		}
		// The socket went away under the frame: read its close, then send the
		// frame again on the next session.
		sv.drain(ctx, sv.c.timeout)
		if sv.connected {
			sv.connected = false
			sv.last = newSessionClosed(websocket.CloseAbnormalClosure, "the frame could not be sent")
		}
	}
}

// drain hands the waiting events to onEvent. With a wait above 0 it also
// waits, until no reply is outstanding, the socket closes, or the wait ends.
func (sv *supervisor) drain(ctx context.Context, wait time.Duration) {
	if !sv.connected {
		return
	}
	dctx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	for sv.connected {
		var ev SessionEvent
		if wait > 0 {
			if sv.pending == 0 {
				return
			}
			var err error
			if ev, err = sv.session.events.next(dctx); err != nil {
				return
			}
		} else if ev = sv.session.events.tryNext(); ev == nil {
			return
		}
		switch e := ev.(type) {
		case *SessionClosed:
			sv.last, sv.connected = e, false
			return
		case *Reply:
			sv.answered++
			sv.pending = max(0, sv.pending-1-len(e.Dropped))
		case *FrameError:
			sv.pending = max(0, sv.pending-1)
		}
		if sv.onEvent != nil {
			sv.onEvent(ev)
		}
	}
}

// finish ends a session still connected and closes its socket.
func (sv *supervisor) finish() {
	if sv.session == nil || !sv.connected {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sv.c.timeout)
	defer cancel()
	_, _ = sv.session.End(ctx)
	_ = sv.session.Close()
	sv.connected = false
}

func jitterFloat() float64 {
	jitterMu.Lock()
	defer jitterMu.Unlock()
	return jitter.Float64()
}
