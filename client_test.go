package decisionnode_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const truthBody = `{"model":"decisionnode-1.0","answers":{"q":{"type":"truth","truth":0.94}},"usage":{"input_tokens":9,"output_tokens":0}}`

func clearEnv(t *testing.T) {
	for _, v := range []string{"DECISIONNODE_API_KEY", "DECISIONNODE_BASE_URL", "DECISIONNODE_MODEL"} {
		t.Setenv(v, "")
	}
}

type recorded struct {
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
}

func (r *recorded) add(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(b))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, b)
	r.heads = append(r.heads, req.Header.Clone())
}

func (r *recorded) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, *recorded) {
	rec := &recorded{}
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		handler(w, r, int(atomic.AddInt32(&n, 1)))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func respond(w http.ResponseWriter, status int, body string, headers map[string]string) {
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func client(t *testing.T, url string, opts ...dn.Option) *dn.Client {
	clearEnv(t)
	return dn.NewClient(append([]dn.Option{dn.WithAPIKey("dn_test"), dn.WithBaseURL(url)}, opts...)...)
}

func oneTruth() dn.Questions { return dn.Questions{dn.Q("q", dn.Truth("?"))} }

func TestBuildersWireForm(t *testing.T) {
	cases := []struct {
		name string
		q    any
		want string
	}{
		{"truth", dn.Truth("Escalate?"), `{"type":"truth","instructions":"Escalate?"}`},
		{"truth both sides", dn.TruthWith("Refund?", "asks for money back", "anything else"), `{"type":"truth","instructions":"Refund?","criteria":{"true":"asks for money back","false":"anything else"}}`},
		{"truth one side", dn.TruthWith(nil, nil, "no"), `{"type":"truth","criteria":{"false":"no"}}`},
		{"truth no criteria", dn.TruthWith("x", nil, nil), `{"type":"truth","instructions":"x"}`},
		{"choice names", dn.Choice("Pick", []string{"b", "a"}), `{"type":"choice","instructions":"Pick","criteria":{"b":null,"a":null}}`},
		{"choice map", dn.Choice("Pick", map[string]string{"b": "bee", "a": "ay"}), `{"type":"choice","instructions":"Pick","criteria":{"a":"ay","b":"bee"}}`},
		{"choice ordered", dn.Choice(nil, dn.Options{{Name: "z", Description: "last"}, {Name: "", Description: nil}}), `{"type":"choice","criteria":{"z":"last","":null}}`},
		{"score", dn.Score("How urgent?", []string{"low", "high"}), `{"type":"score","instructions":"How urgent?","criteria":["low","high"]}`},
		{"score structured", dn.Score(map[string]any{"question": "q"}, []any{map[string]any{"what": "none"}, "some"}), `{"type":"score","instructions":{"question":"q"},"criteria":[{"what":"none"},"some"]}`},
		{"no html escaping", dn.Truth("a < b && c > d"), `{"type":"truth","instructions":"a < b && c > d"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(c.q); err != nil {
				t.Fatal(err)
			}
			got := bytes.TrimSpace(buf.Bytes())
			if !bytes.Equal(got, []byte(c.want)) {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestDecideSendsOrderedBody(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"decisionnode-1.0","answers":{"z":{"type":"truth","truth":0.1},"a":{"type":"choice","choice":"x","confidence":0.5,"probabilities":{"x":0.75,"y":0.25}}},"usage":{"input_tokens":3,"output_tokens":0}}`,
			map[string]string{"x-request-id": "req_1", "x-decisionnode-latency-ms": "12.5", "x-decisionnode-safety": "harm=0.031, self_harm=0.004"})
	})
	c := client(t, srv.URL+"/")
	d, err := c.Decide(context.Background(), dn.Request{
		Questions: dn.Questions{
			dn.Q("z", dn.Truth("last first?")),
			dn.Q("a", map[string]any{"type": "choice", "criteria": map[string]any{"x": nil, "y": nil}}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(rec.bodies[0])
	want := `{"model":"decisionnode-latest","state":"","questions":{"z":{"type":"truth","instructions":"last first?"},"a":{"criteria":{"x":null,"y":null},"type":"choice"}}}`
	if body != want {
		t.Fatalf("body\n got %s\nwant %s", body, want)
	}
	h := rec.heads[0]
	if h.Get("Authorization") != "Bearer dn_test" || h.Get("User-Agent") != dn.UserAgent || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %v", h)
	}
	if d.Truth("z").Truth != 0.1 || d.Choice("a").Choice != "x" || d.Choice("a").Probabilities["y"] != 0.25 {
		t.Fatalf("answers: %+v", d.Answers)
	}
	if d.Truth("a") != nil || d.Score("z") != nil || d.Answer("missing") != nil {
		t.Fatal("typed accessors must return nil for another type")
	}
	if d.RequestID != "req_1" || d.LatencyMs == nil || *d.LatencyMs != 12.5 || d.Usage.InputTokens == nil || *d.Usage.InputTokens != 3 || d.Model != "decisionnode-1.0" {
		t.Fatalf("decision: %+v", d)
	}
	if d.Safety == nil || !d.Safety.Checked || d.Safety.Harm != 0.031 {
		t.Fatalf("safety: %+v", d.Safety)
	}
}

func TestEnvironmentAndPrecedence(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	clearEnv(t)
	t.Setenv("DECISIONNODE_API_KEY", "dn_env")
	t.Setenv("DECISIONNODE_BASE_URL", srv.URL)
	t.Setenv("DECISIONNODE_MODEL", "decisionnode-flash-latest")
	ctx := context.Background()

	if _, err := dn.NewClient().Decide(ctx, dn.Request{Questions: oneTruth()}); err != nil {
		t.Fatal(err)
	}
	if _, err := dn.NewClient(dn.WithAPIKey("dn_arg"), dn.WithModel("decisionnode-1.0")).Decide(ctx, dn.Request{Questions: oneTruth()}); err != nil {
		t.Fatal(err)
	}
	if _, err := dn.NewClient().Decide(ctx, dn.Request{Questions: oneTruth(), Model: "decisionnode-1.0-flash"}); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"Bearer dn_env", "Bearer dn_arg", "Bearer dn_env"}
	wantModels := []string{"decisionnode-flash-latest", "decisionnode-1.0", "decisionnode-1.0-flash"}
	for i := range wantKeys {
		var body map[string]any
		_ = json.Unmarshal(rec.bodies[i], &body)
		if rec.heads[i].Get("Authorization") != wantKeys[i] || body["model"] != wantModels[i] {
			t.Errorf("request %d: key %q model %v", i, rec.heads[i].Get("Authorization"), body["model"])
		}
	}
}

func TestFailsBeforeSending(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	ctx := context.Background()
	clearEnv(t)

	_, err := dn.NewClient(dn.WithBaseURL(srv.URL)).Decide(ctx, dn.Request{Questions: oneTruth()})
	var auth *dn.AuthenticationError
	if !errors.As(err, &auth) {
		t.Fatalf("no key: got %v", err)
	}

	c := client(t, srv.URL)
	_, err = c.Decide(ctx, dn.Request{State: "s"})
	var base *dn.Error
	if err == nil || errors.As(err, &base) {
		t.Fatalf("no questions: got %v", err)
	}

	if _, err := dn.ImageFile("x", filepath.Join(t.TempDir(), "missing.png")); !errors.Is(err, os.ErrNotExist) || errors.As(err, &base) {
		t.Fatalf("missing image file: got %v", err)
	}

	// Spec 1.1: a file is read when the Image is made, and its format is
	// checked at the call, where another format is the API's own
	// *BadRequestError, with no status and no request id.
	gif := filepath.Join(t.TempDir(), "a.gif")
	_ = os.WriteFile(gif, []byte("GIF89a...."), 0o600)
	gifImage, err := dn.ImageFile("x", gif)
	if err != nil {
		t.Fatalf("a readable file: got %v", err)
	}
	var bad *dn.BadRequestError
	_, err = c.Decide(ctx, dn.Request{Questions: oneTruth(), Images: []dn.Image{gifImage}})
	if !errors.As(err, &bad) || bad.Status != 0 || bad.RequestID != "" || bad.ErrorType != "api_usage_error" {
		t.Fatalf("unsupported image file: got %#v", err)
	}
	_, err = c.Decide(ctx, dn.Request{Questions: oneTruth(), Images: []dn.Image{dn.ImageData("x", []byte("GIF89a...."), "")}})
	if !errors.As(err, &bad) {
		t.Fatalf("unsupported image bytes: got %v", err)
	}

	_, err = c.Decide(ctx, dn.Request{Questions: oneTruth(), State: map[string]any{"bad": func() {}}})
	if err == nil || errors.As(err, &base) {
		t.Fatalf("unencodable state: got %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImages(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	png := filepath.Join(t.TempDir(), "p")
	pngBytes := fixture(t, "3x2.png")
	_ = os.WriteFile(png, pngBytes, 0o600)
	fileImage, err := dn.ImageFile("file", png)
	if err != nil {
		t.Fatal(err)
	}
	// The file is read when the Image is made: removing it now changes nothing.
	_ = os.Remove(png)
	jpg := fixture(t, "3x2.jpg")
	webp := fixture(t, "3x2-lossless.webp")
	b64, err := dn.ImageBase64("b64", base64.StdEncoding.EncodeToString(webp), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client(t, srv.URL).Decide(context.Background(), dn.Request{
		Questions: oneTruth(),
		Images: []dn.Image{
			fileImage,
			dn.ImageData("bytes", jpg, ""),
			b64,
			dn.ImageURL("remote", "https://example.com/r.jpg"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Images []map[string]string `json:"images"`
	}
	_ = json.Unmarshal(rec.bodies[0], &body)
	want := []map[string]string{
		{"id": "file", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes)},
		{"id": "bytes", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(jpg)},
		{"id": "b64", "media_type": "image/webp", "data": base64.StdEncoding.EncodeToString(webp)},
		{"id": "remote", "url": "https://example.com/r.jpg"},
	}
	if len(body.Images) != len(want) {
		t.Fatalf("images: %v", body.Images)
	}
	for i := range want {
		for k, v := range want[i] {
			if body.Images[i][k] != v || len(body.Images[i]) != len(want[i]) {
				t.Errorf("image %d: got %v, want %v", i, body.Images[i], want[i])
			}
		}
	}
}

func TestErrorTypesMatchWithErrorsAs(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.Header.Get("x-case") {
		case "context":
			respond(w, 400, `{"detail":{"error_type":"max_tokens_exceeded"}}`, map[string]string{"x-request-id": "req_c", "x-decisionnode-error": "the request is 9000 tokens, the limit is 8192"})
		case "refusal":
			respond(w, 403, `{"detail":{"error_type":"refusal_error","message":"refused","category":"self_harm"}}`, map[string]string{"x-decisionnode-safety": "harm=0.002, self_harm=0.97;blocked"})
		case "validation":
			respond(w, 422, `{"detail":[{"type":"missing","loc":["body","questions"],"msg":"Field required"}]}`, nil)
		case "rate":
			respond(w, 429, `{"detail":{"error_type":"rate_limit_error","message":"slow down"}}`, map[string]string{"retry-after": "3"})
		}
	})
	ctx := context.Background()
	call := func(name string) error {
		_, err := client(t, srv.URL, dn.WithMaxRetries(0), dn.WithHeaders(map[string]string{"x-case": name})).Decide(ctx, dn.Request{Questions: oneTruth()})
		return err
	}

	err := call("context")
	var cl *dn.ContextLimitError
	var br *dn.BadRequestError
	var base *dn.Error
	if !errors.As(err, &cl) || !errors.As(err, &br) || !errors.As(err, &base) {
		t.Fatalf("context limit: %v", err)
	}
	if base.Status != 400 || base.RequestID != "req_c" || !strings.Contains(base.Reason, "9000 tokens") || base.ErrorType != "max_tokens_exceeded" {
		t.Fatalf("context limit fields: %+v / %s", base, err)
	}

	err = call("refusal")
	var rf *dn.RefusalError
	if !errors.As(err, &rf) || rf.Category != "self_harm" || rf.Safety == nil || rf.Safety.Flagged[0] != "self_harm" || rf.Error() != "refused" {
		t.Fatalf("refusal: %v", err)
	}

	err = call("validation")
	var ve *dn.ValidationError
	if !errors.As(err, &ve) || len(ve.Entries) != 1 {
		t.Fatalf("validation: %v", err)
	}

	err = call("rate")
	var rl *dn.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 3*time.Second {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestRetriesAndHonoursRetryAfterMs(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			respond(w, 529, `{"detail":{"error_type":"overloaded_error","message":"busy"}}`, map[string]string{"retry-after": "5", "retry-after-ms": "30"})
			return
		}
		respond(w, 200, truthBody, map[string]string{"x-request-id": "req_last"})
	})
	start := time.Now()
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if rec.count() != 3 || d.RequestID != "req_last" {
		t.Fatalf("attempts %d, request id %q", rec.count(), d.RequestID)
	}
	if el := time.Since(start); el < 60*time.Millisecond || el > 2*time.Second {
		t.Fatalf("elapsed %v: retry-after-ms not honoured", el)
	}
}

func TestNeverRetried(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 422} {
		srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
			respond(w, status, `{"detail":{"error_type":"x","message":"no"}}`, nil)
		})
		_, err := client(t, srv.URL, dn.WithMaxRetries(5)).Decide(context.Background(), dn.Request{Questions: oneTruth()})
		if err == nil || rec.count() != 1 {
			t.Errorf("status %d: %d attempts, err %v", status, rec.count(), err)
		}
	}
}

func TestTimeoutIsRetriedThenReported(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	_, err := client(t, srv.URL, dn.WithTimeout(50*time.Millisecond), dn.WithMaxRetries(1)).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var te *dn.TimeoutError
	if !errors.As(err, &te) || !te.Timeout() {
		t.Fatalf("got %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("attempts: %d", rec.count())
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 503, `{"detail":"down"}`, map[string]string{"retry-after": "10"})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client(t, srv.URL).Decide(ctx, dn.Request{Questions: oneTruth()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second || rec.count() != 1 {
		t.Fatalf("elapsed %v, attempts %d", time.Since(start), rec.count())
	}
}

func TestDecideManyOrderAndConcurrency(t *testing.T) {
	var inFlight, peak int32
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if cur <= p || atomic.CompareAndSwapInt32(&peak, p, cur) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		var body struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(mustRead(r), &body)
		if body.State == "fail" {
			respond(w, 422, `{"detail":[]}`, nil)
			return
		}
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":1,"output_tokens":0}}`, map[string]string{"x-request-id": body.State})
	})
	states := []string{"a", "b", "fail", "c", "d", "e", "f"}
	var reqs []dn.Request
	for _, s := range states {
		reqs = append(reqs, dn.Request{State: s, Questions: oneTruth()})
	}
	results, err := client(t, srv.URL).DecideMany(context.Background(), reqs, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range states {
		if s == "fail" {
			var ve *dn.ValidationError
			if !errors.As(results[i].Err, &ve) {
				t.Errorf("result %d: %v", i, results[i].Err)
			}
			continue
		}
		if results[i].Err != nil || results[i].Decision.RequestID != s {
			t.Errorf("result %d: %+v", i, results[i])
		}
	}
	if peak > 3 || peak < 2 {
		t.Fatalf("peak concurrency %d", peak)
	}
}

func mustRead(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

func TestHelpers(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var body struct {
			Questions map[string]map[string]any `json:"questions"`
		}
		_ = json.Unmarshal(mustRead(r), &body)
		switch {
		case body.Questions["check"] != nil:
			respond(w, 200, `{"model":"m","answers":{"check":{"type":"truth","truth":0.7}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
		case body.Questions["classify"] != nil:
			respond(w, 200, `{"model":"m","answers":{"classify":{"type":"choice","choice":"b","confidence":0.2,"probabilities":{"a":0.4,"b":0.6}}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
		case body.Questions["score"] != nil:
			respond(w, 200, `{"model":"m","answers":{"score":{"type":"score","score":1.5,"confidence":0.5,"legend":{"0":"lo","1":"mid","2":{"what":"hi"}},"probabilities":{"0":0.1,"1":0.30000000000000004,"2":0.6}}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
		default:
			respond(w, 200, `{"model":"m","answers":{"risk":{"type":"score","score":3.1,"confidence":0.6,"legend":{"0":"none"},"probabilities":{"0":1.0}},"touches_production":{"type":"truth","truth":0.9},"verdict":{"type":"choice","choice":"block","confidence":0.9,"probabilities":{"allow":0.01,"confirm":0.04,"block":0.95}}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
		}
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	ta, err := c.Check(ctx, dn.CheckRequest{State: "s", Instructions: "?", IfTrue: "yes"})
	if err != nil || ta.Truth != 0.7 || ta.Type() != "truth" {
		t.Fatalf("check: %v %+v", err, ta)
	}
	ca, err := c.Classify(ctx, dn.ClassifyRequest{Instructions: "?", Options: []string{"a", "b"}})
	if err != nil || ca.Choice != "b" || ca.Probabilities["a"] != 0.4 {
		t.Fatalf("classify: %v %+v", err, ca)
	}
	sa, err := c.Score(ctx, dn.ScoreRequest{Instructions: "?", Levels: []string{"lo", "mid", "hi"}})
	if err != nil || sa.Score != 1.5 || sa.Probabilities[1] != 0.30000000000000004 || sa.Legend[2].(map[string]any)["what"] != "hi" {
		t.Fatalf("score: %v %+v", err, sa)
	}
	d, err := c.Gate(ctx, dn.GateRequest{Action: "rm -rf /srv/data"})
	if err != nil || d.Choice("verdict").Choice != "block" || d.Truth("touches_production").Truth != 0.9 {
		t.Fatalf("gate: %v %+v", err, d)
	}
	var gate struct {
		State map[string]any `json:"state"`
	}
	_ = json.Unmarshal(rec.bodies[3], &gate)
	if _, has := gate.State["context"]; has || gate.State["action"] != "rm -rf /srv/data" {
		t.Fatalf("gate state: %v", gate.State)
	}
}

func TestModelsAndDecideRaw(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Method == http.MethodGet {
			respond(w, 200, `{"models":[{"name":"decisionnode-1.0","description":"d","release_date":"2026-10-01","max_images":4}]}`, nil)
			return
		}
		respond(w, 200, `{"model":"m","answers":{},"usage":{"input_tokens":1,"output_tokens":0},"new_field":[1,2.5]}`, nil)
	})
	c := client(t, srv.URL)
	ms, err := c.Models(context.Background())
	if err != nil || len(ms) != 1 || ms[0].Name != "decisionnode-1.0" || ms[0].Limits["max_images"] != float64(4) || len(ms[0].Limits) != 1 {
		t.Fatalf("models: %v %+v", err, ms)
	}
	if rec.heads[0].Get("Content-Type") != "" || len(rec.bodies[0]) != 0 {
		t.Fatal("GET must carry no body")
	}
	out, err := c.DecideRaw(context.Background(), json.RawMessage(`{"questions":{"q":{"type":"truth"}},"future":true}`))
	if err != nil || out["new_field"].([]any)[1] != 2.5 {
		t.Fatalf("raw: %v %v", err, out)
	}
	if string(rec.bodies[1]) != `{"questions":{"q":{"type":"truth"}},"future":true}` {
		t.Fatalf("raw body: %s", rec.bodies[1])
	}
}

func TestUnknownAnswerType(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"f":{"type":"ranking","order":["b","a"]}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	u, ok := d.Answer("f").(*dn.UnknownAnswer)
	if !ok || u.Type() != "ranking" || u.Raw()["order"].([]any)[0] != "b" {
		t.Fatalf("got %#v", d.Answer("f"))
	}
	if d.Safety != nil || d.LatencyMs != nil {
		t.Fatal("no safety or latency header: both must be empty")
	}
}

func TestDebugLogNeverHoldsState(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, truthBody, map[string]string{"x-request-id": "req_log"})
	})
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	_, err := client(t, srv.URL, dn.WithLogger(logger)).Decide(context.Background(), dn.Request{State: "secret-state", Questions: dn.Questions{dn.Q("q", dn.Truth("secret-question"))}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"method=POST", "path=/v1/decide", "status=200", "request_id=req_log", "elapsed="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}
	for _, never := range []string{"secret-state", "secret-question", "dn_test"} {
		if strings.Contains(out, never) {
			t.Errorf("log holds %q: %s", never, out)
		}
	}
}

func TestQuestionsFromSortsByID(t *testing.T) {
	qs := dn.QuestionsFrom(map[string]dn.Question{"b": dn.Truth("b"), "a": dn.Truth("a")})
	got, _ := json.Marshal(qs)
	if string(got) != `{"a":{"type":"truth","instructions":"a"},"b":{"type":"truth","instructions":"b"}}` {
		t.Fatalf("got %s", got)
	}
}

func TestRedirectsNeverFollowed(t *testing.T) {
	for _, hc := range []*http.Client{nil, {}} {
		srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if r.URL.Path == "/elsewhere" {
				respond(w, 200, truthBody, nil)
				return
			}
			respond(w, 307, `{"detail":{"error_type":"redirect","message":"Moved."}}`, map[string]string{"Location": "/elsewhere"})
		})
		var opts []dn.Option
		if hc != nil {
			opts = append(opts, dn.WithHTTPClient(hc))
		}
		_, err := client(t, srv.URL, opts...).Decide(context.Background(), dn.Request{Questions: oneTruth()})
		var e *dn.Error
		if !errors.As(err, &e) || e.Status != 307 || err.Error() != "Moved." {
			t.Fatalf("got %#v", err)
		}
		if rec.count() != 1 {
			t.Fatalf("expected 1 request, got %d", rec.count())
		}
	}
}

func TestNonJSONSuccessIsServerError(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `<html></html>`, map[string]string{"x-request-id": "req_h"})
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var se *dn.ServerError
	if !errors.As(err, &se) || se.Status != 200 || se.RequestID != "req_h" || se.Reason != "" {
		t.Fatalf("got %#v", err)
	}
}

func TestNonJSONSuccessReadsReason(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `<html></html>`, map[string]string{"x-request-id": "req_h", "x-decisionnode-error": "the body was cut short"})
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var se *dn.ServerError
	if !errors.As(err, &se) || se.Reason != "the body was cut short" {
		t.Fatalf("got %#v", err)
	}
}

func TestBaseErrorName(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 500, `{"detail":{"error_type":"api_error","message":"Internal error."}}`, nil)
	})
	_, err := client(t, srv.URL, dn.WithMaxRetries(0)).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var e *dn.DecisionNodeError
	if !errors.As(err, &e) || e.Status != 500 {
		t.Fatalf("got %#v", err)
	}
}

func TestHelperMalformedAnswerIsServerError(t *testing.T) {
	bodies := map[string]string{
		"check":    `{"model":"m","answers":{"check":{"type":"truth","truth":"0.9"}}}`,
		"missing":  `{"model":"m","answers":{}}`,
		"classify": `{"model":"m","answers":{"classify":{"type":"choice","choice":"a","confidence":0.5}}}`,
		"score":    `{"model":"m","answers":{"score":{"type":"score","score":1,"confidence":0.5,"probabilities":[0.5,0.5],"legend":{"0":"a","1":"b"}}}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
				respond(w, 200, body, map[string]string{"x-request-id": "req_m", "x-decisionnode-error": "could not be completed"})
			})
			c := client(t, srv.URL)
			ctx := context.Background()
			var err error
			switch name {
			case "check", "missing":
				_, err = c.Check(ctx, dn.CheckRequest{Instructions: "?"})
			case "classify":
				_, err = c.Classify(ctx, dn.ClassifyRequest{Instructions: "?", Options: []string{"a", "b"}})
			case "score":
				_, err = c.Score(ctx, dn.ScoreRequest{Instructions: "?", Levels: []string{"a", "b"}})
			}
			var se *dn.ServerError
			if !errors.As(err, &se) || se.Status != 200 || se.RequestID != "req_m" || se.Reason != "could not be completed" {
				t.Fatalf("got %#v", err)
			}
		})
	}
}

func TestDecideManyChecksEveryRequestFirst(t *testing.T) {
	gif := []byte("GIF89a\x01\x00\x01\x00")
	for name, bad := range map[string]dn.Request{
		"empty questions": {State: "c"},
		"unknown image":   {State: "c", Questions: oneTruth(), Images: []dn.Image{dn.ImageData("img", gif, "")}},
		"not encodable":   {State: make(chan int), Questions: oneTruth()},
	} {
		t.Run(name, func(t *testing.T) {
			srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
			reqs := []dn.Request{{State: "a", Questions: oneTruth()}, {State: "b", Questions: oneTruth()}, bad}
			res, err := client(t, srv.URL).DecideMany(context.Background(), reqs, 1)
			var apiErr *dn.Error
			// Spec 1.1: a request-content fault is the API's own class for
			// the whole call; argument problems stay plain errors.
			var bad *dn.BadRequestError
			if wantAPI := name == "unknown image"; err == nil || res != nil || errors.As(err, &apiErr) != wantAPI || (wantAPI && !errors.As(err, &bad)) {
				t.Fatalf("got %v, %v", res, err)
			}
			if rec.count() != 0 {
				t.Fatalf("%d requests were sent", rec.count())
			}
		})
	}
}

type otherTransport struct{}

func (otherTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(r)
}

func TestCallerHTTPClientMustBeInspectable(t *testing.T) {
	insecure := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	ownDialer := &http.Transport{DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("unused")
	}}
	refused := map[string]*http.Client{
		"unknown transport": {Transport: otherTransport{}},
		"skips TLS checks":  {Transport: insecure},
		"own TLS dialer":    {Transport: ownDialer},
	}
	for name, hc := range refused {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			c := dn.NewClient(dn.WithAPIKey("dn_test"), dn.WithHTTPClient(hc))
			var apiErr *dn.Error
			if c.Err() == nil || errors.As(c.Err(), &apiErr) {
				t.Fatalf("got %v", c.Err())
			}
			if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth()}); err != c.Err() {
				t.Fatalf("call returned %v", err)
			}
		})
	}
	accepted := map[string]*http.Client{
		"nil transport":  {},
		"http.Transport": {Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}},
	}
	for name, hc := range accepted {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			if err := dn.NewClient(dn.WithAPIKey("dn_test"), dn.WithHTTPClient(hc)).Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("skips TLS checks on local http", func(t *testing.T) {
		clearEnv(t)
		c := dn.NewClient(dn.WithAPIKey("dn_test"), dn.WithBaseURL("http://localhost:1"), dn.WithHTTPClient(&http.Client{Transport: insecure}))
		if c.Err() != nil {
			t.Fatal(c.Err())
		}
	})
}

func TestEmptyArgumentsFallBack(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, truthBody, nil)
	})
	clearEnv(t)
	t.Setenv("DECISIONNODE_API_KEY", "dn_env")
	t.Setenv("DECISIONNODE_BASE_URL", srv.URL)
	c := dn.NewClient(dn.WithAPIKey(""), dn.WithBaseURL(""), dn.WithModel(""))
	if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Model: ""}); err != nil {
		t.Fatal(err)
	}
	if h := rec.heads[0].Get("Authorization"); h != "Bearer dn_env" {
		t.Fatalf("authorization %q", h)
	}
	if !strings.Contains(string(rec.bodies[0]), `"model":"decisionnode-latest"`) {
		t.Fatalf("body %s", rec.bodies[0])
	}
}

func TestDecideManyConcurrencyBelowOne(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, truthBody, nil)
	})
	for _, n := range []int{0, -1} {
		results, err := client(t, srv.URL).DecideMany(context.Background(), []dn.Request{{Questions: oneTruth()}}, n)
		var apiErr *dn.Error
		if err == nil || errors.As(err, &apiErr) || results != nil {
			t.Fatalf("concurrency %d: got %v, %v", n, results, err)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("expected no request, got %d", rec.count())
	}
}

func TestMissingUsageCountsAreNil(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":4}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.InputTokens == nil || *d.Usage.InputTokens != 4 || d.Usage.OutputTokens != nil || d.LatencyMs != nil {
		t.Fatalf("got %+v", d)
	}
}

func TestBaseURLMustBeHTTPS(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	for _, u := range []string{"http://api.example.com", "http://10.0.0.1:8080", "ftp://example.com", "api.example.com", "http://"} {
		c := client(t, u)
		if c.Err() == nil {
			t.Errorf("%s: no configuration error", u)
		}
		_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth()})
		var base *dn.Error
		if err == nil || errors.As(err, &base) {
			t.Errorf("%s: expected an argument error, got %v", u, err)
		}
		if _, err := c.Models(context.Background()); err == nil {
			t.Errorf("%s: models: expected an argument error", u)
		}
		if _, err := c.DecideMany(context.Background(), []dn.Request{{Questions: oneTruth()}}, 1); err == nil {
			t.Errorf("%s: decide many: expected an argument error", u)
		}
	}
	clearEnv(t)
	t.Setenv("DECISIONNODE_BASE_URL", "http://api.example.com")
	if dn.NewClient(dn.WithAPIKey("k")).Err() == nil {
		t.Error("plain http from the environment was accepted")
	}
	for _, u := range []string{"https://api.example.com", "http://localhost:8080", "http://127.0.0.1", "http://[::1]:9000"} {
		if err := client(t, u).Err(); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()}); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 request, got %d", rec.count())
	}
}

func TestTimeoutAndRetriesArguments(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	for name, opt := range map[string]dn.Option{
		"zero timeout":     dn.WithTimeout(0),
		"negative timeout": dn.WithTimeout(-time.Second),
		"negative retries": dn.WithMaxRetries(-1),
	} {
		c := client(t, srv.URL, opt)
		_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth()})
		var base *dn.Error
		if c.Err() == nil || err == nil || errors.As(err, &base) {
			t.Errorf("%s: expected an argument error, got %v", name, err)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
	if err := client(t, srv.URL, dn.WithMaxRetries(0), dn.WithTimeout(time.Millisecond)).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestTimeoutCoversTheBody(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"m",`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	start := time.Now()
	_, err := client(t, srv.URL, dn.WithTimeout(100*time.Millisecond), dn.WithMaxRetries(0)).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var te *dn.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("the timeout did not cover the body: %s", time.Since(start))
	}
}

func TestMalformedKnownAnswerIsUnknown(t *testing.T) {
	body := `{"model":"m","answers":{` +
		`"t1":{"type":"truth"},` +
		`"t2":{"type":"truth","truth":"high"},` +
		`"c1":{"type":"choice","choice":"a","confidence":0.5},` +
		`"s1":{"type":"score","score":1,"confidence":0.5,"probabilities":{"x":0.5},"legend":{"0":"a"}},` +
		`"n":null,` +
		`"ok":{"type":"truth","truth":0.5}` +
		`},"usage":{"input_tokens":1,"output_tokens":0}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2", "c1", "s1", "n"} {
		if _, ok := d.Answer(id).(*dn.UnknownAnswer); !ok {
			t.Errorf("%s: got %#v", id, d.Answer(id))
		}
	}
	if u := d.Answer("t2").(*dn.UnknownAnswer); u.Type() != "truth" || u.Raw()["truth"] != "high" {
		t.Errorf("t2 raw: %#v", u.Raw())
	}
	if d.Truth("ok") == nil || d.Truth("ok").Truth != 0.5 {
		t.Errorf("ok: got %#v", d.Answer("ok"))
	}
}

func TestResponseHook(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			respond(w, 429, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, map[string]string{"retry-after-ms": "5"})
			return
		}
		respond(w, 200, truthBody, map[string]string{"x-request-id": "req_1"})
	})
	type key struct{}
	var mu sync.Mutex
	var got []dn.ResponseInfo
	var ctxValues []any
	c := client(t, srv.URL, dn.WithSleep(func(context.Context, time.Duration) error { return nil }),
		dn.WithResponseHook(func(ctx context.Context, r dn.ResponseInfo) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, r)
			ctxValues = append(ctxValues, ctx.Value(key{}))
			// Changing the copies must not change what the client returns.
			for i := range r.Body {
				r.Body[i] = 'x'
			}
			r.Header.Del("x-request-id")
		}))
	d, err := c.Decide(context.WithValue(context.Background(), key{}, "v"), dn.Request{State: 1, Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if d.RequestID != "req_1" || d.Truth("q") == nil {
		t.Fatalf("decision changed by the hook: %+v", d)
	}
	if len(got) != 2 {
		t.Fatalf("hook called %d times, want 2", len(got))
	}
	if got[0].Status != 429 || got[0].Attempt != 1 || got[1].Status != 200 || got[1].Attempt != 2 {
		t.Fatalf("statuses and attempts: %+v", got)
	}
	if got[0].Method != http.MethodPost || got[0].Path != "/v1/decide" {
		t.Fatalf("method and path: %s %s", got[0].Method, got[0].Path)
	}
	if got[0].Header.Get("retry-after-ms") != "5" {
		t.Fatalf("headers not passed: %v", got[0].Header)
	}
	for i, v := range ctxValues {
		if v != "v" {
			t.Fatalf("attempt %d: the hook's context lost the caller's values", i+1)
		}
	}
}
