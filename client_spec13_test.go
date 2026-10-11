package decisionnode_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const boostedBody = `{"model":"decisionnode-moe","answers":{"q":{"type":"truth","truth":0.97}},` +
	`"evidence":{"age":{"version":"age-v3","status":"ok","latency_ms":212,"billed_calls":2,"results":[` +
	`{"image":"a1b2","age_years":27,"low":22,"high":33,"confidence":0.81,"faces":1},{"image":"c3d4","faces":0}]},` +
	`"ai_generated":{"version":"ai_generated-v2","status":"ok","latency_ms":340,"results":[` +
	`{"image":"c3d4","generated":0.93,"confidence":0.86,"method":"inference","top_generator":{"name":"Generator A","probability":0.71},"c2pa_signer":null},` +
	`{"image":"e5f6","generated":0.99,"confidence":0.99,"method":"c2pa","top_generator":null,"c2pa_signer":"Example Camera Co."},` +
	`{"image":"g7h8","generated":0.04,"confidence":0.9,"method":"inference","top_generator":null,"c2pa_signer":null,"c2pa":"untrusted","region":{"x":4}}]},` +
	`"faces":{"version":"faces-v1","status":"ok","latency_ms":90,"results":[{"image":"a1b2","count":1}]},` +
	`"text":{"version":"text-v1","status":"timeout","latency_ms":800,"results":[]}},` +
	`"usage":{"input_tokens":341,"output_tokens":0,"booster_tokens":72,"boosters":3}}`

func TestBoostersWireForm(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	_, err := c.Decide(ctx, dn.Request{
		Questions: oneTruth(),
		Model:     "decisionnode-flash@acme/fraud:3",
		Boosters: []any{
			dn.BoosterAge,
			dn.Booster{Name: dn.BoosterAIGenerated, Images: []string{"c3d4"}, Require: dn.Ptr(true)},
			&dn.Booster{Name: "objects", Options: map[string]any{"min": 0.5}},
			map[string]any{"name": "text"},
			dn.Booster{Name: "faces", Require: dn.Ptr(false)},
			dn.Booster{Name: "objects_v2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"decisionnode-flash@acme/fraud:3","state":"","questions":{"q":{"type":"truth","instructions":"?"}},` +
		`"boosters":["age",{"name":"ai_generated","images":["c3d4"],"require":true},{"name":"objects","options":{"min":0.5}},{"name":"text"},{"name":"faces","require":false},"objects_v2"]}`
	if got := string(rec.bodies[0]); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	// No boosters, or an empty list: no boosters key, byte for byte.
	for _, b := range [][]any{nil, {}} {
		if _, err := c.Decide(ctx, dn.Request{Questions: oneTruth(), Boosters: b}); err != nil {
			t.Fatal(err)
		}
	}
	plain := `{"model":"decisionnode-moe","state":"","questions":{"q":{"type":"truth","instructions":"?"}}}`
	if string(rec.bodies[1]) != plain || string(rec.bodies[2]) != plain {
		t.Fatalf("no boosters: %s / %s", rec.bodies[1], rec.bodies[2])
	}
	// The helpers and batch lines carry them as Decide does.
	if _, err := c.Check(ctx, dn.CheckRequest{Instructions: "?", Boosters: []any{"age"}}); err == nil {
		t.Fatal("the stub's answer is under q, so Check must report it malformed")
	}
	if got := string(rec.bodies[3]); !strings.HasSuffix(got, `,"boosters":["age"]}`) {
		t.Fatalf("check: %s", got)
	}
	if _, err := c.Gate(ctx, dn.GateRequest{Action: "rm -rf /", Boosters: []any{"age"}}); err != nil {
		t.Fatal(err)
	}
	if got := string(rec.bodies[4]); !strings.HasSuffix(got, `,"boosters":["age"]}`) {
		t.Fatalf("gate: %s", got)
	}
	if err := c.CheckBatchCreate([]dn.BatchRequest{{CustomID: dn.Ptr("l1"), Request: dn.Request{Questions: oneTruth(), Boosters: []any{"age"}}}}); err != nil {
		t.Fatal(err)
	}
	// A nil *Booster is the caller's mistake, found before sending.
	if _, err := c.Decide(ctx, dn.Request{Questions: oneTruth(), Boosters: []any{(*dn.Booster)(nil)}}); err == nil || rec.count() != 5 {
		t.Fatalf("nil booster: %v", err)
	}
}

func TestBoosterOptionsCountTowardNesting(t *testing.T) {
	c := client(t, "https://api.example.com")
	deep := map[string]any{}
	cur := deep
	// body, boosters, the booster, options: 4, then 125 more objects.
	for i := 0; i < 124; i++ {
		next := map[string]any{}
		cur["x"] = next
		cur = next
	}
	req := dn.Request{Questions: oneTruth(), Boosters: []any{dn.Booster{Name: "age", Options: deep}}}
	if err := c.CheckDecide(req); err != nil {
		t.Fatalf("128 levels: %v", err)
	}
	cur["x"] = map[string]any{}
	err := c.CheckDecide(req)
	var bad *dn.BadRequestError
	if !errors.As(err, &bad) || bad.ErrorType != "api_usage_error" || bad.Error() != "Invalid request." || !strings.Contains(bad.Reason, "129") {
		t.Fatalf("129 levels: %v", err)
	}
}

func TestEvidenceAndBoosterUsage(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, boostedBody, map[string]string{"x-decisionnode-boosters": "age=ok:212,ai_generated=ok:340"})
	})
	c := client(t, srv.URL)
	d, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{"age", "ai_generated"}})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Usage.BoosterTokens != 72 || *d.Usage.Boosters != 3 {
		t.Fatalf("usage: %+v", d.Usage)
	}
	age := d.Evidence.Get("age")
	if age.Version != "age-v3" || age.Status != "ok" || *age.LatencyMs != 212 || *age.BilledCalls != 2 || len(age.Results) != 2 || d.Evidence.Get("text").BilledCalls != nil {
		t.Fatalf("age: %+v", age)
	}
	read := age.Results[0].(*dn.AgeReading)
	if read.Image != "a1b2" || read.Faces != 1 || *read.AgeYears != 27 || *read.Low != 22 || *read.High != 33 || *read.Confidence != 0.81 {
		t.Fatalf("age reading: %+v", read)
	}
	none := age.Results[1].(*dn.AgeReading)
	if none.Faces != 0 || none.AgeYears != nil || none.Confidence != nil || none.Raw()["image"] != "c3d4" {
		t.Fatalf("no face: %+v", none)
	}
	gen := d.Evidence.Get("ai_generated").Results
	inf := gen[0].(*dn.AIGeneratedReading)
	if inf.Generated != 0.93 || inf.Method != "inference" || inf.TopGenerator.Name != "Generator A" || inf.TopGenerator.Probability != 0.71 || inf.C2PASigner != nil {
		t.Fatalf("inference reading: %+v", inf)
	}
	c2pa := gen[1].(*dn.AIGeneratedReading)
	if c2pa.Method != "c2pa" || c2pa.TopGenerator != nil || *c2pa.C2PASigner != "Example Camera Co." {
		t.Fatalf("c2pa reading: %+v", c2pa)
	}
	untrusted := gen[2].(*dn.AIGeneratedReading)
	if *untrusted.C2PA != "untrusted" || untrusted.TopGenerator != nil || untrusted.C2PASigner != nil || inf.C2PA != nil {
		t.Fatalf("untrusted reading: %+v", untrusted)
	}
	// A booster this version does not know: a generic reading.
	if u, ok := d.Evidence.Get("faces").Results[0].(*dn.UnknownReading); !ok || u.Raw()["count"] != 1.0 {
		t.Fatalf("unknown booster: %#v", d.Evidence.Get("faces").Results[0])
	}
	var order []string
	for _, e := range d.Evidence {
		order = append(order, e.Booster)
	}
	if strings.Join(order, ",") != "age,ai_generated,faces,text" || d.Evidence.Get("missing") != nil {
		t.Fatalf("order: %v", order)
	}
	text := d.Evidence.Get("text")
	if text.Status != "timeout" || text.Results == nil || len(text.Results) != 0 {
		t.Fatalf("timeout: %+v", text)
	}
	if got := dn.ParseBoosters(d.Header); len(got) != 2 || got[1].Name != "ai_generated" || got[1].Status != "ok" || *got[1].LatencyMs != 340 {
		t.Fatalf("header: %+v", got)
	}
}

func TestMalformedReadingsAreGeneric(t *testing.T) {
	cases := []string{
		`{"image":"a","faces":1}`,   // a face but no age
		`{"image":"a","faces":"1"}`, // faces of the wrong type
		`{"faces":0}`,               // no image
		`{"image":"a","faces":1.5}`, // not a count
		`{"image":"a","faces":1,"age_years":"27","low":22,"high":33,"confidence":0.8}`,
	}
	for _, reading := range cases {
		body := `{"model":"m","answers":{},"evidence":{"age":{"version":"age-v3","status":"ok","latency_ms":1,"results":[` + reading + `]}}}`
		srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
		d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Evidence.Get("age").Results[0].(*dn.UnknownReading); !ok {
			t.Fatalf("%s: %#v", reading, d.Evidence.Get("age").Results[0])
		}
	}
	for _, reading := range []string{
		`{"image":"a","generated":0.9,"confidence":0.9}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"inference","top_generator":{"name":"x"}}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"c2pa","top_generator":null,"c2pa_signer":7}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"inference","c2pa_signer":null}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"inference","top_generator":null}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"inference","top_generator":"x","c2pa_signer":null}`,
		`{"image":"a","generated":0.9,"confidence":0.9,"method":"inference","top_generator":null,"c2pa_signer":null,"c2pa":true}`,
	} {
		body := `{"model":"m","answers":{},"evidence":{"ai_generated":{"status":"ok","results":[` + reading + `]}}}`
		srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
		d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Evidence.Get("ai_generated").Results[0].(*dn.UnknownReading); !ok {
			t.Fatalf("%s: %#v", reading, d.Evidence.Get("ai_generated").Results[0])
		}
		if d.Evidence.Get("ai_generated").LatencyMs != nil {
			t.Fatal("a missing latency is nil")
		}
	}
}

func TestNoBoostersNoEvidence(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":1,"output_tokens":0}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Evidence != nil || d.Usage.BoosterTokens != nil || d.Usage.Boosters != nil || dn.ParseBoosters(d.Header) != nil {
		t.Fatalf("decision: %+v", d)
	}
}

func TestBoosterErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		check  func(err error) bool
	}{
		{400, `{"detail":{"error_type":"booster_unknown","message":"Unknown booster: agee"}}`, func(err error) bool {
			var e *dn.BadRequestError
			return errors.As(err, &e) && e.ErrorType == "booster_unknown" && e.Error() == "Unknown booster: agee"
		}},
		{400, `{"detail":{"error_type":"booster_input_mismatch","message":"The booster 'age' needs at least one image."}}`, func(err error) bool {
			var e *dn.BadRequestError
			return errors.As(err, &e) && e.ErrorType == "booster_input_mismatch"
		}},
		{403, `{"detail":{"error_type":"booster_not_enabled","message":"The booster 'age' is not enabled for this workspace."}}`, func(err error) bool {
			var e *dn.PermissionDeniedError
			var r *dn.RefusalError
			return errors.As(err, &e) && !errors.As(err, &r) && e.ErrorType == "booster_not_enabled"
		}},
		{403, `{"detail":{"error_type":"workspace_suspended","message":"suspended"}}`, func(err error) bool {
			var e *dn.PermissionDeniedError
			return errors.As(err, &e)
		}},
	}
	for _, tc := range cases {
		srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
			respond(w, tc.status, tc.body, map[string]string{"x-decisionnode-boosters": "age=error:3"})
		})
		_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{"age"}})
		if !tc.check(err) || rec.count() != 1 {
			t.Fatalf("%d %s: %v after %d attempts", tc.status, tc.body, err, rec.count())
		}
		var base *dn.Error
		if !errors.As(err, &base) || len(base.Boosters) != 1 || base.Boosters[0].Status != "error" {
			t.Fatalf("boosters on the error: %+v", base)
		}
	}
}

func TestBoosterUnavailableIsRetried(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 529, `{"detail":{"error_type":"booster_unavailable","message":"The booster 'age' did not answer in time."}}`,
			map[string]string{"x-decisionnode-boosters": "age=timeout:800, ai_generated=ok:120", "retry-after-ms": "5"})
	})
	var waits []time.Duration
	c := client(t, srv.URL, dn.WithSleep(func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }))
	_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{dn.Booster{Name: "age", Require: dn.Ptr(true)}, "ai_generated"}})
	var over *dn.OverloadedError
	if !errors.As(err, &over) || over.ErrorType != "booster_unavailable" || rec.count() != 3 || len(waits) != 2 {
		t.Fatalf("%v after %d attempts", err, rec.count())
	}
	b := over.Boosters
	if len(b) != 2 || b[0].Name != "age" || b[0].Status != "timeout" || *b[0].LatencyMs != 800 || b[1].Name != "ai_generated" || *b[1].LatencyMs != 120 {
		t.Fatalf("boosters: %+v", b)
	}
}

func TestFineTunedModels(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/v1/models" {
			respond(w, 200, `{"models":[`+
				`{"name":"decisionnode-flash","version":"decisionnode-flash","max_images":16},`+
				`{"name":"decisionnode-flash@acme/fraud:3","version":"decisionnode-flash@acme/fraud:3","description":"Claims fraud","release_date":"2026-10-09","fine_tuned":true,"base":"decisionnode-flash","adapter_version":3,"status":"deployed","max_images":16},`+
				`{"name":"decisionnode-flash@acme/fraud:4","version":"decisionnode-flash@acme/fraud:4","fine_tuned":true,"base":"decisionnode-flash","adapter_version":4,"status":"ready"}]}`, nil)
			return
		}
		respond(w, 200, `{"model":"decisionnode-flash@acme/fraud:3","answers":{"q":{"type":"truth","truth":0.5}}}`, nil)
	})
	c := client(t, srv.URL)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cat, deployed, ready := models[0], models[1], models[2]
	if cat.FineTuned != nil || cat.Base != "" || cat.AdapterVersion != nil || cat.Status != "" {
		t.Fatalf("catalogue entry: %+v", cat)
	}
	if !*deployed.FineTuned || deployed.Base != "decisionnode-flash" || *deployed.AdapterVersion != 3 || deployed.Status != "deployed" ||
		deployed.Version != "decisionnode-flash@acme/fraud:3" || deployed.Description != "Claims fraud" {
		t.Fatalf("deployed entry: %+v", deployed)
	}
	if ready.Status != "ready" || *ready.AdapterVersion != 4 {
		t.Fatalf("ready entry: %+v", ready)
	}
	d, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Model: "decisionnode-flash@acme/fraud"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != "decisionnode-flash@acme/fraud:3" || !strings.Contains(string(rec.bodies[1]), `"model":"decisionnode-flash@acme/fraud"`) {
		t.Fatalf("model: %s, sent %s", d.Model, rec.bodies[1])
	}
}

func TestSpecVersion13(t *testing.T) {
	if dn.SpecVersion != "1.3" {
		t.Fatal(dn.SpecVersion)
	}
}

func TestBodyLimit(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}}}`, nil)
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	overhead := len(`{"model":"decisionnode-moe","state":"","questions":{"q":{"type":"truth","instructions":"?"}}}`)
	at := strings.Repeat("x", dn.MaxBodyBytes-overhead)
	if _, err := c.Decide(ctx, dn.Request{State: at, Questions: oneTruth()}); err != nil {
		t.Fatal(err)
	}
	if n := len(rec.bodies[0]); n != dn.MaxBodyBytes {
		t.Fatalf("sent %d bytes", n)
	}
	over := at + "x"
	check := func(err error, what string) {
		t.Helper()
		var big *dn.RequestTooLargeError
		if !errors.As(err, &big) || big.Status != 0 || big.RequestID != "" || big.ErrorType != "request_too_large_error" ||
			!strings.HasPrefix(big.Error(), "The request body is over 26,214,400 bytes (25 MiB).") {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err := c.Decide(ctx, dn.Request{State: over, Questions: oneTruth()})
	check(err, "decide")
	check(c.CheckDecide(dn.Request{State: over, Questions: oneTruth()}), "check")
	_, err = c.DecideMany(ctx, []dn.Request{{Questions: oneTruth()}, {State: over, Questions: oneTruth()}}, 2)
	check(err, "decide many")
	_, err = c.Batches().Create(ctx, []dn.BatchRequest{{CustomID: dn.Ptr("a"), Request: dn.Request{State: over, Questions: oneTruth()}}})
	check(err, "batch line")
	if rec.count() != 1 {
		t.Fatalf("%d requests sent", rec.count())
	}

	// The API's 413 is the same error, never retried.
	big, rec413 := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 413, `{"detail":{"error_type":"request_too_large_error","message":"The request body is over 26,214,400 bytes (25 MiB)."}}`, map[string]string{"x-request-id": "req_413"})
	})
	_, err = client(t, big.URL).Decide(ctx, dn.Request{Questions: oneTruth()})
	var tooLarge *dn.RequestTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Status != 413 || tooLarge.RequestID != "req_413" || rec413.count() != 1 {
		t.Fatalf("413: %v after %d attempts", err, rec413.count())
	}
}

// The body is measured as it will go out, before anything is uploaded: an
// inline image by its base64, an image to upload by its upload id.
func TestBodyLimitCountsImagesAsSent(t *testing.T) {
	for _, threshold := range []int{dn.DefaultInlineThreshold, 0} {
		s := newStorageServer(t)
		c := client(t, s.srv.URL, dn.WithInlineThreshold(threshold))
		ctx := context.Background()
		png := fixture(t, "3x2.png")
		req := func(state string) dn.Request {
			return dn.Request{State: state, Questions: oneTruth(), Images: []dn.Image{dn.ImageData("scan", png, "")}}
		}
		if _, err := c.Decide(ctx, req("")); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		overhead := len(s.decides[0])
		presigns := s.presigns
		s.mu.Unlock()
		// A cached upload goes by the same id length, so a fresh client keeps
		// the count exact for the uploaded case.
		c = client(t, s.srv.URL, dn.WithInlineThreshold(threshold))
		over := strings.Repeat("x", dn.MaxBodyBytes-overhead+1)
		var big *dn.RequestTooLargeError
		if _, err := c.Decide(ctx, req(over)); !errors.As(err, &big) {
			t.Fatalf("threshold %d, one byte over: %v", threshold, err)
		}
		if err := c.CheckDecide(req(over)); !errors.As(err, &big) {
			t.Fatalf("threshold %d, check: %v", threshold, err)
		}
		s.mu.Lock()
		if len(s.decides) != 1 || s.presigns != presigns {
			t.Fatalf("threshold %d: sent or uploaded before refusing", threshold)
		}
		s.mu.Unlock()
		if _, err := c.Decide(ctx, req(over[1:])); err != nil {
			t.Fatalf("threshold %d, at the limit: %v", threshold, err)
		}
		s.mu.Lock()
		if n := len(s.decides[1]); n != dn.MaxBodyBytes {
			t.Fatalf("threshold %d: sent %d bytes", threshold, n)
		}
		s.mu.Unlock()
	}
}

func TestFallback(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		h := map[string]string{"x-decisionnode-model": "decisionnode-flash"}
		if n == 1 {
			h["x-decisionnode-fallback"] = "decisionnode-flash"
			h["x-decisionnode-requested-model"] = "decisionnode-moe"
		}
		respond(w, 200, `{"model":"decisionnode-flash","answers":{"check":{"type":"truth","truth":0.5}}}`, h)
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	d, err := c.Decide(ctx, dn.Request{Questions: oneTruth(), Model: "decisionnode-moe"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != "decisionnode-flash" || d.Fallback == nil || d.Fallback.ServedModel != "decisionnode-flash" || d.Fallback.RequestedModel != "decisionnode-moe" {
		t.Fatalf("fallback answer: %+v %+v", d, d.Fallback)
	}
	if strings.Contains(string(rec.bodies[0]), "fallback") {
		t.Fatalf("no fallback key when left out: %s", rec.bodies[0])
	}
	if _, err := c.Check(ctx, dn.CheckRequest{Instructions: "?", Fallback: dn.Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(rec.bodies[1]), `,"fallback":false}`) {
		t.Fatalf("check body: %s", rec.bodies[1])
	}
	d, err = c.Decide(ctx, dn.Request{Questions: oneTruth(), Fallback: dn.Ptr(true)})
	if err != nil || d.Fallback != nil || !strings.HasSuffix(string(rec.bodies[2]), `,"fallback":true}`) {
		t.Fatalf("ordinary answer: %v %+v %s", err, d, rec.bodies[2])
	}
}

// An error body that is not JSON is classed by its status alone, with the
// client's own words and no field read from the body.
func TestErrorBodyNotJSON(t *testing.T) {
	cases := []struct {
		status  int
		id      string
		check   func(error) bool
		message string
	}{
		{413, "", func(err error) bool { var e *dn.RequestTooLargeError; return errors.As(err, &e) }, "HTTP 413 Content Too Large"},
		{400, "req_1", func(err error) bool {
			var e *dn.BadRequestError
			var c *dn.ContextLimitError
			return errors.As(err, &e) && !errors.As(err, &c)
		}, "HTTP 400 Bad Request (request id req_1)"},
		{403, "", func(err error) bool {
			var e *dn.PermissionDeniedError
			var r *dn.RefusalError
			return errors.As(err, &e) && !errors.As(err, &r)
		}, "HTTP 403 Forbidden"},
		{422, "", func(err error) bool { var e *dn.ValidationError; return errors.As(err, &e) && e.Entries == nil }, "HTTP 422 Unprocessable Content"},
		{418, "req_2", func(err error) bool { _, ok := err.(*dn.Error); return ok }, "HTTP 418 (request id req_2)"},
	}
	for _, tc := range cases {
		for _, body := range []string{"<html><body>busy</body></html>", ""} {
			srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if tc.id != "" {
					w.Header().Set("x-request-id", tc.id)
				}
				w.Header().Set("x-decisionnode-error", "from the edge")
				w.Header().Set("content-type", "text/html")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(body))
			})
			_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
			var base *dn.Error
			if !tc.check(err) || !errors.As(err, &base) || err.Error() != tc.message || base.ErrorType != "" ||
				base.RequestID != tc.id || base.Reason != "from the edge" || base.Body != nil || rec.count() != 1 {
				t.Fatalf("%d %q: %T %q after %d attempts", tc.status, body, err, err, rec.count())
			}
		}
	}
	// A JSON envelope without a message keeps "".
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 400, `{}`, nil) })
	if _, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()}); err == nil || err.Error() != "" {
		t.Fatalf("empty envelope: %q", err)
	}
}

func TestImageIDOfAtMost64Characters(t *testing.T) {
	c := client(t, "https://api.example.com")
	url := "https://files.example.com/a.jpg"
	for _, id := range []string{strings.Repeat("a", 64), strings.Repeat("\U00020000", 64)} {
		if err := c.CheckDecide(dn.Request{Questions: oneTruth(), Images: []dn.Image{dn.ImageURL(id, url)}}); err != nil {
			t.Fatalf("64 code points: %v", err)
		}
	}
	long := strings.Repeat("a", 65)
	for _, imgs := range [][]dn.Image{
		{dn.ImageURL("first", url), dn.ImageURL(long, url)},
		{dn.ImageURL(long, url), dn.ImageURL(long, url)}, // too long before it is a duplicate
	} {
		err := c.CheckDecide(dn.Request{Questions: oneTruth(), Images: imgs})
		var bad *dn.BadRequestError
		want := "Image 1: its id is longer than 64 characters."
		if imgs[0].ID() == long {
			want = "Image 0: its id is longer than 64 characters."
		}
		if !errors.As(err, &bad) || bad.ErrorType != "api_usage_error" || bad.Error() != want {
			t.Fatalf("65 code points: %v", err)
		}
	}
}

func TestUsageCost(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		cost := `,"cost":{"usd":0.036014322,"model_usd":0.000014322,"boosters_usd":0.036,"free_tier":false}`
		if n == 2 {
			cost = ""
		}
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":341,"output_tokens":0`+cost+`}}`, nil)
	})
	c := client(t, srv.URL)
	d, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	// The whole cost as sent, not the sum of its parts (0.036014321999999994).
	if cost := d.Usage.Cost; cost == nil || cost.USD != 0.036014322 || cost.ModelUSD != 0.000014322 || cost.BoostersUSD != 0.036 || cost.FreeTier {
		t.Fatalf("cost: %+v", d.Usage.Cost)
	}
	if d, err = c.Decide(context.Background(), dn.Request{Questions: oneTruth()}); err != nil || d.Usage.Cost != nil {
		t.Fatalf("absent cost: %v %+v", err, d.Usage.Cost)
	}
}

func TestNewBoosterOptions(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 400, `{"detail":{"error_type":"booster_unknown","message":"Unknown booster: ocr"}}`, nil)
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{
		dn.BoosterFaceDetection,
		dn.OCRBooster("What is the total on the receipt?"),
		dn.WantedPersonBooster(0.9),
	}})
	var bad *dn.BadRequestError
	if !errors.As(err, &bad) || bad.ErrorType != "booster_unknown" {
		t.Fatalf("the server's answer: %v", err)
	}
	if got := string(rec.bodies[0]); !strings.HasSuffix(got, `,"boosters":["face_detection",{"name":"ocr","options":{"prompt":"What is the total on the receipt?"}},{"name":"wanted_person","options":{"threshold":0.9}}]}`) {
		t.Fatalf("body: %s", got)
	}
}

func TestOCRReading(t *testing.T) {
	body := `{"model":"m","answers":{},"evidence":{"ocr":{"version":"ocr-v1","status":"ok","latency_ms":420,"results":[` +
		`{"image":"receipt","text":"Total 740.00 SEK","truncated":false},{"image":"blank","text":"","truncated":false},{"image":"form","text":"{}","truncated":true},` +
		`{"image":"a","text":740,"truncated":false},{"image":"a","text":"x","truncated":"yes"},{"image":"a","text":"x"},{"image":"a","truncated":false}]}}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	rs := d.Evidence.Get("ocr").Results
	read, ok := rs[0].(*dn.OCRReading)
	if !ok || read.Image != "receipt" || read.Text != "Total 740.00 SEK" || read.Truncated {
		t.Fatalf("reading: %#v", rs[0])
	}
	if blank, ok := rs[1].(*dn.OCRReading); !ok || blank.Text != "" {
		t.Fatalf("an empty text is a reading: %#v", rs[1])
	}
	if cut, ok := rs[2].(*dn.OCRReading); !ok || !cut.Truncated {
		t.Fatalf("truncated: %#v", rs[2])
	}
	for _, r := range rs[3:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("malformed: %#v", r)
		}
	}
}

func TestFaceReadings(t *testing.T) {
	body := `{"model":"m","answers":{},"evidence":{` +
		`"face_detection":{"status":"ok","results":[{"image":"gate","faces":70,"detections":[{"box":{"x1":0.3219,"y1":0.2611,"x2":0.3797,"y2":0.3806},"confidence":0.97}]},` +
		`{"image":"gate","faces":1,"detections":[{"box":{"x1":1,"y1":2,"x2":3},"confidence":0.5}]},{"image":"gate","faces":1}]},` +
		`"wanted_person":{"status":"ok","results":[{"image":"gate","faces":2,"matches":[{"box":{"x1":412,"y1":188,"x2":486,"y2":274},"score":0.93,"reference":"wl_1:entry-0192"}]},` +
		`{"image":"gate","faces":1,"matches":[{"box":{"x1":1,"y1":2,"x2":3,"y2":4},"score":0.9,"reference":192}]}]}}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, body, nil) })
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	faces := d.Evidence.Get("face_detection").Results
	fd, ok := faces[0].(*dn.FaceDetectionReading)
	if !ok || fd.Faces != 70 || len(fd.Detections) != 1 || fd.Detections[0].Box.X1 != 0.3219 || fd.Detections[0].Confidence != 0.97 {
		t.Fatalf("face detection: %#v", faces[0])
	}
	for _, r := range faces[1:] {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("malformed: %#v", r)
		}
	}
	wanted := d.Evidence.Get("wanted_person").Results
	wp, ok := wanted[0].(*dn.WantedPersonReading)
	if !ok || wp.Faces != 2 || len(wp.Matches) != 1 || wp.Matches[0].Reference != "wl_1:entry-0192" || wp.Matches[0].Box.Y2 != 274 {
		t.Fatalf("wanted person: %#v", wanted[0])
	}
	if _, ok := wanted[1].(*dn.UnknownReading); !ok {
		t.Fatalf("a reference that is not a string: %#v", wanted[1])
	}
}

// A request naming ocr is never retried after a timeout; its other retries
// stay, and a request without ocr is retried as before.
func TestOCRNotRetriedAfterTimeout(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		time.Sleep(300 * time.Millisecond)
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}}}`, nil)
	})
	c := client(t, srv.URL, dn.WithTimeout(100*time.Millisecond), dn.WithSleep(func(ctx context.Context, d time.Duration) error { return nil }))
	for _, boosters := range [][]any{{"ocr"}, {dn.OCRBooster("What is the total?")}} {
		before := rec.count()
		_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: boosters})
		var te *dn.TimeoutError
		if !errors.As(err, &te) || rec.count()-before != 1 {
			t.Fatalf("ocr %v: %v after %d attempts", boosters, err, rec.count()-before)
		}
	}
	before := rec.count()
	_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{"age"}})
	var te *dn.TimeoutError
	if !errors.As(err, &te) || rec.count()-before != 3 {
		t.Fatalf("without ocr: %v after %d attempts", err, rec.count()-before)
	}
	busy, recBusy := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 529, `{"detail":{"error_type":"overloaded_error","message":"busy"}}`, map[string]string{"retry-after-ms": "1"})
	})
	c = client(t, busy.URL, dn.WithSleep(func(ctx context.Context, d time.Duration) error { return nil }))
	if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Boosters: []any{"ocr"}}); err == nil || recBusy.count() != 3 {
		t.Fatalf("a 529 is still retried: %v after %d attempts", err, recBusy.count())
	}
	if dn.OCRTimeout != 30*time.Second {
		t.Fatal(dn.OCRTimeout)
	}
}
