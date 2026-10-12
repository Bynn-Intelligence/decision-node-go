package decisionnode_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

func TestCollections(t *testing.T) {
	var (
		mu   sync.Mutex
		urls []*url.URL
	)
	seen := func(i int) *url.URL {
		mu.Lock()
		defer mu.Unlock()
		return urls[i]
	}
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		urls = append(urls, r.URL)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/collections":
			respond(w, 201, `{"id":"col_1","type":"face","name":"Staff","public":false,"items":0,"created_at":"2026-10-12T09:00:00Z","deletion":null}`, nil)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/collections":
			respond(w, 200, `{"collections":[{"id":"col_1","type":"face","name":"Staff","public":false,"items":1,"deletion":{"at":"2026-11-01T00:00:00Z","reason":"storage_unpaid"}},`+
				`{"id":"celebrities","type":"face","name":"celebrities","public":true,"items":1200}]}`, nil)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/items"):
			respond(w, 201, `{"id":"itm_1","collection":"col_1","reference":"emp-1042","label":null,"faces_found":2,`+
				`"box":{"x1":412,"y1":220,"x2":580,"y2":470},"created_at":"2026-10-12T09:00:00Z","usage":{"enrollments":1,"cost":{"usd":0.001}}}`, nil)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items"):
			respond(w, 200, `{"items":[{"id":"itm_1","reference":"emp-1042","label":"Reception","created_at":"2026-10-12T09:00:00Z"}],"offset":0,"limit":5,"total":1,"next_offset":null}`, nil)
		case r.Method == http.MethodDelete:
			respond(w, 200, `{"id":"itm_1","deleted":true}`, nil)
		default:
			respond(w, 404, `{"detail":{"error_type":"collection_not_found","message":"Collection not found: x."}}`, nil)
		}
	})
	c := client(t, srv.URL)
	ctx := context.Background()
	col, err := c.Collections().Create(ctx, "Staff", "")
	if err != nil || col.ID != "col_1" || col.Deletion != nil || col.Raw["deletion"] != nil {
		t.Fatalf("create: %#v %v", col, err)
	}
	if got := string(rec.bodies[0]); got != `{"type":"face","name":"Staff"}` {
		t.Fatalf("create body: %s", got)
	}
	list, err := c.Collections().List(ctx)
	if err != nil || len(list) != 2 || list[0].Deletion == nil || list[0].Deletion.Reason != "storage_unpaid" || !list[1].Public {
		t.Fatalf("list: %#v %v", list, err)
	}
	item, err := c.Collections().Enroll(ctx, "col_1", dn.Enrollment{Image: dn.ImageURL("badge", "https://files.example.com/b.jpg"), Reference: "emp-1042"})
	if err != nil || item.FacesFound != 2 || item.Box == nil || item.Box.X2 != 580 || item.Label != "" || item.Usage == nil || *item.Usage.Enrollments != 1 {
		t.Fatalf("enroll: %#v %v", item, err)
	}
	if got := string(rec.bodies[2]); got != `{"image":{"id":"badge","url":"https://files.example.com/b.jpg"},"reference":"emp-1042"}` {
		t.Fatalf("enroll body: %s", got)
	}
	page, err := c.Collections().ListItems(ctx, "col_1", dn.ItemListOptions{Limit: dn.Ptr(5)})
	if err != nil || len(page.Items) != 1 || page.Items[0].Label != "Reception" || page.NextOffset != nil || seen(3).RawQuery != "limit=5" {
		t.Fatalf("items: %#v %v", page, err)
	}
	gone, err := c.Collections().DeleteItem(ctx, "col_1", "itm/1")
	if err != nil || !gone.Deleted || seen(4).EscapedPath() != "/v1/collections/col_1/items/itm%2F1" {
		t.Fatalf("delete item: %#v %v %s", gone, err, seen(4).EscapedPath())
	}
	_, err = c.Collections().Get(ctx, "x")
	var nf *dn.NotFoundError
	if !errors.As(err, &nf) || nf.ErrorType != "collection_not_found" {
		t.Fatalf("get: %v", err)
	}
}

func TestCollectionsCheckedLocally(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 500, `{}`, nil) })
	c := client(t, srv.URL)
	ctx := context.Background()
	badge := dn.ImageURL("badge", "https://files.example.com/b.jpg")
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{second(c.Collections().Create(ctx, "", "")), "string_too_short"},
		{second(c.Collections().Create(ctx, strings.Repeat("é", 81), "")), "string_too_long"},
		{second(c.Collections().Create(ctx, "Plates", "plate")), "literal_error"},
		{second(c.Collections().Enroll(ctx, "col_1", dn.Enrollment{Image: badge, Reference: "emp 1042"})), "string_pattern_mismatch"},
		{second(c.Collections().Enroll(ctx, "col_1", dn.Enrollment{Image: badge, Reference: strings.Repeat("e", 65)})), "string_too_long"},
		{second(c.Collections().Enroll(ctx, "col_1", dn.Enrollment{Image: badge, Reference: "r", Label: dn.Ptr(strings.Repeat("l", 129))})), "string_too_long"},
		{second(c.Collections().Enroll(ctx, "col_1", dn.Enrollment{Reference: "r"})), "missing"},
	} {
		var ve *dn.ValidationError
		if !errors.As(tc.err, &ve) {
			t.Fatalf("%s: %v", tc.kind, tc.err)
		}
		if e, _ := ve.Entries[0].(map[string]any); e["type"] != tc.kind {
			t.Fatalf("%s: %#v", tc.kind, ve.Entries)
		}
	}
	_, err := c.Collections().ListItems(ctx, "col_1", dn.ItemListOptions{Offset: dn.Ptr(-1)})
	var br *dn.BadRequestError
	if !errors.As(err, &br) || br.ErrorType != "api_usage_error" {
		t.Fatalf("offset: %v", err)
	}
	if len(rec.bodies) != 0 {
		t.Fatalf("sent %d requests", len(rec.bodies))
	}
}

func second[T any](_ T, err error) error { return err }
