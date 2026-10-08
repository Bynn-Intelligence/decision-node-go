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
	ids, err := c.Upload(context.Background(), []dn.Image{dn.ImageData("a", fixture(t, "3x2.jpg"), "")})
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
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
		reqs[i] = dn.BatchRequest{CustomID: fmt.Sprint(i), Request: dn.Request{State: strings.Repeat("x", 3_800_000), Questions: oneTruth()}}
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
	reqs := []dn.BatchRequest{{CustomID: "a", Request: dn.Request{Questions: oneTruth()}}}
	var apiErr *dn.Error
	if _, _, err := c.RunBatch(context.Background(), reqs, dn.RunBatchOptions{PollInterval: 5 * time.Second}); err == nil || errors.As(err, &apiErr) {
		t.Fatalf("poll interval: %v", err)
	}
	bad := []dn.BatchRequest{{CustomID: "a", Request: dn.Request{Questions: dn.Questions{dn.Q("q", dn.Score("?", []string{}))}}}}
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
