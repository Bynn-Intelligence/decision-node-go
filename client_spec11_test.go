package decisionnode_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

// storageServer answers presigns, PUTs and decisions, and records them.
type storageServer struct {
	mu       sync.Mutex
	presigns int
	puts     []http.Header
	decides  [][]byte
	srv      *httptest.Server
}

func newStorageServer(t *testing.T) *storageServer {
	s := &storageServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/uploads":
			s.presigns++
			var req struct {
				Files []map[string]any `json:"files"`
			}
			_ = json.Unmarshal(body, &req)
			var ups []map[string]any
			for i, f := range req.Files {
				ups = append(ups, map[string]any{
					"upload_id":  fmt.Sprintf("upl_%032x", s.presigns*10+i),
					"url":        s.srv.URL + fmt.Sprintf("/store/%d-%d?X-Amz-Signature=x", s.presigns, i),
					"method":     "PUT",
					"headers":    map[string]string{"Content-Type": f["content_type"].(string), "Content-MD5": f["checksum"].(string), "If-None-Match": "*"},
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				})
			}
			respond(w, 201, string(mustJSON(map[string]any{"uploads": ups, "expires_in": 300})), nil)
		case strings.HasPrefix(r.URL.Path, "/store/"):
			s.puts = append(s.puts, r.Header.Clone())
			w.WriteHeader(200)
		default:
			s.decides = append(s.decides, body)
			respond(w, 200, truthBody, nil)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func bigPNG(t *testing.T, size int) []byte {
	b := fixture(t, "3x2.png")
	return append(b, make([]byte, size-len(b))...)
}

func TestLargeImageIsUploadedOnceAndReused(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL, dn.WithHeaders(map[string]string{"x-custom": "1"}))
	img := dn.ImageData("r", bigPNG(t, 600_000), "")
	reqs := make([]dn.Request, 6)
	for i := range reqs {
		reqs[i] = dn.Request{State: fmt.Sprint(i), Questions: oneTruth(), Images: []dn.Image{img}}
	}
	// Six concurrent requests with the same bytes: one presign, one PUT.
	res, err := c.DecideMany(context.Background(), reqs, 6)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presigns != 1 || len(s.puts) != 1 || len(s.decides) != 6 {
		t.Fatalf("%d presigns, %d puts, %d decisions", s.presigns, len(s.puts), len(s.decides))
	}
	put := s.puts[0]
	for _, h := range []string{"Authorization", "X-Custom", "User-Agent"} {
		if put.Get(h) != "" {
			t.Errorf("the PUT carried %s: %q", h, put.Get(h))
		}
	}
	if put.Get("If-None-Match") != "*" || put.Get("Content-Type") != "image/png" {
		t.Errorf("the PUT headers: %v", put)
	}
	for _, d := range s.decides {
		if !bytes.Contains(d, []byte(`"images":[{"id":"r","upload_id":"upl_`)) || bytes.Contains(d, []byte(`"data"`)) {
			t.Fatalf("decision body: %.200s", d)
		}
	}
}

func TestSmallImagesGoInline(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL)
	_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Images: []dn.Image{dn.ImageData("a", fixture(t, "3x2.png"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	ups, err := c.Upload(context.Background(), []dn.Image{dn.ImageData("a", fixture(t, "3x2.jpg"), "")})
	if err != nil || len(ups) != 1 || !strings.HasPrefix(ups[0].ID, "upl_") || time.Until(ups[0].ExpiresAt) < 50*time.Minute {
		t.Fatal(ups, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presigns != 1 || !bytes.Contains(s.decides[0], []byte(`"media_type":"image/png","data":"`)) {
		t.Fatalf("%d presigns; %.200s", s.presigns, s.decides[0])
	}
}

func TestBatchAddSplitsAt10MiB(t *testing.T) {
	var sizes []int
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		sizes = append(sizes, bytes.Count(body, []byte("\n")))
		respond(w, 200, `{"id":"batch_0123456789","state":"draft","counters":{"requests":3},"ended_at":null}`, nil)
	})
	reqs := make([]dn.BatchRequest, 3)
	for i := range reqs {
		reqs[i] = dn.BatchRequest{CustomID: dn.Ptr(fmt.Sprint(i)), Request: dn.Request{State: strings.Repeat("x", 3_800_000), Questions: oneTruth()}}
	}
	b, err := client(t, srv.URL).Batches().Add(context.Background(), "batch_0123456789", reqs)
	if err != nil || b.State != "draft" || b.Ended() {
		t.Fatal(b, err)
	}
	if fmt.Sprint(sizes) != "[2 1]" {
		t.Fatalf("lines per call: %v", sizes)
	}
}

func TestRunBatchPollIntervalAndLocalChecks(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, `{}`, nil) })
	c := client(t, srv.URL)
	reqs := []dn.BatchRequest{{CustomID: dn.Ptr("a"), Request: dn.Request{Questions: oneTruth()}}}
	var apiErr *dn.Error
	if _, _, err := c.RunBatch(context.Background(), reqs, dn.RunBatchOptions{PollInterval: 5 * time.Second}); err == nil || errors.As(err, &apiErr) {
		t.Fatalf("poll interval: %v", err)
	}
	bad := []dn.BatchRequest{{CustomID: dn.Ptr("a"), Request: dn.Request{Questions: dn.Questions{dn.Q("q", dn.Score("?", []string{}))}}}}
	var v *dn.ValidationError
	if _, _, err := c.RunBatch(context.Background(), bad, dn.RunBatchOptions{}); !errors.As(err, &v) {
		t.Fatalf("local check: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
}

func TestSessionLimitIsNotRetried(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 429, `{"detail":{"error_type":"session_limit_exceeded","message":"This key already holds 4 open sessions, the limit."}}`, map[string]string{"retry-after": "3"})
	})
	_, err := client(t, srv.URL).OpenSession(context.Background(), dn.SessionRequest{Questions: oneTruth()})
	var limit *dn.SessionLimitError
	var rate *dn.RateLimitError
	if !errors.As(err, &limit) || !errors.As(err, &rate) || rate.RetryAfter != 3*time.Second || rec.count() != 1 {
		t.Fatalf("got %v after %d attempts", err, rec.count())
	}
}

func TestEndSessionByID(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/sessions/ses_abc" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if n == 1 {
			respond(w, 200, `{"session_id":"ses_abc","status":"ended","frames_answered":3,"last_seq":3,"tokens_billed":120}`, nil)
			return
		}
		if n == 2 {
			respond(w, 404, `{"detail":{"error_type":"not_found_error","message":"No open session with this id for this key."}}`, nil)
			return
		}
		respond(w, 500, `{"detail":{"error_type":"api_error","message":"x"}}`, nil)
	})
	c := client(t, srv.URL)
	sum, err := c.EndSession(context.Background(), "ses_abc")
	if err != nil || sum.FramesAnswered != 3 || *sum.LastSeq != 3 || sum.Status != "ended" {
		t.Fatal(sum, err)
	}
	if sum, err := c.EndSession(context.Background(), "ses_abc"); sum != nil || err != nil {
		t.Fatal("a 404 is success with no summary", sum, err)
	}
	var se *dn.ServerError
	if _, err := c.EndSession(context.Background(), "ses_abc"); !errors.As(err, &se) || rec.count() != 3 {
		t.Fatalf("a 500 is not retried: %v after %d requests", err, rec.count())
	}
}

func TestSessionFramesAndRawJSON(t *testing.T) {
	reply := `{"seq":1,"answers":{"abort":{"type":"truth","truth":0.8200000000000001}},"usage":{"input_tokens":3,"output_tokens":0},"latency_ms":1.0}`
	var got []string
	var mu sync.Mutex
	up := websocket.Upgrader{}
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" {
			respond(w, 200, `{"session_id":"ses_000000000000000000000001","model":"decisionnode-1.0","stream_url":"`+
				strings.Replace(srvURL, "http", "ws", 1)+`/stream","questions":["abort"],"answers":{}}`, nil)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		mu.Lock()
		got = append(got, string(msg))
		mu.Unlock()
		_ = conn.WriteMessage(websocket.TextMessage, []byte(reply))
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4410, "expired"))
		_, _, _ = conn.ReadMessage()
	}))
	defer srv.Close()
	srvURL = srv.URL
	ctx := context.Background()
	s, err := client(t, srv.URL).OpenSession(ctx, dn.SessionRequest{Questions: dn.Questions{dn.Q("abort", dn.Truth("Abort?"))}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	var bad *dn.BadRequestError
	for name, f := range map[string]dn.Frame{
		"no frame":         {},
		"empty frame":      {Frame: ""},
		"unknown question": {Frame: "a", Questions: []string{"speed"}},
		"empty questions":  {Frame: "a", Questions: []string{}},
		"negative seq":     {Frame: "a", Seq: dn.Ptr(int64(-1))},
		"url image":        {Image: dn.Ptr(dn.ImageURL("f", "https://example.com/f.jpg"))},
		"number frame":     {Frame: 5},
	} {
		if _, err := s.Send(ctx, f); !errors.As(err, &bad) || bad.ErrorType != "api_usage_error" {
			t.Errorf("%s: got %v", name, err)
		}
	}
	seq, err := s.Send(ctx, dn.Frame{Frame: "a"})
	if err != nil || seq != 1 {
		t.Fatalf("the first frame sent after refusals: seq %d, %v", seq, err)
	}
	ev, _ := s.Next(ctx)
	r, ok := ev.(*dn.Reply)
	if !ok || string(r.RawJSON) != reply {
		t.Fatalf("reply %#v", ev)
	}
	ev, _ = s.Next(ctx)
	closed, ok := ev.(*dn.SessionClosed)
	if !ok || closed.Code != 4410 || !closed.Reopen || string(closed.RawJSON) != `{"code":4410,"reason":"expired","reopen":true}` {
		t.Fatalf("closed %#v %s", ev, closed.RawJSON)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != `{"seq":1,"frame":"a"}` {
		t.Fatalf("frames on the socket: %q", got)
	}
}

func TestChecksSendNothing(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	c := client(t, srv.URL)
	big := dn.ImageData("scan", bigPNG(t, 600_000), "")
	if err := c.CheckDecide(dn.Request{Questions: oneTruth(), Images: []dn.Image{big}}); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckDecide(dn.Request{Questions: dn.Questions{dn.Q("n", dn.NumberWith("How many?", 60, 50, nil))}}); err == nil {
		t.Fatal("a grid with min above max passed the check")
	}
	if err := c.CheckSession(dn.SessionRequest{Questions: oneTruth(), Window: dn.Ptr(65)}); err == nil {
		t.Fatal("window 65 passed")
	}
	dup := []dn.BatchRequest{{CustomID: dn.Ptr("a"), Request: dn.Request{Questions: oneTruth()}}, {CustomID: dn.Ptr("a"), Request: dn.Request{Questions: oneTruth()}}}
	var bad *dn.BadRequestError
	if err := c.CheckBatchCreate(dup); !errors.As(err, &bad) || bad.Error() != `Line 2: custom_id "a" is already in the batch.` {
		t.Fatalf("got %v", err)
	}
	if err := c.CheckBatchCreate(nil); err != nil {
		t.Fatal("an empty create is valid:", err)
	}
	if err := c.CheckBatchAdd(nil); !errors.As(err, &bad) {
		t.Fatal("an empty add passed")
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
}

func TestResultsStreamLineByLine(t *testing.T) {
	const lines = 3000
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher := w.(http.Flusher)
		for i := 0; i < lines; i++ {
			fmt.Fprintf(w, `{"custom_id":"r%d","status":200,"body":{"model":"decisionnode-1.0","answers":{"q":{"type":"truth","truth":0.5}}},"headers":{"x-request-id":"req_%d"}}`+"\n", i, i)
			if i == lines/2 {
				flusher.Flush()
				// A pause shorter than the idle timeout does not end the read.
				time.Sleep(300 * time.Millisecond)
			}
		}
	})
	c := client(t, srv.URL, dn.WithTimeout(time.Second))
	r, err := c.Batches().ResultsReader(context.Background(), "batch_0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := 0
	for r.Next() {
		res := r.Result()
		if res.CustomID != fmt.Sprintf("r%d", n) || res.Decision.Truth("q").Truth != 0.5 || res.Headers["x-request-id"] != fmt.Sprintf("req_%d", n) {
			t.Fatalf("line %d: %+v", n, res)
		}
		n++
	}
	if r.Err() != nil || n != lines {
		t.Fatalf("%d lines, %v", n, r.Err())
	}
}

func TestResultsIdleTimeout(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		fmt.Fprintln(w, `{"custom_id":"a","status":"cancelled","body":null,"headers":{}}`)
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
	})
	r, err := client(t, srv.URL, dn.WithTimeout(300*time.Millisecond), dn.WithMaxRetries(0)).Batches().ResultsReader(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.Next() || r.Result().StatusText != "cancelled" {
		t.Fatal("the first line")
	}
	var te *dn.TimeoutError
	if r.Next() || !errors.As(r.Err(), &te) {
		t.Fatalf("a stalled body: %v", r.Err())
	}
}

func TestUploadTimeoutArgument(t *testing.T) {
	clearEnv(t)
	if err := dn.NewClient(dn.WithUploadTimeout(0)).Err(); err == nil {
		t.Fatal("an upload timeout of 0 was accepted")
	}
}

func TestSessionSocketUsesTheClientTransport(t *testing.T) {
	up := websocket.Upgrader{}
	var srv *httptest.Server
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" {
			respond(w, 200, `{"session_id":"ses_000000000000000000000001","stream_url":"`+strings.Replace(srv.URL, "https", "wss", 1)+`/stream","questions":["q"]}`, nil)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""))
		_ = conn.Close()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	// The test server's certificate is trusted only through this transport,
	// and both sides offer h2: the socket must still dial HTTP/1.1.
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = true
	c := client(t, srv.URL, dn.WithHTTPClient(&http.Client{Transport: tr}))
	s, err := c.OpenSession(context.Background(), dn.SessionRequest{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Connect(context.Background()); err != nil {
		t.Fatalf("connect through the client's TLS settings: %v", err)
	}
	ev, _ := s.Next(context.Background())
	if closed, ok := ev.(*dn.SessionClosed); !ok || closed.Code != 1000 {
		t.Fatalf("got %#v", ev)
	}
}

func TestBatchResultRawJSONIsByteIdentical(t *testing.T) {
	lines := []string{
		`{"custom_id":"count-1","status":200,"body":{"model":"decisionnode-1.0","answers":{"cars":{"type":"number","number":7,"expected":7.0,"confidence":0.78,"probabilities":{"8":0.098,"6":0.105,"7":0.783}}},"usage":{"input_tokens":300,"output_tokens":0}},"headers":{"x-request-id":"req_1"}}`,
		`{"custom_id":"t2" , "status":"expired","body":null,"headers":{}}`,
	}
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, lines[0]+"\n"+lines[1]+"\r\n")
	})
	got, err := client(t, srv.URL).Batches().Results(context.Background(), "batch_0123456789")
	if err != nil || len(got) != 2 {
		t.Fatal(got, err)
	}
	for i := range lines {
		if string(got[i].RawJSON) != lines[i] {
			t.Fatalf("line %d:\ngot  %s\nwant %s", i, got[i].RawJSON, lines[i])
		}
	}
	if got[0].Decision.Number("cars").Expected != 7 || got[1].StatusText != "expired" {
		t.Fatal("parsed fields")
	}
}
