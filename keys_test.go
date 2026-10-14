package decisionnode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const keyJSON = `{"id":"key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60","name":"Kiosk 0412","prefix":"dn_live_","scope":"decide","last4":"9f3a",` +
	`"device_id":"kiosk-0412","labels":{"store":"118","fw":"2.4"},"limits":{"requests_per_second":5,"tokens_per_second":20000},` +
	`"daily_spend_cap":{"usd":2.0},"spent_today":{"usd":0.0},"expires_at":"2027-10-13T00:00:00Z","may_search_collections":false,` +
	`"status":"active","created_by_key":{"id":"key_1f0e2d3c4b5a69788796a5b4c3d2e1f0"},"created_at":"2026-10-20T09:12:04Z",` +
	`"last_used_at":null,"revoked_at":null}`

// The requests a test server saw, by method and path with the query.
type routes struct {
	mu   sync.Mutex
	seen []string
}

func (r *routes) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Method+" "+req.URL.RequestURI())
}

func TestKeysCreate(t *testing.T) {
	var seen routes
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		seen.add(r)
		if n == 1 {
			respond(w, 409, `{"detail":{"error_type":"idempotency_in_progress","message":"A request with this Idempotency-Key is still running. Try again shortly."}}`,
				map[string]string{"Retry-After": "0"})
			return
		}
		respond(w, 201, `{"key":`+keyJSON+`,"secret":"dn_live_secret"}`, nil)
	})
	c := client(t, srv.URL)
	created, err := c.Keys().Create(context.Background(), dn.KeyCreate{Name: "Kiosk 0412", DeviceID: "kiosk-0412", Labels: map[string]string{"store": "118"},
		Limits: &dn.KeyLimits{RequestsPerSecond: dn.Ptr(5)}, DailySpendCapUSD: dn.Ptr(2.0)})
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret != "dn_live_secret" || created.Key.ID != "key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60" || created.Key.Labels["fw"] != "2.4" ||
		*created.Key.Limits.TokensPerSecond != 20000 || created.Key.DailySpendCap.USD != 2 || created.Key.CreatedByKeyID != "key_1f0e2d3c4b5a69788796a5b4c3d2e1f0" ||
		created.Key.LastUsedAt != "" || created.Key.MaySearchCollections {
		t.Fatalf("created: %+v", created.Key)
	}
	if s := fmt.Sprint(created); strings.Contains(s, "dn_live_secret") {
		t.Fatalf("the secret is printed: %s", s)
	}
	if want := `{"daily_spend_cap":{"usd":2},"device_id":"kiosk-0412","labels":{"store":"118"},"limits":{"requests_per_second":5},"name":"Kiosk 0412"}`; string(rec.bodies[0]) != want {
		t.Fatalf("body %s", rec.bodies[0])
	}
	first, second := rec.heads[0].Get("Idempotency-Key"), rec.heads[1].Get("Idempotency-Key")
	uuid4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuid4.MatchString(first) || first != second || seen.seen[1] != "POST /v1/keys" {
		t.Fatalf("Idempotency-Key %q then %q, %v", first, second, seen.seen)
	}

	// An enrollment's own Idempotency-Key is sent as given.
	srv2, rec2 := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 201, `{"key":`+keyJSON+`,"secret":"s"}`, nil)
	})
	if _, err := client(t, srv2.URL).Keys().Create(context.Background(), dn.KeyCreate{Name: "n", IdempotencyKey: dn.Ptr("enroll-kiosk-0412")}); err != nil {
		t.Fatal(err)
	}
	if rec2.heads[0].Get("Idempotency-Key") != "enroll-kiosk-0412" || string(rec2.bodies[0]) != `{"name":"n"}` {
		t.Fatalf("given key: %q %s", rec2.heads[0].Get("Idempotency-Key"), rec2.bodies[0])
	}
}

func TestKeysListUpdateRevoke(t *testing.T) {
	var seen routes
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		seen.add(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/keys" && r.URL.Query().Get("cursor") == "":
			respond(w, 200, `{"items":[`+keyJSON+`],"next_cursor":"c2"}`, nil)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/keys":
			respond(w, 200, `{"items":[`+keyJSON+`],"next_cursor":null}`, nil)
		case r.Method == http.MethodPatch:
			respond(w, 200, keyJSON, nil)
		case r.Method == http.MethodDelete:
			respond(w, 200, `{"id":"key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60","revoked":true}`, nil)
		default:
			respond(w, 200, `{"would_revoke":37,"more":false}`, nil)
		}
	})
	ctx := context.Background()
	c := client(t, srv.URL)
	keys, err := c.Keys().All(ctx, dn.KeyListOptions{Labels: map[string]string{"store": "118", "fw": "2.4"}, Status: "all", Limit: dn.Ptr(2)})
	if err != nil || len(keys) != 2 {
		t.Fatalf("all: %v %d", err, len(keys))
	}
	if _, err := c.Keys().Update(ctx, "key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60", dn.KeyUpdate{Name: dn.Ptr("Kiosk 12"), ClearLimits: true, DailySpendCapUSD: dn.Ptr(1.5)}); err != nil {
		t.Fatal(err)
	}
	revoked, err := c.Keys().Revoke(ctx, "key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60")
	if err != nil || !revoked.Revoked {
		t.Fatalf("revoke: %v %+v", err, revoked)
	}
	counted, err := c.Keys().RevokeByLabels(ctx, dn.KeyRevoke{Labels: map[string]string{"store": "118"}, DryRun: true})
	if err != nil || counted.WouldRevoke != 37 || counted.More {
		t.Fatalf("dry run: %v %+v", err, counted)
	}
	want := []string{
		"GET /v1/keys?label=fw%3A2.4&label=store%3A118&status=all&limit=2",
		"GET /v1/keys?label=fw%3A2.4&label=store%3A118&status=all&limit=2&cursor=c2",
		"PATCH /v1/keys/key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60",
		"DELETE /v1/keys/key_7c2e9a41b0d34f6e8a5c1d2b3e4f5a60",
		"POST /v1/keys/revoke",
	}
	if strings.Join(seen.seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(seen.seen, "\n"))
	}
	var patch map[string]any
	_ = json.Unmarshal(rec.bodies[2], &patch)
	if v, ok := patch["limits"]; !ok || v != nil || patch["name"] != "Kiosk 12" || len(patch) != 3 {
		t.Fatalf("patch body %s", rec.bodies[2])
	}
	if string(rec.bodies[4]) != `{"dry_run":true,"labels":{"store":"118"}}` {
		t.Fatalf("revoke body %s", rec.bodies[4])
	}
}

// A key's daily spend cap resets at 00:00 UTC: the 429 is not retried.
// Each fixed rule is refused locally with the API's own error; nothing is
// sent.
func TestKeysLocalChecks(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 500, `{}`, nil) })
	ctx := context.Background()
	keys := client(t, srv.URL).Keys()
	entry := func(err error) string {
		var v *dn.ValidationError
		if !errors.As(err, &v) || len(v.Entries) != 1 {
			return fmt.Sprintf("%T %v", err, err)
		}
		e := v.Entries[0].(map[string]any)
		return fmt.Sprint(e["loc"], " ", e["msg"])
	}
	usage := func(err error) string {
		var b *dn.BadRequestError
		if !errors.As(err, &b) || b.ErrorType != "api_usage_error" {
			return fmt.Sprintf("%T %v", err, err)
		}
		return b.Error()
	}
	_, err1 := keys.Create(ctx, dn.KeyCreate{Name: "  "})
	_, err2 := keys.Create(ctx, dn.KeyCreate{Name: "n", Labels: map[string]string{"site": ""}})
	_, err3 := keys.Create(ctx, dn.KeyCreate{Name: "n", DailySpendCapUSD: dn.Ptr(0.005)})
	_, err4 := keys.Create(ctx, dn.KeyCreate{Name: "n", IdempotencyKey: dn.Ptr("")})
	_, err5 := keys.Update(ctx, "key_1", dn.KeyUpdate{})
	_, err6 := keys.RevokeByLabels(ctx, dn.KeyRevoke{})
	_, err7 := keys.List(ctx, dn.KeyListOptions{Limit: dn.Ptr(201)})
	got := []string{entry(err1), entry(err2), entry(err3), usage(err4), usage(err5), entry(err6), usage(err7)}
	want := []string{
		"[body name] Give the key a name.",
		"[body labels site] A label's value is 1 to 64 characters of text.",
		"[body daily_spend_cap] daily_spend_cap.usd is from 0.01 to 1000, in cents, or null.",
		"Idempotency-Key must be sent once, as 1 to 255 printable ASCII characters.",
		"Nothing to change.",
		"[body labels] A bulk revoke names at least one label: every key it revokes has them all.",
		"limit is a whole number from 1 to 200.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") || rec.count() != 0 {
		t.Fatalf("got:\n%s\n%d sent", strings.Join(got, "\n"), rec.count())
	}
}

func TestKeySpendCapNotRetried(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 429, `{"detail":{"error_type":"key_spend_cap_reached","message":"This key's daily spend cap of USD 2.00 is reached. It resets at 00:00 UTC.",`+
			`"details":{"resets_at":"2026-10-21T00:00:00Z","cap":{"usd":2.0},"spent":{"usd":2.01}}}}`, map[string]string{"Retry-After": "0"})
	})
	_, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	var rl *dn.RateLimitError
	if !errors.As(err, &rl) || rl.ErrorType != "key_spend_cap_reached" || rl.Details["resets_at"] != "2026-10-21T00:00:00Z" || rec.count() != 1 {
		t.Fatalf("%T %v, %d attempts", err, err, rec.count())
	}
}
