package decisionnode_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const readyItem = `{"id":"itm_1","reference":"policy","title":"Claims policy","format":"markdown","pinned":true,"pin_rank":0,"source":"article",` +
	`"file":null,"status":"ready","stage":null,"progress":null,"serving":"current","error":null,"delayed":false,"chunks":3,"tokens":420,` +
	`"content_sha256":"ab","version":1,"queued_at":"2026-10-20T09:00:00Z","ready_at":"2026-10-20T09:00:02Z","metadata":{"mission":"M-42"}}`

func TestKnowledgeDocuments(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	staleOnce := true
	var srvURL string
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		stale := false
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["upload_id"] == "upl_1" && staleOnce {
				staleOnce, stale = false, true
			}
		}
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/collections":
			respond(w, 201, `{"id":"col_1","type":"text","name":"Policies","public":false,"items":0,"scope_key":"mission","chunks":0,"tokens":0,"version":0,`+
				`"status_counts":{"queued":0,"processing":0,"ready":0,"failed":0},"pinned":{"tokens":0,"cap":4096,"largest_scope":null}}`, nil)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/uploads":
			id := "upl_1"
			if !staleOnce {
				id = "upl_2"
			}
			respond(w, 200, `{"uploads":[{"upload_id":"`+id+`","purpose":"knowledge","url":"`+srvURL+`/storage/`+id+`","method":"PUT",`+
				`"headers":{"Content-Type":"application/pdf"},"expires_at":"2099-01-01T00:00:00Z"}],"expires_in":300}`, nil)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/storage/"):
			w.WriteHeader(200)
		case stale:
			respond(w, 400, `{"detail":{"error_type":"upload_expired","message":"Upload upl_1 has expired."}}`, nil)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			respond(w, 202, `{"id":"itm_1","status":"queued","warnings":["possible_secret"],"file":{"filename":"claims.pdf","content_type":"application/pdf","byte_size":9,"pages":null,"original_held":true}}`, nil)
		default:
			respond(w, 404, `{"detail":{"error_type":"collection_not_found","message":"Collection not found: x."}}`, nil)
		}
	})
	srvURL = srv.URL
	c := client(t, srv.URL)
	ctx := context.Background()
	cols := c.Collections()

	col, err := cols.CreateWith(ctx, dn.CollectionCreate{Name: "Policies", Type: dn.CollectionText, ScopeKey: dn.Ptr("mission")})
	if err != nil || col.ScopeKey != "mission" || col.StatusCounts == nil || col.Pinned == nil || col.Pinned.Cap != 4096 || col.Pinned.LargestScope != nil {
		t.Fatalf("create: %#v %v", col, err)
	}
	if got := string(rec.bodies[0]); got != `{"type":"text","name":"Policies","scope_key":"mission"}` {
		t.Fatalf("create body: %s", got)
	}

	// Markdown inline, with every field and wait.
	_, err = cols.AddDocument(ctx, "col_1", dn.DocumentText("# Receipts\nAlways.", ""), dn.DocumentOptions{
		Title: dn.Ptr("Claims policy"), Reference: dn.Ptr("policy"), Pinned: dn.Ptr(true), PinRank: dn.Ptr(0),
		Metadata: map[string]any{"mission": "M-42"}, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(rec.bodies[1]); got != `{"title":"Claims policy","text":"# Receipts\nAlways.","reference":"policy","pinned":true,"pin_rank":0,"metadata":{"mission":"M-42"}}` {
		t.Fatalf("inline body: %s", got)
	}
	if paths[1] != "POST /v1/collections/col_1/items?wait=true" || rec.heads[1].Get("Idempotency-Key") == "" {
		t.Fatalf("inline: %s %v", paths[1], rec.heads[1])
	}

	// A small .md file goes inline, titled by its stem.
	if _, err = cols.AddDocument(ctx, "col_1", dn.DocumentData([]byte("Hello."), "", "brief.v2.md"), dn.DocumentOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := string(rec.bodies[2]); got != `{"title":"brief.v2","text":"Hello.","format":"markdown"}` {
		t.Fatalf("md file body: %s", got)
	}

	// A PDF is uploaded; the first reference goes stale and is uploaded again
	// under a new key.
	item, err := cols.AddDocument(ctx, "col_1", dn.DocumentData([]byte("%PDF-1.7\n"), "", ""), dn.DocumentOptions{})
	if err != nil || item.Status != "queued" || item.File == nil || !item.File.OriginalHeld || len(item.Warnings) != 1 {
		t.Fatalf("pdf: %#v %v", item, err)
	}
	var presign map[string]any
	_ = json.Unmarshal(rec.bodies[3], &presign)
	file := presign["files"].([]any)[0].(map[string]any)
	if file["purpose"] != "knowledge" || file["content_type"] != "application/pdf" || file["byte_size"] != 9.0 || file["filename"] != nil {
		t.Fatalf("presign: %s", rec.bodies[3])
	}
	want := []string{"POST /v1/uploads", "PUT /storage/upl_1", "POST /v1/collections/col_1/items", "POST /v1/uploads", "PUT /storage/upl_2", "POST /v1/collections/col_1/items"}
	if got := paths[3:]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pdf calls: %v", got)
	}
	if got := string(rec.bodies[8]); got != `{"upload_id":"upl_2"}` {
		t.Fatalf("pdf item body: %s", got)
	}
	if rec.heads[5].Get("Idempotency-Key") == rec.heads[8].Get("Idempotency-Key") || rec.heads[4].Get("Authorization") != "" {
		t.Fatal("the resend reused its key, or the PUT carried the API key")
	}
}

func TestDocumentsCheckedLocally(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 500, `{}`, nil) })
	c := client(t, srv.URL)
	ctx := context.Background()
	cols := c.Collections()
	text := dn.DocumentText("Rule.", "")
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{second(cols.CreateWith(ctx, dn.CollectionCreate{Name: "K", Type: "text", ScopeKey: dn.Ptr("Mission")})), "string_pattern_mismatch"},
		{second(cols.AddDocument(ctx, "col_1", text, dn.DocumentOptions{})), "missing"},
		{second(cols.AddDocument(ctx, "col_1", text, dn.DocumentOptions{Title: dn.Ptr("  ")})), "string_too_long"},
		{second(cols.AddDocument(ctx, "col_1", text, dn.DocumentOptions{Title: dn.Ptr("T"), PinRank: dn.Ptr(1001)})), "int_range"},
		{second(cols.AddDocument(ctx, "col_1", dn.DocumentText("x", "html"), dn.DocumentOptions{Title: dn.Ptr("T")})), "literal_error"},
		{second(cols.AddDocument(ctx, "col_1", dn.DocumentText("", ""), dn.DocumentOptions{Title: dn.Ptr("T")})), "string_too_short"},
		{second(cols.AddDocument(ctx, "col_1", dn.DocumentData([]byte("hi"), "text/plain", ""), dn.DocumentOptions{})), "missing"},
		{second(cols.UpdateItem(ctx, "col_1", "itm_1", dn.ItemUpdate{Title: dn.Ptr(strings.Repeat("t", 201))})), "string_too_long"},
	} {
		var ve *dn.ValidationError
		if !errors.As(tc.err, &ve) {
			t.Fatalf("%s: %v", tc.kind, tc.err)
		}
		if e, _ := ve.Entries[0].(map[string]any); e["type"] != tc.kind {
			t.Fatalf("%s: %#v", tc.kind, ve.Entries)
		}
	}
	for _, tc := range []struct {
		doc dn.Document
		msg string
	}{
		{dn.DocumentData(nil, "application/pdf", "a.pdf"), "files[0].byte_size: must be an integer from 1 to 52428800."},
		{dn.DocumentData(make([]byte, dn.MaxDocumentBytes+1), "application/pdf", ""), "files[0].byte_size: 52428801 is over the limit of 52428800 bytes."},
		{dn.DocumentData([]byte("x"), "application/zip", "a.zip"), "files[0].content_type: must be one of"},
	} {
		_, err := cols.AddDocument(ctx, "col_1", tc.doc, dn.DocumentOptions{})
		var br *dn.BadRequestError
		if !errors.As(err, &br) || br.ErrorType != "api_usage_error" || !strings.HasPrefix(br.Error(), tc.msg) {
			t.Fatalf("%s: %v", tc.msg, err)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
}

func TestWaitUntilReady(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch n {
		case 1:
			respond(w, 200, `{"id":"itm_1","status":"queued"}`, nil)
		case 2:
			respond(w, 429, `{"detail":{"error_type":"rate_limit_error","message":"Slow down."}}`, map[string]string{"Retry-After": "3"})
		case 3:
			respond(w, 200, `{"id":"itm_1","status":"processing","stage":"embedding","progress":{"chunks_embedded":2,"chunks_total":3,"pages":null}}`, nil)
		case 4:
			respond(w, 200, `{"id":"itm_1","status":"uploading"}`, nil)
		default:
			respond(w, 200, readyItem, nil)
		}
	})
	var waits []time.Duration
	c := client(t, srv.URL, dn.WithSleep(func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}))
	item, err := c.Collections().WaitUntilReady(context.Background(), "col_1", "itm_1", dn.WaitOptions{})
	if err != nil || item.Status != "ready" || item.Title != "Claims policy" || !item.Pinned || item.Metadata["mission"] != "M-42" {
		t.Fatalf("%#v %v", item, err)
	}
	// 1 s; the 429's own retry waits 3 s, and the next wait is 3 s as well;
	// then 2.25 s.
	want := []time.Duration{time.Second, 3 * time.Second, 3 * time.Second, 2250 * time.Millisecond}
	if len(waits) != len(want) {
		t.Fatalf("waits %v", waits)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v, want %v", waits, want)
		}
	}

	_, err = c.Collections().WaitUntilReady(context.Background(), "col_1", "itm_1", dn.WaitOptions{PollInterval: dn.Ptr(50 * time.Millisecond)})
	if err == nil || !strings.Contains(err.Error(), "poll interval") {
		t.Fatalf("poll interval: %v", err)
	}
}

func TestWaitUntilReadyTimesOut(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"id":"col_1","type":"text","status_counts":{"queued":1,"processing":0,"ready":0,"failed":0}}`, nil)
	})
	c := client(t, srv.URL, dn.WithSleep(func(ctx context.Context, d time.Duration) error { return nil }))
	_, err := c.Collections().WaitUntilCollectionReady(context.Background(), "col_1", dn.WaitOptions{Timeout: dn.Ptr(3 * time.Second)})
	var te *dn.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("%v", err)
	}
	// Reads at 0, 1, 2.5 and 3 s, the last wait cut to the deadline.
	if rec.count() != 4 {
		t.Fatalf("%d reads", rec.count())
	}
}

func TestKnowledgeReading(t *testing.T) {
	evidence := `{"knowledge":{"version":"knowledge-v1","status":"ok","latency_ms":141,"billed_calls":1,"results":[{` +
		`"collection":"col_1","collection_name":"Claims policy","corpus":{"version":41},"query":{"source":"auto","tokens":212},` +
		`"budget":{"mode":"auto","tokens":840,"entry":"short","cause":null},"scope":{"key":"mission","value":"M-42"},"warnings":[],` +
		`"passages":[{"n":1,"pinned":true,"item":"itm_1","reference":"policy-2026","title":"Claims policy 2026","heading":"Receipts",` +
		`"chunk":3,"byte_start":1840,"byte_end":2310,"tokens":96,"score":null,"lexical":false,"rendered":true},` +
		`{"n":2,"pinned":false,"item":"itm_2","reference":null,"title":null,"heading":null,"chunk":0,"byte_start":0,"byte_end":120,"tokens":30,"score":0.8123,"lexical":true,"rendered":true}],` +
		`"omitted":{"below_min_score":0,"per_item_cap":1},"pending_items":0,"search":{"plan":"exact","candidates":50,"fallback":false},"truncated":false}]},` +
		`"other":{"status":"ok","results":[{"collection":"x"}]}}`
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			respond(w, 200, `{"request_id":"req_1","model":"decisionnode-moe","answers":{"q":{"type":"truth","truth":true,"probability":0.9}},"evidence":`+evidence+`}`, nil)
			return
		}
		respond(w, 200, `{"collection":"col_1","passages":[{"n":1,"pinned":false,"item":"itm_1","chunk":0,"byte_start":0,"byte_end":4,"score":0.5,"text":"Rule"}],"query":{"source":"request","tokens":2,"text":"rule"}}`, nil)
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	b := dn.KnowledgeBooster("col_1", dn.KnowledgeOptions{Scope: dn.Ptr("M-42"), TopK: dn.Ptr(4), Require: true})
	d, err := c.Decide(ctx, dn.Request{State: "claim", Questions: oneTruth(), Boosters: []any{b}})
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Boosters []map[string]any `json:"boosters"`
	}
	_ = json.Unmarshal(rec.bodies[0], &sent)
	if got, _ := json.Marshal(sent.Boosters[0]); string(got) != `{"name":"knowledge","options":{"collection":"col_1","scope":"M-42","top_k":4},"require":true}` {
		t.Fatalf("booster: %s", got)
	}
	r, ok := d.Evidence.Get("knowledge").Results[0].(*dn.KnowledgeReading)
	if !ok || r.CollectionName != "Claims policy" || r.Scope == nil || r.Scope.Value != "M-42" || len(r.Passages) != 2 || r.Search.Candidates != 50 {
		t.Fatalf("reading: %#v", d.Evidence.Get("knowledge").Results[0])
	}
	if p := r.Passages[0]; !p.Pinned || p.Score != nil || p.ByteEnd != 2310 || p.Heading != "Receipts" {
		t.Fatalf("pin: %#v", p)
	}
	if p := r.Passages[1]; p.Score == nil || *p.Score != 0.8123 || p.Title != "" || !p.Lexical {
		t.Fatalf("passage: %#v", p)
	}
	if r.Omitted["per_item_cap"] != 1 || len(r.Warnings) != 0 || r.Budget.Cause != "" {
		t.Fatalf("reading: %#v", r)
	}

	s, err := c.Collections().Search(ctx, "col_1", "rule", dn.SearchOptions{TopK: dn.Ptr(2), MaxTokens: "auto"})
	if err != nil || s.Passages[0].Text != "Rule" || s.Query.Text != "rule" || s.Truncated != nil || s.Passages[0].Rendered != nil {
		t.Fatalf("search: %#v %v", s, err)
	}
	if got := string(rec.bodies[1]); got != `{"query":"rule","top_k":2,"max_tokens":"auto"}` {
		t.Fatalf("search body: %s", got)
	}
}

func TestKnowledgeMalformedReading(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"request_id":"req_1","model":"decisionnode-moe","answers":{"q":{"type":"truth","truth":true,"probability":0.9}},`+
			`"evidence":{"knowledge":{"status":"ok","results":[{"collection":"col_1","passages":[{"n":"1"}]},{"passages":[]}]}}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{State: "s", Questions: oneTruth(),
		Boosters: []any{dn.KnowledgeBooster("col_1", dn.KnowledgeOptions{})}})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range d.Evidence.Get("knowledge").Results {
		if _, ok := r.(*dn.UnknownReading); !ok {
			t.Fatalf("result %d: %#v", i, r)
		}
	}
}

func TestIngestionDelayedNotRetried(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 529, `{"detail":{"error_type":"booster_unavailable","message":"Ingestion is delayed.","details":{"reason":"ingestion_delayed"}}}`,
			map[string]string{"Retry-After": "60"})
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{State: "s", Questions: oneTruth(),
		Boosters: []any{dn.KnowledgeBooster("col_1", dn.KnowledgeOptions{Require: true})}})
	var oe *dn.OverloadedError
	if !errors.As(err, &oe) || oe.Details["reason"] != "ingestion_delayed" || rec.count() != 1 {
		t.Fatalf("%v after %d attempts", err, rec.count())
	}
}

func TestCheckDocumentAndHook(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, readyItem, nil) })
	var hooked []string
	c := client(t, srv.URL, dn.WithResponseHook(func(ctx context.Context, r dn.ResponseInfo) { hooked = append(hooked, r.Path) }))
	cols := c.Collections()
	var ve *dn.ValidationError
	if err := cols.CheckDocument(dn.DocumentText("Rule.", ""), dn.DocumentOptions{}); !errors.As(err, &ve) {
		t.Fatalf("check: %v", err)
	}
	var br *dn.BadRequestError
	if err := cols.CheckDocument(dn.DocumentData([]byte("PK\x03\x04"), "application/zip", "a.zip"), dn.DocumentOptions{}); !errors.As(err, &br) {
		t.Fatalf("check file: %v", err)
	}
	if err := cols.CheckDocument(dn.DocumentData([]byte("%PDF-1.7"), "", "a.pdf"), dn.DocumentOptions{}); err != nil {
		t.Fatalf("check pdf: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("%d requests were sent", rec.count())
	}
	if _, err := cols.WaitUntilReady(context.Background(), "col_1", "itm_1", dn.WaitOptions{}); err != nil || len(hooked) != 1 {
		t.Fatalf("wait: %v, hooked %v", err, hooked)
	}
}

func TestListItemsSearchAndSort(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.RawQuery != "limit=5&q=claims+policy&sort=title" {
			respond(w, 400, `{"detail":{"error_type":"api_usage_error","message":"`+r.URL.RawQuery+`"}}`, nil)
			return
		}
		respond(w, 200, `{"items":[],"offset":0,"limit":5,"total":0,"total_capped":false,"next_offset":null,"q":"claims policy","sort":"title","reference":null,"reference_prefix":null}`, nil)
	})
	cols := client(t, srv.URL).Collections()
	page, err := cols.ListItems(context.Background(), "col_1", dn.ItemListOptions{Limit: dn.Ptr(5), Q: dn.Ptr("claims policy"), Sort: dn.Ptr(dn.SortTitle)})
	if err != nil || page.Q != "claims policy" || page.Sort != "title" {
		t.Fatalf("%#v %v", page, err)
	}
	for _, o := range []dn.ItemListOptions{{Q: dn.Ptr("")}, {Q: dn.Ptr(strings.Repeat("q", 201))}, {Sort: dn.Ptr("size")}} {
		var br *dn.BadRequestError
		if _, err := cols.ListItems(context.Background(), "col_1", o); !errors.As(err, &br) || br.ErrorType != "api_usage_error" {
			t.Fatalf("%v", err)
		}
	}
}
