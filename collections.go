package decisionnode

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"unicode/utf8"
)

// The limits of a collection and its items, checked locally before any
// request.
const (
	// MaxCollectionNameChars is the longest name a collection may have, in
	// characters (code points).
	MaxCollectionNameChars = 80
	// MaxItemReferenceChars is the longest reference an item may carry.
	MaxItemReferenceChars = 64
	// MaxItemLabelChars is the longest label an item may carry.
	MaxItemLabelChars = 128
)

// itemReferencePattern is the API's pattern for an item's reference,
// checked after the length.
const itemReferencePattern = "^[A-Za-z0-9_.:-]{1,64}$"

var itemReference = regexp.MustCompile(itemReferencePattern)

// Collection is a collection object: one of the workspace's own collections
// of faces, or a public one it may search. Every field is as sent.
type Collection struct {
	// ID is "col_" and 32 hex digits for the workspace's own collection; a
	// public collection's id is its name, such as "celebrities".
	ID string
	// Type is "face".
	Type string
	Name string
	// Public is true on a public collection, which is read-only.
	Public bool
	// Items is how many items the collection holds now.
	Items int
	// CreatedAt is as sent (RFC 3339, UTC).
	CreatedAt string
	// Deletion is set while the workspace's items are marked for deletion.
	Deletion *CollectionDeletion
	// Raw is the collection object as sent.
	Raw map[string]any
}

// CollectionDeletion says when and why a collection's items will be deleted.
type CollectionDeletion struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// CollectionItem is an item of a collection: one enrolled face, known by its
// reference and, when it has one, its label. The picture is never kept.
type CollectionItem struct {
	// ID is "itm_" and 32 hex digits.
	ID string
	// Collection is the collection's id; set on an enrollment's answer.
	Collection string
	Reference  string
	// Label is "" when the item has none.
	Label string
	// FacesFound is how many faces the enrolled image held; the largest was
	// enrolled. Set on an enrollment's answer.
	FacesFound int
	// Box is the enrolled face's corners in pixels of the image as sent; set
	// on an enrollment's answer.
	Box *FaceBox
	// CreatedAt is as sent (RFC 3339, UTC).
	CreatedAt string
	// Usage is what an enrollment billed; nil on a listed item.
	Usage *EnrollmentUsage
	// Raw is the item object as sent.
	Raw map[string]any
}

// EnrollmentUsage is what one enrollment billed, as sent.
type EnrollmentUsage struct {
	// Enrollments is the face-model calls billed, nil when not sent.
	Enrollments *int `json:"enrollments"`
	// Cost is what they cost, nil when not sent.
	Cost *Cost `json:"cost"`
}

// CollectionItemPage is one page of ListItems, newest first.
type CollectionItemPage struct {
	Items  []CollectionItem
	Offset int
	Limit  int
	Total  int
	// NextOffset is the offset of the next page, nil on the last.
	NextOffset *int
	Raw        map[string]any
}

// Deleted is the answer to a delete: the id deleted and Deleted true.
type Deleted struct {
	ID      string
	Deleted bool
	Raw     map[string]any
}

// Enrollment is one face to enroll: the image it is in, the caller's own
// reference for it and an optional label.
type Enrollment struct {
	// Image is the picture, in any form a decision takes; it is sent as a
	// decision's image is (local bytes inline or uploaded). The largest
	// face found is enrolled.
	Image Image
	// Reference is 1 to 64 characters of A-Z, a-z, 0-9, _ . : and -.
	Reference string
	// Label, when not nil, is shown in a match: at most 128 characters.
	Label *string
}

// ItemListOptions pages ListItems. A nil field is left to the server:
// offset 0, limit 20. Offset is 0 or more and Limit 1 to 100; anything else
// is refused locally with a *BadRequestError.
type ItemListOptions struct {
	Offset *int
	Limit  *int
}

// CollectionService is the Collections API: the workspace's collections of
// faces, which the face_recognition booster searches.
type CollectionService struct{ c *Client }

// Collections returns the client's Collections API.
func (c *Client) Collections() *CollectionService { return &CollectionService{c: c} }

// Create creates a collection with POST /v1/collections. typ "" is "face",
// the only type. The name is 1 to 80 characters; a name or a type the API
// would refuse is refused locally with a *ValidationError and its entries.
// The call carries an Idempotency-Key, new for each call and the same on
// every attempt, so it is retried like any call and creates one collection
// however often it is sent; a 409 idempotency_in_progress is retried after
// its Retry-After.
func (s *CollectionService) Create(ctx context.Context, name, typ string) (*Collection, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	if typ == "" {
		typ = "face"
	}
	var entries []any
	reason := ""
	if typ != "face" {
		entries = append(entries, map[string]any{"type": "literal_error", "loc": []any{"body", "type"}, "msg": "Input should be 'face'",
			"input": echoed(typ), "ctx": map[string]any{"expected": "'face'"}})
		reason = "type must be face"
	}
	if e := stringEntry([]any{"body", "name"}, name, MaxCollectionNameChars, false); e != nil {
		entries = append(entries, e)
		if reason == "" {
			reason = fmt.Sprintf("name must be 1 to %d characters", MaxCollectionNameChars)
		}
	}
	if len(entries) > 0 {
		return nil, localValidation(reason, entries...)
	}
	body, err := marshal(struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}{typ, name})
	if err != nil {
		return nil, encodeError(err)
	}
	key, err := newIdempotencyKey()
	if err != nil {
		return nil, err
	}
	resp, err := s.c.call(ctx, http.MethodPost, "/v1/collections", body, callOptions{policy: retryIdempotent, idempotencyKey: key})
	if err != nil {
		return nil, err
	}
	return parseCollectionResponse(resp)
}

// List reads the workspace's own collections, newest first, then the public
// ones it may search, with GET /v1/collections.
func (s *CollectionService) List(ctx context.Context) ([]Collection, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, "/v1/collections", nil)
	if err != nil {
		return nil, err
	}
	var shape struct {
		Collections *[]json.RawMessage `json:"collections"`
	}
	if err := json.Unmarshal(resp.data, &shape); err != nil {
		return nil, invalidResponse(resp, err)
	}
	if shape.Collections == nil {
		return nil, invalidResponse(resp, fmt.Errorf("the response holds no collections"))
	}
	out := make([]Collection, 0, len(*shape.Collections))
	for _, data := range *shape.Collections {
		col, err := parseCollection(data)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		out = append(out, *col)
	}
	return out, nil
}

// Get reads one collection with GET /v1/collections/{id}. An id that is
// neither the workspace's own nor a public one it may search is a
// *NotFoundError with ErrorType collection_not_found.
func (s *CollectionService) Get(ctx context.Context, collectionID string) (*Collection, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, collectionPath(collectionID), nil)
	if err != nil {
		return nil, err
	}
	return parseCollectionResponse(resp)
}

// Delete deletes a collection and every item in it at once with DELETE
// /v1/collections/{id}. A public collection cannot be deleted: a
// *PermissionDeniedError with ErrorType collection_read_only.
func (s *CollectionService) Delete(ctx context.Context, collectionID string) (*Deleted, error) {
	return s.delete(ctx, collectionPath(collectionID))
}

// Enroll enrolls one face with POST /v1/collections/{id}/items: the largest
// face in the image. The reference and the label are checked locally as
// the API checks them (a *ValidationError), and an image URL by a
// decision's URL rules; any other fault of the picture is the API's
// *ValidationError with the entry image_unreadable, and an image with no
// face the one with no_face. The call carries an Idempotency-Key, as Create
// does, so a retry never enrolls or bills the face twice.
func (s *CollectionService) Enroll(ctx context.Context, collectionID string, e Enrollment) (*CollectionItem, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	var entries []any
	reason := ""
	if e.Image.id == "" && e.Image.url == "" && e.Image.uploadID == "" && e.Image.src == nil {
		entries = append(entries, map[string]any{"type": "missing", "loc": []any{"body", "image"}, "msg": "Field required",
			"input": nil})
		reason = "image: Field required"
	}
	if en := stringEntry([]any{"body", "reference"}, e.Reference, MaxItemReferenceChars, true); en != nil {
		entries = append(entries, en)
		if reason == "" {
			reason = fmt.Sprintf("reference must be 1 to %d characters of A-Z, a-z, 0-9, _ . : and -", MaxItemReferenceChars)
		}
	}
	if e.Label != nil {
		if en := stringEntry([]any{"body", "label"}, *e.Label, MaxItemLabelChars, false); en != nil {
			entries = append(entries, en)
			if reason == "" {
				reason = fmt.Sprintf("label must be at most %d characters", MaxItemLabelChars)
			}
		}
	}
	if len(entries) > 0 {
		return nil, localValidation(reason, entries...)
	}
	// Only a URL's rules are checked here, as a decision's; every other
	// fault of the picture is the server's 422 image_unreadable.
	images := []Image{e.Image}
	if e.Image.url != "" {
		if err := checkImageURL(e.Image.id, e.Image.url); err != nil {
			return nil, err
		}
	}
	path := collectionPath(collectionID) + "/items"
	for resend := 0; ; resend++ {
		pl, err := s.c.placeImages(ctx, images, true)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		buf.WriteString(`{"image":`)
		buf.Write(pl.wires[0])
		buf.WriteByte(',')
		if err := writeMember(&buf, "reference", e.Reference); err != nil {
			return nil, err
		}
		if e.Label != nil {
			buf.WriteByte(',')
			if err := writeMember(&buf, "label", *e.Label); err != nil {
				return nil, err
			}
		}
		buf.WriteByte('}')
		if err := checkBodySize(buf.Len()); err != nil {
			return nil, err
		}
		// A resend after a stale upload is another body, so it has its own key.
		key, err := newIdempotencyKey()
		if err != nil {
			return nil, err
		}
		resp, err := s.c.call(ctx, http.MethodPost, path, buf.Bytes(), callOptions{policy: retryIdempotent, idempotencyKey: key})
		if err != nil && resend == 0 && len(pl.placed) > 0 && staleUpload(err) {
			s.c.uploads.drop(pl.placed)
			continue
		}
		if err != nil {
			return nil, err
		}
		item, err := parseItem(resp.data)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		return item, nil
	}
}

// ListItems reads one page of a collection's items, newest first, with GET
// /v1/collections/{id}/items. A public collection's items are not listed: a
// *PermissionDeniedError with ErrorType collection_read_only.
func (s *CollectionService) ListItems(ctx context.Context, collectionID string, opts ItemListOptions) (*CollectionItemPage, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	query, err := pageQuery(opts.Offset, opts.Limit)
	if err != nil {
		return nil, err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, collectionPath(collectionID)+"/items"+query, nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	var shape struct {
		Items      []json.RawMessage `json:"items"`
		Offset     int               `json:"offset"`
		Limit      int               `json:"limit"`
		Total      int               `json:"total"`
		NextOffset *int              `json:"next_offset"`
	}
	_ = json.Unmarshal(resp.data, &shape)
	page := &CollectionItemPage{Items: []CollectionItem{}, Offset: shape.Offset, Limit: shape.Limit, Total: shape.Total, NextOffset: shape.NextOffset, Raw: raw}
	for _, data := range shape.Items {
		item, err := parseItem(data)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		page.Items = append(page.Items, *item)
	}
	return page, nil
}

// DeleteItem deletes one item at once with DELETE
// /v1/collections/{id}/items/{item_id}. An id not in the collection is a
// *NotFoundError with ErrorType item_not_found.
func (s *CollectionService) DeleteItem(ctx context.Context, collectionID, itemID string) (*Deleted, error) {
	return s.delete(ctx, collectionPath(collectionID)+"/items/"+url.PathEscape(itemID))
}

func (s *CollectionService) delete(ctx context.Context, path string) (*Deleted, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	resp, err := s.c.doPayload(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	d := &Deleted{Raw: raw}
	d.ID, _ = raw["id"].(string)
	d.Deleted, _ = raw["deleted"].(bool)
	return d, nil
}

// newIdempotencyKey is a random UUID (version 4) for one call's
// Idempotency-Key.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return "", fmt.Errorf("decisionnode: no randomness for an Idempotency-Key: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// collectionPath is a collection's path, its id escaped as given.
func collectionPath(id string) string { return "/v1/collections/" + url.PathEscape(id) }

// pageQuery is the query of a paged list, offset first, as the API documents
// it: "" when neither is given. An offset below 0 or a limit outside 1 to
// 100 is the API's 400, refused locally.
func pageQuery(offset, limit *int) (string, error) {
	q := url.Values{}
	if offset != nil {
		if *offset < 0 {
			return "", usageError(fmt.Sprintf("Query parameter offset must be an integer of 0 or more, got %d.", *offset))
		}
		q.Set("offset", strconv.Itoa(*offset))
	}
	if limit != nil {
		if *limit < 1 || *limit > 100 {
			return "", usageError(fmt.Sprintf("Query parameter limit must be an integer from 1 to 100, got %d.", *limit))
		}
		q.Set("limit", strconv.Itoa(*limit))
	}
	if len(q) == 0 {
		return "", nil
	}
	return "?" + encodeOrdered(q, "offset", "limit"), nil
}

// stringEntry is the API's 422 entry for a string field that is empty,
// longer than most characters (code points) or, for an item's reference,
// not of its pattern, checked in that order; nil when it is none of them.
// Its input is echoed as the API echoes it.
func stringEntry(loc []any, v string, most int, reference bool) map[string]any {
	switch {
	case v == "":
		return map[string]any{"type": "string_too_short", "loc": loc, "msg": "String should have at least 1 character",
			"input": "", "ctx": map[string]any{"min_length": 1}}
	case utf8.RuneCountInString(v) > most:
		return map[string]any{"type": "string_too_long", "loc": loc, "msg": fmt.Sprintf("String should have at most %d characters", most),
			"input": echoed(v), "ctx": map[string]any{"max_length": most}}
	case reference && !itemReference.MatchString(v):
		return map[string]any{"type": "string_pattern_mismatch", "loc": loc, "msg": fmt.Sprintf("String should match pattern '%s'", itemReferencePattern),
			"input": v, "ctx": map[string]any{"pattern": itemReferencePattern}}
	}
	return nil
}

// echoChars is how much of a string a collections 422 entry echoes.
const echoChars = 64

// echoed is a string as a collections 422 entry echoes it: its first 64
// characters (code points), with "..." when it was longer.
func echoed(v string) string {
	if utf8.RuneCountInString(v) <= echoChars {
		return v
	}
	return string([]rune(v)[:echoChars]) + "..."
}

func parseCollectionResponse(resp *response) (*Collection, error) {
	col, err := parseCollection(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	return col, nil
}

func parseCollection(data []byte) (*Collection, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		ID        string              `json:"id"`
		Type      string              `json:"type"`
		Name      string              `json:"name"`
		Public    bool                `json:"public"`
		Items     int                 `json:"items"`
		CreatedAt *string             `json:"created_at"`
		Deletion  *CollectionDeletion `json:"deletion"`
	}
	// A field of an unexpected type is left empty; Raw holds it as sent.
	_ = json.Unmarshal(data, &shape)
	col := &Collection{ID: shape.ID, Type: shape.Type, Name: shape.Name, Public: shape.Public, Items: shape.Items, Deletion: shape.Deletion, Raw: raw}
	if shape.CreatedAt != nil {
		col.CreatedAt = *shape.CreatedAt
	}
	return col, nil
}

func parseItem(data []byte) (*CollectionItem, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		ID         string           `json:"id"`
		Collection string           `json:"collection"`
		Reference  string           `json:"reference"`
		Label      *string          `json:"label"`
		FacesFound int              `json:"faces_found"`
		CreatedAt  *string          `json:"created_at"`
		Usage      *EnrollmentUsage `json:"usage"`
	}
	_ = json.Unmarshal(data, &shape)
	item := &CollectionItem{ID: shape.ID, Collection: shape.Collection, Reference: shape.Reference, FacesFound: shape.FacesFound, Usage: shape.Usage, Raw: raw}
	if shape.Label != nil {
		item.Label = *shape.Label
	}
	if shape.CreatedAt != nil {
		item.CreatedAt = *shape.CreatedAt
	}
	if box, ok := faceBox(raw["box"]); ok {
		item.Box = &box
	}
	return item, nil
}
