package decisionnode

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The bounds of /v1/keys checked locally before any request.
const (
	// MaxKeyNameChars is the longest name a key may have, once trimmed.
	MaxKeyNameChars = 48
	// MaxKeyLabels is the most labels a key may carry.
	MaxKeyLabels = 16
	// MaxKeyPage is the most keys a page may hold.
	MaxKeyPage = 200
	// MaxRevokeDevices is the most device ids one RevokeByLabels may name.
	MaxRevokeDevices = 1000
	// MaxKeySpendCapUSD is the highest daily spend cap, in USD.
	MaxKeySpendCapUSD = 1000
)

// KeyService makes, lists, changes and revokes the Decisions-only keys of a
// workspace through /v1/keys, for the devices a provisioning server hands
// them to. Every call needs a Manage device keys key, made in the console;
// any other kind gets a *PermissionDeniedError with ErrorType
// key_scope_denied. The fixed rules are checked locally first, as the API
// answers them; a limit's ceiling, the expiry's window and a name's
// characters are the server's. Get it from Client.Keys.
type KeyService struct{ c *Client }

// Keys returns the client's /v1/keys calls.
func (c *Client) Keys() *KeyService { return &KeyService{c: c} }

// Key is a Decisions-only key, every field as sent. Its secret is never
// listed: it is in the CreatedKey of the call that made it, once. There is
// no call that reads one key: List finds it by its labels or DeviceID.
type Key struct {
	// ID is "key_" and 32 hex digits.
	ID     string
	Name   string
	Prefix string
	Scope  string
	// Last4 is the secret's last four characters.
	Last4 string
	// DeviceID is "" on a key without one.
	DeviceID string
	// Labels is empty on a key without any.
	Labels map[string]string
	// Limits is nil when the workspace's limits apply.
	Limits *KeyLimits
	// DailySpendCap is nil on a key without a cap; SpentToday is what the
	// key has spent since 00:00 UTC.
	DailySpendCap *KeySpend
	SpentToday    *KeySpend
	// ExpiresAt is RFC 3339; "" on a key that never expires.
	ExpiresAt            string
	MaySearchCollections bool
	// Status is active, expired or revoked.
	Status string
	// CreatedByKeyID is the Manage device keys key that made it; "" for a
	// key made in the console.
	CreatedByKeyID string
	// The times are RFC 3339; "" where the API sent null.
	CreatedAt  string
	LastUsedAt string
	RevokedAt  string
	// Raw is the key object as sent.
	Raw map[string]any
}

// KeyLimits are a key's own rate limits, each a whole number from 1 to the
// workspace's own; nil leaves it out.
type KeyLimits struct {
	RequestsPerSecond *int `json:"requests_per_second,omitempty"`
	TokensPerSecond   *int `json:"tokens_per_second,omitempty"`
}

// KeySpend is an amount in USD.
type KeySpend struct {
	USD float64 `json:"usd"`
}

// KeyCreate is a new key's settings. Name is required; the rest is sent
// only when given.
type KeyCreate struct {
	// Name is 1 to 48 letters, digits, spaces, dots, dashes and
	// underscores, unique among the workspace's active keys.
	Name string
	// DeviceID is your id for the device, unique among the workspace's
	// active keys.
	DeviceID string
	// Labels are up to 16 names and values, such as {"store": "118"}.
	Labels map[string]string
	Limits *KeyLimits
	// DailySpendCapUSD is the most the key may spend in a day, to two
	// decimals.
	DailySpendCapUSD *float64
	// ExpiresAt is RFC 3339, at least a minute and at most ten years ahead.
	ExpiresAt string
	// MaySearchCollections allows the face_recognition booster.
	MaySearchCollections *bool
	// IdempotencyKey is sent as the Idempotency-Key header, 1 to 255
	// printable ASCII characters: send the same one for the same
	// enrollment, so a retry, even one of yours after a lost answer,
	// returns the same key and its secret for 24 hours. Nil sends a new one
	// for this call, the same on each of its attempts.
	IdempotencyKey *string
}

// CreatedKey is the answer to Create: the key and its secret, shown once.
type CreatedKey struct {
	Key Key
	// Secret is the key itself: hand it to the device once, never log it.
	Secret string
	// Raw is the answer as sent.
	Raw map[string]any
}

// String hides the secret, so printing a CreatedKey does not show it.
func (k CreatedKey) String() string { return "CreatedKey{" + k.Key.ID + ", secret hidden}" }

// KeyListOptions filter the keys listed, newest first; each is sent only
// when given.
type KeyListOptions struct {
	// Labels must all match a key's; each is sent as label=name:value.
	Labels   map[string]string
	DeviceID string
	// Status is active (the server's default), expired, revoked or all.
	Status string
	// Limit is 1 to 200 (50 by default).
	Limit *int
	// Cursor is the NextCursor of the page before.
	Cursor string
}

// KeyPage is one page of keys.
type KeyPage struct {
	Items []Key
	// NextCursor is the next page's Cursor; "" on the last page.
	NextCursor string
	// Raw is the page as sent.
	Raw map[string]any
}

// KeyUpdate changes a key: a field left nil (or false) stays as it is.
// Labels replaces the key's labels whole; each Clear field sends null,
// which removes that setting. A key's kind and device_id never change.
type KeyUpdate struct {
	Name                 *string
	Labels               map[string]string
	ClearLabels          bool
	Limits               *KeyLimits
	ClearLimits          bool
	DailySpendCapUSD     *float64
	ClearDailySpendCap   bool
	ExpiresAt            *string
	ClearExpiresAt       bool
	MaySearchCollections *bool
}

// RevokedKey is the answer to Revoke.
type RevokedKey struct {
	ID      string
	Revoked bool
	// Raw is the answer as sent.
	Raw map[string]any
}

// KeyRevoke names a group of keys to revoke: every active key whose labels
// match all of Labels (at least one), and, when DeviceIDs is given, among
// those devices (up to 1,000).
type KeyRevoke struct {
	Labels    map[string]string
	DeviceIDs []string
	// DryRun counts the keys and revokes none.
	DryRun bool
}

// KeysRevoked is the answer to RevokeByLabels: Revoked keys (WouldRevoke on
// a dry run), and More while matching keys are left: call it again until
// More is false.
type KeysRevoked struct {
	Revoked     int
	WouldRevoke int
	More        bool
	// Raw is the answer as sent.
	Raw map[string]any
}

// Create makes a Decisions-only key with POST /v1/keys. It carries an
// Idempotency-Key and is retried as Collections' Create is, a 409
// idempotency_in_progress after its wait included.
func (s *KeyService) Create(ctx context.Context, k KeyCreate) (*CreatedKey, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	if e := keyNameEntry(k.Name); e != nil {
		return nil, keyValidation(e)
	}
	body := map[string]any{"name": k.Name}
	if k.DeviceID != "" {
		if !deviceIDOK(k.DeviceID) {
			return nil, keyValidation(keyEntry([]any{"device_id"}, "device_id is 1 to 128 printable ASCII characters, not starting with a space.", k.DeviceID))
		}
		body["device_id"] = k.DeviceID
	}
	if k.Labels != nil {
		if e := keyLabelsEntry(k.Labels); e != nil {
			return nil, keyValidation(e)
		}
		body["labels"] = k.Labels
	}
	if k.Limits != nil {
		body["limits"] = k.Limits
	}
	if k.DailySpendCapUSD != nil {
		if e := keySpendCapEntry(*k.DailySpendCapUSD); e != nil {
			return nil, keyValidation(e)
		}
		body["daily_spend_cap"] = KeySpend{USD: *k.DailySpendCapUSD}
	}
	if k.ExpiresAt != "" {
		body["expires_at"] = k.ExpiresAt
	}
	if k.MaySearchCollections != nil {
		body["may_search_collections"] = *k.MaySearchCollections
	}
	var key string
	if k.IdempotencyKey != nil {
		key = *k.IdempotencyKey
		ok := len(key) >= 1 && len(key) <= 255
		for i := 0; i < len(key) && ok; i++ {
			ok = key[i] >= 0x20 && key[i] <= 0x7e
		}
		if !ok {
			return nil, usageError("Idempotency-Key must be sent once, as 1 to 255 printable ASCII characters.")
		}
	}
	payload, err := marshal(body)
	if err != nil {
		return nil, err
	}
	if key == "" {
		if key, err = newIdempotencyKey(); err != nil {
			return nil, err
		}
	}
	resp, err := s.c.call(ctx, http.MethodPost, "/v1/keys", payload, callOptions{policy: retryIdempotent, idempotencyKey: key})
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	created := &CreatedKey{Raw: raw}
	created.Secret, _ = raw["secret"].(string)
	if obj, ok := raw["key"].(map[string]any); ok {
		created.Key = keyFrom(obj)
	}
	return created, nil
}

// List reads one page of the workspace's Decisions-only keys, newest first,
// with GET /v1/keys. Follow NextCursor, or call All.
func (s *KeyService) List(ctx context.Context, opts KeyListOptions) (*KeyPage, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	// The label filter, device_id and status are checked as Usage's List
	// checks them.
	q, err := usageListQuery(UsageListOptions{Labels: opts.Labels, DeviceID: opts.DeviceID, Status: opts.Status})
	if err != nil {
		return nil, err
	}
	if opts.Limit != nil {
		if *opts.Limit < 1 || *opts.Limit > MaxKeyPage {
			return nil, usageError("limit is a whole number from 1 to 200.")
		}
		q = append(q, queryParam{"limit", strconv.Itoa(*opts.Limit)})
	}
	if opts.Cursor != "" {
		q = append(q, queryParam{"cursor", opts.Cursor})
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, "/v1/keys"+encodeQuery(q), nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	page := &KeyPage{Items: []Key{}, Raw: raw}
	page.NextCursor, _ = raw["next_cursor"].(string)
	items, _ := raw["items"].([]any)
	for _, it := range items {
		if obj, ok := it.(map[string]any); ok {
			page.Items = append(page.Items, keyFrom(obj))
		}
	}
	return page, nil
}

// All reads every page of List, following NextCursor until it is "".
func (s *KeyService) All(ctx context.Context, opts KeyListOptions) ([]Key, error) {
	keys := []Key{}
	for {
		page, err := s.List(ctx, opts)
		if err != nil {
			return nil, err
		}
		keys = append(keys, page.Items...)
		if page.NextCursor == "" {
			return keys, nil
		}
		opts.Cursor = page.NextCursor
	}
}

// Update changes a key with PATCH /v1/keys/{id}, sending only the fields
// given, and returns it. A key not managed by the caller is a
// *NotFoundError; an expired key takes only a rename (a *Error with Status
// 409 and ErrorType key_expired otherwise).
func (s *KeyService) Update(ctx context.Context, keyID string, u KeyUpdate) (*Key, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	body := map[string]any{}
	if u.Name != nil {
		if e := keyNameEntry(*u.Name); e != nil {
			return nil, keyValidation(e)
		}
		body["name"] = *u.Name
	}
	switch {
	case u.ClearLabels:
		body["labels"] = nil
	case u.Labels != nil:
		if e := keyLabelsEntry(u.Labels); e != nil {
			return nil, keyValidation(e)
		}
		body["labels"] = u.Labels
	}
	switch {
	case u.ClearLimits:
		body["limits"] = nil
	case u.Limits != nil:
		body["limits"] = u.Limits
	}
	switch {
	case u.ClearDailySpendCap:
		body["daily_spend_cap"] = nil
	case u.DailySpendCapUSD != nil:
		if e := keySpendCapEntry(*u.DailySpendCapUSD); e != nil {
			return nil, keyValidation(e)
		}
		body["daily_spend_cap"] = KeySpend{USD: *u.DailySpendCapUSD}
	}
	switch {
	case u.ClearExpiresAt:
		body["expires_at"] = nil
	case u.ExpiresAt != nil:
		body["expires_at"] = *u.ExpiresAt
	}
	if u.MaySearchCollections != nil {
		body["may_search_collections"] = *u.MaySearchCollections
	}
	if len(body) == 0 {
		return nil, usageError("Nothing to change.")
	}
	resp, err := s.c.do(ctx, http.MethodPatch, "/v1/keys/"+url.PathEscape(keyID), body)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	k := keyFrom(raw)
	return &k, nil
}

// Revoke revokes one key at once with DELETE /v1/keys/{id}. A key already
// revoked is a *Error with Status 409 and ErrorType already_revoked.
func (s *KeyService) Revoke(ctx context.Context, keyID string) (*RevokedKey, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	resp, err := s.c.doPayload(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(keyID), nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	r := &RevokedKey{Raw: raw}
	r.ID, _ = raw["id"].(string)
	r.Revoked, _ = raw["revoked"].(bool)
	return r, nil
}

// RevokeByLabels revokes a group of keys with POST /v1/keys/revoke, at most
// 10,000 a call, or counts them with DryRun. Repeating it is safe: it
// revokes what is left.
func (s *KeyService) RevokeByLabels(ctx context.Context, r KeyRevoke) (*KeysRevoked, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	e := keyLabelsEntry(r.Labels)
	if e == nil && len(r.Labels) == 0 {
		e = keyEntry([]any{"labels"}, "A bulk revoke names at least one label: every key it revokes has them all.", nil)
	}
	if e != nil {
		return nil, keyValidation(e)
	}
	body := map[string]any{"labels": r.Labels}
	if r.DeviceIDs != nil {
		if len(r.DeviceIDs) > MaxRevokeDevices {
			return nil, keyValidation(keyEntry([]any{"device_ids"}, "device_ids is a list of at most 1,000 device ids.", nil))
		}
		for _, d := range r.DeviceIDs {
			if !deviceIDOK(d) {
				return nil, keyValidation(keyEntry([]any{"device_id"}, "device_id is 1 to 128 printable ASCII characters, not starting with a space.", d))
			}
		}
		body["device_ids"] = r.DeviceIDs
	}
	if r.DryRun {
		body["dry_run"] = true
	}
	resp, err := s.c.do(ctx, http.MethodPost, "/v1/keys/revoke", body)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	out := &KeysRevoked{Raw: raw}
	if n, ok := raw["revoked"].(float64); ok {
		out.Revoked = int(n)
	}
	if n, ok := raw["would_revoke"].(float64); ok {
		out.WouldRevoke = int(n)
	}
	out.More, _ = raw["more"].(bool)
	return out, nil
}

// keyEntry is the API's 422 entry for a /v1/keys body field, its input
// echoed as the API echoes it.
func keyEntry(loc []any, msg string, input any) map[string]any {
	return map[string]any{"type": "value_error", "loc": append([]any{"body"}, loc...), "msg": msg, "input": echoedValue(input)}
}

// keyValidation is the local 422 of one entry, the first the API refuses.
func keyValidation(e map[string]any) error {
	loc, _ := e["loc"].([]any)
	where := make([]string, 0, len(loc))
	for _, p := range loc[1:] {
		where = append(where, fmt.Sprint(p))
	}
	return localValidation(strings.Join(where, ".")+": "+e["msg"].(string), e)
}

// keyNameEntry checks a key's name as a client may: trimmed, 1 to 48
// characters. Its character set is the server's.
func keyNameEntry(name string) map[string]any {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return keyEntry([]any{"name"}, "Give the key a name.", name)
	case utf8.RuneCountInString(trimmed) > MaxKeyNameChars:
		return keyEntry([]any{"name"}, "Key names are at most 48 characters.", name)
	}
	return nil
}

// keyLabelsEntry checks a key's labels: at most 16, each name 1 to 32 of
// a-z, 0-9, _ . and -, each value 1 to 64 characters with no control
// character, in name order.
func keyLabelsEntry(labels map[string]string) map[string]any {
	if len(labels) > MaxKeyLabels {
		return keyEntry([]any{"labels"}, "A key has at most 16 labels.", nil)
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := labels[name]
		if !usageLabelName.MatchString(name) {
			return keyEntry([]any{"labels"}, "A label's name is 1 to 32 of a-z, 0-9, _ . and -.", nil)
		}
		if n := utf8.RuneCountInString(value); n < 1 || n > 64 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return keyEntry([]any{"labels", name}, "A label's value is 1 to 64 characters of text.", nil)
		}
	}
	return nil
}

// keySpendCapEntry checks a daily spend cap: 0.01 to 1,000 USD in whole
// cents.
func keySpendCapEntry(usd float64) map[string]any {
	cents := math.Round(usd * 100)
	if math.IsNaN(usd) || math.IsInf(usd, 0) || math.Abs(usd*100-cents) >= 1e-6 || cents < 1 || usd > MaxKeySpendCapUSD {
		return keyEntry([]any{"daily_spend_cap"}, "daily_spend_cap.usd is from 0.01 to 1000, in cents, or null.", nil)
	}
	return nil
}

// deviceIDOK is a device id's rule: 1 to 128 printable ASCII characters,
// not starting with a space.
func deviceIDOK(id string) bool {
	ok := len(id) >= 1 && len(id) <= 128 && id[0] != ' '
	for i := 0; i < len(id) && ok; i++ {
		ok = id[i] >= 0x20 && id[i] <= 0x7e
	}
	return ok
}

// keyFrom reads a key object; a field of an unexpected type is left empty.
func keyFrom(raw map[string]any) Key {
	str := func(name string) string { s, _ := raw[name].(string); return s }
	k := Key{ID: str("id"), Name: str("name"), Prefix: str("prefix"), Scope: str("scope"), Last4: str("last4"), DeviceID: str("device_id"),
		ExpiresAt: str("expires_at"), Status: str("status"), CreatedAt: str("created_at"), LastUsedAt: str("last_used_at"),
		RevokedAt: str("revoked_at"), Labels: map[string]string{}, Raw: raw}
	k.MaySearchCollections, _ = raw["may_search_collections"].(bool)
	if labels, ok := raw["labels"].(map[string]any); ok {
		for name, v := range labels {
			if s, ok := v.(string); ok {
				k.Labels[name] = s
			}
		}
	}
	if obj, ok := raw["limits"].(map[string]any); ok {
		k.Limits = &KeyLimits{}
		remarshal(obj, k.Limits)
	}
	if obj, ok := raw["daily_spend_cap"].(map[string]any); ok {
		k.DailySpendCap = &KeySpend{}
		remarshal(obj, k.DailySpendCap)
	}
	if obj, ok := raw["spent_today"].(map[string]any); ok {
		k.SpentToday = &KeySpend{}
		remarshal(obj, k.SpentToday)
	}
	if obj, ok := raw["created_by_key"].(map[string]any); ok {
		k.CreatedByKeyID, _ = obj["id"].(string)
	}
	return k
}
