package decisionnode

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
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

// The types of collection.
const (
	// CollectionFace holds faces, which face_recognition searches.
	CollectionFace = "face"
	// CollectionText holds documents (a knowledge base), which the
	// knowledge booster and Search read.
	CollectionText = "text"
)

// Collection is a collection object: one of the workspace's own collections
// of faces or of documents, or a public one it may search. Every field is as
// sent; the fields of a text collection are empty on a face collection.
type Collection struct {
	// ID is "col_" and 32 hex digits for the workspace's own collection; a
	// public collection's id is its name, such as "celebrities".
	ID string
	// Type is CollectionFace or CollectionText.
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
	// ScopeKey is a text collection's scope key, the metadata key whose
	// value scopes an item; "" when it has none.
	ScopeKey string
	// Chunks and Tokens are what a text collection's ready documents hold.
	Chunks int
	Tokens int
	// Version is a text collection's corpus version, which moves on every
	// change to what is served.
	Version int
	// StatusCounts counts a text collection's documents by status; nil when
	// not sent.
	StatusCounts *StatusCounts
	// Pinned is a text collection's pinned load against its cap; nil when
	// not sent.
	Pinned *PinnedLoad
	// Raw is the collection object as sent.
	Raw map[string]any
}

// StatusCounts counts a text collection's documents by status.
type StatusCounts struct {
	Queued     int `json:"queued"`
	Processing int `json:"processing"`
	Ready      int `json:"ready"`
	Failed     int `json:"failed"`
}

// PinnedLoad is the tokens a text collection's pinned documents hold: the
// global ones, with the largest scope value's beside them, against the cap.
type PinnedLoad struct {
	Tokens int `json:"tokens"`
	Cap    int `json:"cap"`
	// LargestScope is the scope value whose pins hold the most tokens; nil
	// when there is none.
	LargestScope *ScopeLoad `json:"largest_scope"`
}

// ScopeLoad is one scope value's pinned tokens.
type ScopeLoad struct {
	Value  string `json:"value"`
	Tokens int    `json:"tokens"`
}

// CollectionDeletion says when and why a collection's items will be deleted.
type CollectionDeletion struct {
	At     string `json:"at"`
	Reason string `json:"reason"`
}

// CollectionItem is an item of a collection: one enrolled face, known by its
// reference and, when it has one, its label (the picture is never kept), or
// one document of a text collection. A field the item's kind does not have
// is empty.
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
	// Metadata is the item's own keys and values as sent; nil when it has
	// none. A document's scope value is the collection's scope key here.
	Metadata map[string]any

	// The fields of a document, each as sent and empty when not sent.

	Title string
	// Format is "markdown" or "text" for a document sent as text.
	Format  string
	Pinned  bool
	PinRank int
	// Source is "article" (sent as text) or "file" (an uploaded file).
	Source string
	// File describes an uploaded file; nil for an article.
	File *ItemFile
	// Status is the newest version's: "queued", "processing", "ready",
	// "failed", or any other value as sent ("uploading" included).
	Status string
	// Stage is "extracting", "chunking" or "embedding" while processing;
	// "" otherwise.
	Stage string
	// Progress is how far processing is; nil when not sent.
	Progress *ItemProgress
	// Serving is "current", "previous" (an older version serves until the
	// new one is ready) or "none".
	Serving string
	// Error says why the document failed, as a code such as
	// "pinned_budget_exceeded"; "" when it has not.
	Error string
	// Delayed is true when the document has waited in the queue past the
	// usual time.
	Delayed bool
	// Warnings are notes about the text, such as "possible_secret"; empty,
	// never nil, when there are none.
	Warnings []string
	// Chunks and Tokens are what the serving version holds.
	Chunks        int
	Tokens        int
	ContentSHA256 string
	Version       int
	QueuedAt      string
	ReadyAt       string
	// Text is the stored text, only from GetItem with includeText; nil
	// otherwise, and while no version serves. Byte offsets of chunks and
	// passages are UTF-8 byte offsets into it, end exclusive.
	Text *string

	// Raw is the item object as sent.
	Raw map[string]any
}

// ItemFile is the file a document was made from.
type ItemFile struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int    `json:"byte_size"`
	// Pages is the page count of a paged file; nil when not known.
	Pages *int `json:"pages"`
	// OriginalHeld is true while the original file is still kept; it is
	// deleted once a version is ready, and only its text is kept.
	OriginalHeld bool `json:"original_held"`
}

// ItemProgress is how far a document's processing is.
type ItemProgress struct {
	ChunksEmbedded int `json:"chunks_embedded"`
	ChunksTotal    int `json:"chunks_total"`
	// Pages is the pages read so far; nil when not sent.
	Pages *int `json:"pages"`
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
	// TotalCapped is true when Total stopped counting at the server's cap.
	TotalCapped bool
	// Reference and ReferencePrefix are the page's filter, as sent; "" when
	// none.
	Reference       string
	ReferencePrefix string
	Raw             map[string]any
}

// Deleted is the answer to a delete: the id deleted and Deleted true. A text
// collection not purged within the server's time answers with Deleted false
// and Status "purging": the purge goes on, and both answers are success.
type Deleted struct {
	ID      string
	Deleted bool
	// Status is "purging" while a text collection is being purged; "" when
	// not sent.
	Status string
	Raw    map[string]any
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
	// Metadata, when not nil, is the item's own keys and values: at most 20
	// keys of 1 to 40 characters of a-z, 0-9 and _, each value a text of at
	// most 500 characters, a number, or true or false.
	Metadata map[string]any
}

// ItemListOptions pages ListItems. A nil field is left to the server:
// offset 0, limit 20. Offset is 0 or more and Limit 1 to 100; anything else
// is refused locally with a *BadRequestError.
type ItemListOptions struct {
	Offset *int
	Limit  *int
	// Reference lists the items with exactly this reference, and
	// ReferencePrefix those whose reference starts with it; one or the
	// other, each 1 to 64 characters of A-Z, a-z, 0-9, _ . : and -.
	Reference       *string
	ReferencePrefix *string
}

// CollectionService is the Collections API: the workspace's collections of
// faces, which the face_recognition booster searches.
type CollectionService struct{ c *Client }

// Collections returns the client's Collections API.
func (c *Client) Collections() *CollectionService { return &CollectionService{c: c} }

// Create creates a collection with POST /v1/collections: typ "" is
// CollectionFace, or CollectionText for a knowledge base. CreateWith also
// takes a text collection's scope key. The name is 1 to 80 characters; a
// name or a type the API would refuse is refused locally with a
// *ValidationError and its entries. The call carries an Idempotency-Key, new
// for each call and the same on every attempt, so it is retried like any
// call and creates one collection however often it is sent; a 409
// idempotency_in_progress is retried after its Retry-After.
func (s *CollectionService) Create(ctx context.Context, name, typ string) (*Collection, error) {
	return s.CreateWith(ctx, CollectionCreate{Name: name, Type: typ})
}

// CollectionCreate is the collection CreateWith creates.
type CollectionCreate struct {
	// Name is 1 to 80 characters.
	Name string
	// Type is CollectionFace (also for "") or CollectionText.
	Type string
	// ScopeKey, when not nil, is a text collection's scope key: 1 to 40
	// characters of a-z, 0-9 and _, the metadata key whose value scopes a
	// document (a mission, a site, a customer), fixed for the collection's
	// life. Name the value in a search's or the knowledge booster's Scope.
	ScopeKey *string
}

// scopeKeyPattern is the API's pattern for a text collection's scope key.
const scopeKeyPattern = "^[a-z0-9_]{1,40}$"

var scopeKey = regexp.MustCompile(scopeKeyPattern)

// CreateWith creates a collection as Create does, with a text collection's
// scope key, sent only when given. A scope key not of its pattern is refused
// locally with a *ValidationError; one given with a face collection is the
// server's to refuse.
func (s *CollectionService) CreateWith(ctx context.Context, p CollectionCreate) (*Collection, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	typ := p.Type
	if typ == "" {
		typ = CollectionFace
	}
	var entries []any
	reason := ""
	if typ != CollectionFace && typ != CollectionText {
		entries = append(entries, map[string]any{"type": "literal_error", "loc": []any{"body", "type"}, "msg": "Input should be 'face' or 'text'",
			"input": echoed(typ), "ctx": map[string]any{"expected": "'face' or 'text'"}})
		reason = "type must be face or text"
	}
	if e := stringEntry([]any{"body", "name"}, p.Name, MaxCollectionNameChars, false); e != nil {
		entries = append(entries, e)
		if reason == "" {
			reason = fmt.Sprintf("name must be 1 to %d characters", MaxCollectionNameChars)
		}
	}
	if p.ScopeKey != nil && !scopeKey.MatchString(*p.ScopeKey) {
		entries = append(entries, map[string]any{"type": "string_pattern_mismatch", "loc": []any{"body", "scope_key"},
			"msg": "String should match pattern '" + scopeKeyPattern + "'", "input": echoed(*p.ScopeKey)})
		if reason == "" {
			reason = "scope_key must be 1 to 40 characters of a-z, 0-9 and _"
		}
	}
	if len(entries) > 0 {
		return nil, localValidation(reason, entries...)
	}
	body, err := marshal(struct {
		Type     string  `json:"type"`
		Name     string  `json:"name"`
		ScopeKey *string `json:"scope_key,omitempty"`
	}{typ, p.Name, p.ScopeKey})
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

// Delete deletes a collection and every item in it with DELETE
// /v1/collections/{id}. A face collection is deleted at once; a text
// collection is purged, and one not purged within the server's time answers
// Status "purging" with Deleted false, which is success too (the purge is
// not waited for). A public collection cannot be deleted: a
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
	if e.Metadata != nil {
		if ms := metadataEntries(e.Metadata); len(ms) > 0 {
			entries = append(entries, ms...)
			if reason == "" {
				reason = "metadata: " + ms[0].(map[string]any)["msg"].(string)
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
		if e.Metadata != nil {
			buf.WriteByte(',')
			if err := writeMember(&buf, "metadata", e.Metadata); err != nil {
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

// ItemUpdate is what UpdateItem changes; a field left at its zero value is
// not sent and stays as it is.
type ItemUpdate struct {
	// Reference, when not nil, replaces the item's reference.
	Reference *string
	// Label, when not nil, replaces the item's label; ClearLabel sends null
	// instead, which removes it.
	Label      *string
	ClearLabel bool
	// Metadata, when not nil, replaces the item's whole metadata; an empty
	// map removes every key.
	Metadata map[string]any
	// Pinned and PinRank, when not nil, change a document's pin; applied at
	// once, never re-embedded or billed.
	Pinned  *bool
	PinRank *int
	// Title, when not nil, retitles a document: 1 to 200 characters once
	// trimmed. It makes a new version (the API's 202).
	Title *string
}

// UpdateItem changes an item with PATCH /v1/collections/{id}/items/{item_id},
// sending only the fields given, and returns the item: 200 with the fields
// applied, or 202 when a document's new title makes a new version. They are
// checked locally as Enroll and AddDocument check them. It carries an Idempotency-Key, as Create
// and Enroll do, and is retried as they are.
func (s *CollectionService) UpdateItem(ctx context.Context, collectionID, itemID string, u ItemUpdate) (*CollectionItem, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	if u.ClearLabel && u.Label != nil {
		return nil, errors.New("decisionnode: give Label or ClearLabel, not both")
	}
	var entries []any
	reason := ""
	var buf bytes.Buffer
	buf.WriteByte('{')
	member := func(name string, v any) error {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		return writeMember(&buf, name, v)
	}
	if u.Reference != nil {
		if en := stringEntry([]any{"body", "reference"}, *u.Reference, MaxItemReferenceChars, true); en != nil {
			entries = append(entries, en)
			reason = fmt.Sprintf("reference must be 1 to %d characters of A-Z, a-z, 0-9, _ . : and -", MaxItemReferenceChars)
		}
		if err := member("reference", *u.Reference); err != nil {
			return nil, err
		}
	}
	switch {
	case u.ClearLabel:
		if err := member("label", nil); err != nil {
			return nil, err
		}
	case u.Label != nil:
		if en := stringEntry([]any{"body", "label"}, *u.Label, MaxItemLabelChars, false); en != nil {
			entries = append(entries, en)
			if reason == "" {
				reason = fmt.Sprintf("label must be 1 to %d characters", MaxItemLabelChars)
			}
		}
		if err := member("label", *u.Label); err != nil {
			return nil, err
		}
	}
	if u.Metadata != nil {
		if ms := metadataEntries(u.Metadata); len(ms) > 0 {
			entries = append(entries, ms...)
			if reason == "" {
				reason = "metadata: " + ms[0].(map[string]any)["msg"].(string)
			}
		}
		if err := member("metadata", u.Metadata); err != nil {
			return nil, err
		}
	}
	if u.Pinned != nil {
		if err := member("pinned", *u.Pinned); err != nil {
			return nil, err
		}
	}
	if u.PinRank != nil {
		if en := pinRankEntry(*u.PinRank); en != nil {
			entries = append(entries, en)
			if reason == "" {
				reason = fmt.Sprintf("pin_rank must be an integer from 0 to %d", MaxPinRank)
			}
		}
		if err := member("pin_rank", *u.PinRank); err != nil {
			return nil, err
		}
	}
	if u.Title != nil {
		if en := titleEntry(*u.Title); en != nil {
			entries = append(entries, en)
			if reason == "" {
				reason = fmt.Sprintf("title must be 1 to %d characters", MaxTitleChars)
			}
		}
		if err := member("title", *u.Title); err != nil {
			return nil, err
		}
	}
	if buf.Len() == 1 {
		entries = append(entries, map[string]any{"type": "missing", "loc": []any{"body"}, "msg": "Send reference, label, metadata or any of them", "input": nil})
		reason = "send reference, label, metadata or any of them"
	}
	if len(entries) > 0 {
		return nil, localValidation(reason, entries...)
	}
	buf.WriteByte('}')
	key, err := newIdempotencyKey()
	if err != nil {
		return nil, err
	}
	path := collectionPath(collectionID) + "/items/" + url.PathEscape(itemID)
	resp, err := s.c.call(ctx, http.MethodPatch, path, buf.Bytes(), callOptions{policy: retryIdempotent, idempotencyKey: key})
	if err != nil {
		return nil, err
	}
	item, err := parseItem(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	return item, nil
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
	if opts.Reference != nil && opts.ReferencePrefix != nil {
		return nil, usageError("Send query parameter reference or reference_prefix, not both.")
	}
	for _, f := range []struct {
		name  string
		value *string
	}{{"reference", opts.Reference}, {"reference_prefix", opts.ReferencePrefix}} {
		if f.value == nil {
			continue
		}
		if !itemReference.MatchString(*f.value) {
			return nil, usageError(fmt.Sprintf("Query parameter %s must be 1 to 64 characters of A-Z, a-z, 0-9, _, ., : and -, got %s.", f.name, printable(*f.value, 64)))
		}
		if query == "" {
			query = "?"
		} else {
			query += "&"
		}
		query += f.name + "=" + url.QueryEscape(*f.value)
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
		Capped     bool              `json:"total_capped"`
		Reference  *string           `json:"reference"`
		Prefix     *string           `json:"reference_prefix"`
	}
	_ = json.Unmarshal(resp.data, &shape)
	page := &CollectionItemPage{Items: []CollectionItem{}, Offset: shape.Offset, Limit: shape.Limit, Total: shape.Total, NextOffset: shape.NextOffset,
		TotalCapped: shape.Capped, Raw: raw}
	if shape.Reference != nil {
		page.Reference = *shape.Reference
	}
	if shape.Prefix != nil {
		page.ReferencePrefix = *shape.Prefix
	}
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
	d.Status, _ = raw["status"].(string)
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
	col.ScopeKey, _ = raw["scope_key"].(string)
	col.Chunks, _ = wholeNumber(raw["chunks"])
	col.Tokens, _ = wholeNumber(raw["tokens"])
	col.Version, _ = wholeNumber(raw["version"])
	if m, ok := raw["status_counts"].(map[string]any); ok {
		sc := &StatusCounts{}
		sc.Queued, _ = wholeNumber(m["queued"])
		sc.Processing, _ = wholeNumber(m["processing"])
		sc.Ready, _ = wholeNumber(m["ready"])
		sc.Failed, _ = wholeNumber(m["failed"])
		col.StatusCounts = sc
	}
	if m, ok := raw["pinned"].(map[string]any); ok {
		pl := &PinnedLoad{}
		pl.Tokens, _ = wholeNumber(m["tokens"])
		pl.Cap, _ = wholeNumber(m["cap"])
		if ls, ok := m["largest_scope"].(map[string]any); ok {
			sl := &ScopeLoad{}
			sl.Value, _ = ls["value"].(string)
			sl.Tokens, _ = wholeNumber(ls["tokens"])
			pl.LargestScope = sl
		}
		col.Pinned = pl
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
		Metadata   map[string]any   `json:"metadata"`
	}
	_ = json.Unmarshal(data, &shape)
	item := &CollectionItem{ID: shape.ID, Collection: shape.Collection, Reference: shape.Reference, FacesFound: shape.FacesFound, Usage: shape.Usage, Metadata: shape.Metadata, Raw: raw}
	if shape.Label != nil {
		item.Label = *shape.Label
	}
	if shape.CreatedAt != nil {
		item.CreatedAt = *shape.CreatedAt
	}
	if box, ok := faceBox(raw["box"]); ok {
		item.Box = &box
	}
	readDocumentFields(item, raw)
	return item, nil
}

// The limits of an item's metadata, checked locally before any request.
const (
	// MaxMetadataKeys is the most keys an item's metadata may hold.
	MaxMetadataKeys = 20
	// MaxMetadataKeyChars is the longest a metadata key may be.
	MaxMetadataKeyChars = 40
	// MaxMetadataTextChars is the longest a metadata text may be.
	MaxMetadataTextChars = 500
)

var metadataKey = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// metadataEntries are the API's 422 entries for an item's metadata: more
// than MaxMetadataKeys keys (that alone), then, in key order, each key not of
// the pattern, each text over MaxMetadataTextChars and each value that is
// neither a text, a number nor a boolean. A text's control characters, a
// number's range and the whole object's size are the server's to judge.
func metadataEntries(m map[string]any) []any {
	loc := []any{"body", "metadata"}
	entry := func(loc []any, msg string, input any) map[string]any {
		return map[string]any{"type": "value_error", "loc": loc, "msg": msg, "input": input}
	}
	if len(m) > MaxMetadataKeys {
		return []any{entry(loc, fmt.Sprintf("Metadata holds at most %d keys.", MaxMetadataKeys), nil)}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []any
	for _, k := range keys {
		if !metadataKey.MatchString(k) {
			out = append(out, entry(loc, fmt.Sprintf(`A metadata key is 1 to %d characters of a-z, 0-9 and _: "%s".`, MaxMetadataKeyChars, shownKey(k)), nil))
			continue
		}
		switch v := m[k].(type) {
		case string:
			if utf8.RuneCountInString(v) > MaxMetadataTextChars {
				out = append(out, entry(append(append([]any(nil), loc...), k), fmt.Sprintf("A metadata text is at most %d characters.", MaxMetadataTextChars), echoed(v)))
			}
		case bool, json.Number, float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		default:
			out = append(out, entry(append(append([]any(nil), loc...), k), "A metadata value is a text, a number or true or false.", echoedValue(v)))
		}
	}
	return out
}

// shownKey is a metadata key as the API's message shows it: its first 40
// characters, a control character as "?", then "..." when it was longer.
func shownKey(k string) string {
	r := []rune(k)
	more := len(r) > MaxMetadataKeyChars
	if more {
		r = r[:MaxMetadataKeyChars]
	}
	for i, c := range r {
		if unicode.IsControl(c) {
			r[i] = '?'
		}
	}
	if more {
		return string(r) + "..."
	}
	return string(r)
}

// echoedValue is any input as a collections 422 entry echoes it: a string
// as echoed, an object or an array as null, anything else as it is.
func echoedValue(v any) any {
	switch x := v.(type) {
	case string:
		return echoed(x)
	case map[string]any, []any:
		return nil
	}
	return v
}

// printable is a caller's value as an API message echoes it: its first
// most characters, any but printable ASCII as \u{hex}, then "..." when it
// was longer.
func printable(v string, most int) string {
	var b strings.Builder
	n := 0
	for _, c := range v {
		if n == most {
			b.WriteString("...")
			break
		}
		if c >= 0x20 && c <= 0x7e {
			b.WriteRune(c)
		} else {
			fmt.Fprintf(&b, "\\u{%x}", c)
		}
		n++
	}
	return b.String()
}
