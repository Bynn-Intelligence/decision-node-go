package decisionnode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The bounds of a usage read's query, checked locally before any request.
const (
	// MaxUsageKeyIDs is the most key ids one usage list may filter by.
	MaxUsageKeyIDs = 100
	// MaxUsagePage is the most keys a usage page may hold.
	MaxUsagePage = 1000
)

var (
	usageKeyID     = regexp.MustCompile(`^key_[0-9a-f]{32}$`)
	usageLabelName = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)
)

// UsageService reads each key's usage and cost for a period: GET
// /v1/keys/{key_id}/usage for one key and GET /v1/usage for many. A Full
// access key reads every key of its workspace; a Manage device keys key
// the Decisions-only keys. Reads retry as any call. Get it from
// Client.Usage.
type UsageService struct{ c *Client }

// Usage returns the client's per-key usage reads.
func (c *Client) Usage() *UsageService { return &UsageService{c: c} }

// UsageOptions are one key's usage read's period and breakdown, each
// left out when "". Without Start and End the server reads the UTC month so
// far.
type UsageOptions struct {
	// Start and End are RFC 3339, rounded by the server to the hour.
	Start string
	End   string
	// Granularity is none, hour, day or month: buckets of that length.
	Granularity string
	// GroupBy is model or channel.
	GroupBy string
}

// UsageListOptions are a usage list's period, breakdown and filters. Start
// and End are required; the rest is sent only when given.
type UsageListOptions struct {
	Start       string
	End         string
	Granularity string
	GroupBy     string
	// Labels must all match a key's; each is sent as label=name:value.
	Labels map[string]string
	// DeviceID is the key with this device_id.
	DeviceID string
	// KeyIDs are the keys to read, at most MaxUsageKeyIDs.
	KeyIDs []string
	// Scope is full, decide or keys.
	Scope string
	// Status is active, expired, revoked or all (the server's default).
	Status string
	// IncludeEmpty lists the keys with no usage in the period too.
	IncludeEmpty *bool
	// Limit is the most keys a page holds, 1 to MaxUsagePage. A page can
	// hold fewer.
	Limit *int
	// Cursor is the NextCursor of the page before.
	Cursor string
}

// KeyUsage is one key's usage over a period, every field as sent.
type KeyUsage struct {
	Key    UsageKey
	Period UsagePeriod
	// AsOf is when the figures are complete to; the period's figures are
	// final once it equals Period.End.
	AsOf      string
	Totals    UsageTotals
	Buckets   []UsageBucket
	ByModel   []UsageGroup
	ByChannel []UsageGroup
	// Raw is the object as sent.
	Raw map[string]any
}

// UsagePage is one page of GET /v1/usage.
type UsagePage struct {
	Period UsagePeriod
	AsOf   string
	Items  []UsageItem
	// NextCursor is the next page's Cursor; "" on the last page.
	NextCursor string
	// Raw is the page as sent.
	Raw map[string]any
}

// UsageItem is one key's usage on a usage page.
type UsageItem struct {
	Key       UsageKey
	Totals    UsageTotals
	Buckets   []UsageBucket
	ByModel   []UsageGroup
	ByChannel []UsageGroup
	// Raw is the item as sent.
	Raw map[string]any
}

// UsageKey is the key a usage read is about, as it is now.
type UsageKey struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	// DeviceID is "" on a key without one.
	DeviceID string            `json:"device_id"`
	Labels   map[string]string `json:"labels"`
	// Status is active, expired or revoked.
	Status string `json:"status"`
	// The times are RFC 3339; "" where the API sent null.
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at"`
	ExpiresAt string `json:"expires_at"`
}

// UsagePeriod is the period as applied: Start rounded down and End up to
// the hour, in UTC.
type UsagePeriod struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// UsageTotals are a period's or a bucket's counts and cost.
type UsageTotals struct {
	Requests         int64 `json:"requests"`
	FreeTierRequests int64 `json:"free_tier_requests"`
	// InputTokens includes ImageTokens and VideoTokens.
	InputTokens  int64     `json:"input_tokens"`
	ImageTokens  int64     `json:"image_tokens"`
	VideoTokens  int64     `json:"video_tokens"`
	BoosterCalls int64     `json:"booster_calls"`
	Cost         UsageCost `json:"cost"`
}

// UsageCost is what the workspace is charged, in USD, as sent.
type UsageCost struct {
	USD         float64 `json:"usd"`
	ModelUSD    float64 `json:"model_usd"`
	BoostersUSD float64 `json:"boosters_usd"`
}

// UsageBucket is one hour, day or month of a period.
type UsageBucket struct {
	Start  string      `json:"start"`
	End    string      `json:"end"`
	Totals UsageTotals `json:"totals"`
}

// UsageGroup is one model's (Model) or one channel's (Channel) share.
type UsageGroup struct {
	Model   string      `json:"model"`
	Channel string      `json:"channel"`
	Totals  UsageTotals `json:"totals"`
}

// usageParts are the fields an item and a key's usage share.
type usageParts struct {
	Key       UsageKey      `json:"key"`
	Totals    UsageTotals   `json:"totals"`
	Buckets   []UsageBucket `json:"buckets"`
	ByModel   []UsageGroup  `json:"by_model"`
	ByChannel []UsageGroup  `json:"by_channel"`
}

// Key reads one key's usage with GET /v1/keys/{key_id}/usage. A key outside
// the caller's set is a *NotFoundError.
func (s *UsageService) Key(ctx context.Context, keyID string, opts UsageOptions) (*KeyUsage, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	q, err := usagePeriod(opts.Start, opts.End, opts.Granularity, opts.GroupBy)
	if err != nil {
		return nil, err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, "/v1/keys/"+url.PathEscape(keyID)+"/usage"+encodeQuery(q), nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	var shape struct {
		usageParts
		Period UsagePeriod `json:"period"`
		AsOf   string      `json:"as_of"`
	}
	// A field of an unexpected type is left empty; Raw holds it as sent.
	_ = json.Unmarshal(resp.data, &shape)
	return &KeyUsage{Key: shape.Key, Period: shape.Period, AsOf: shape.AsOf, Totals: shape.Totals, Buckets: shape.Buckets,
		ByModel: shape.ByModel, ByChannel: shape.ByChannel, Raw: raw}, nil
}

// List reads one page of keys' usage with GET /v1/usage, every key with
// usage in the period (or every key, with IncludeEmpty) that matches the
// filters. Follow NextCursor, or call All, since a page can hold fewer keys
// than Limit.
func (s *UsageService) List(ctx context.Context, opts UsageListOptions) (*UsagePage, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	q, err := usageListQuery(opts)
	if err != nil {
		return nil, err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, "/v1/usage"+encodeQuery(q), nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	var shape struct {
		Period     UsagePeriod       `json:"period"`
		AsOf       string            `json:"as_of"`
		Items      []json.RawMessage `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	_ = json.Unmarshal(resp.data, &shape)
	page := &UsagePage{Period: shape.Period, AsOf: shape.AsOf, Items: []UsageItem{}, Raw: raw}
	if shape.NextCursor != nil {
		page.NextCursor = *shape.NextCursor
	}
	for _, data := range shape.Items {
		var itemRaw map[string]any
		if err := json.Unmarshal(data, &itemRaw); err != nil {
			return nil, invalidResponse(resp, err)
		}
		var parts usageParts
		_ = json.Unmarshal(data, &parts)
		page.Items = append(page.Items, UsageItem{Key: parts.Key, Totals: parts.Totals, Buckets: parts.Buckets,
			ByModel: parts.ByModel, ByChannel: parts.ByChannel, Raw: itemRaw})
	}
	return page, nil
}

// All reads every page of List, following NextCursor until it is "", and
// returns every page's items in order. opts.Cursor is the first page's.
func (s *UsageService) All(ctx context.Context, opts UsageListOptions) ([]UsageItem, error) {
	items := []UsageItem{}
	for {
		page, err := s.List(ctx, opts)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			return items, nil
		}
		opts.Cursor = page.NextCursor
	}
}

// queryParam is one name and value of a query, in the order sent.
type queryParam struct{ name, value string }

func encodeQuery(q []queryParam) string {
	if len(q) == 0 {
		return ""
	}
	parts := make([]string, len(q))
	for i, p := range q {
		parts[i] = url.QueryEscape(p.name) + "=" + url.QueryEscape(p.value)
	}
	return "?" + strings.Join(parts, "&")
}

func usagePeriod(start, end, granularity, groupBy string) ([]queryParam, error) {
	switch granularity {
	case "", "none", "hour", "day", "month":
	default:
		return nil, usageError("granularity is none, hour, day or month.")
	}
	switch groupBy {
	case "", "model", "channel":
	default:
		return nil, usageError("group_by is model or channel.")
	}
	var q []queryParam
	for _, p := range []queryParam{{"start", start}, {"end", end}, {"granularity", granularity}, {"group_by", groupBy}} {
		if p.value != "" {
			q = append(q, p)
		}
	}
	return q, nil
}

// usageListQuery checks a usage list's query as the API reads it and
// builds it, labels by name so a map sends them the same every time.
func usageListQuery(o UsageListOptions) ([]queryParam, error) {
	q, err := usagePeriod(o.Start, o.End, o.Granularity, o.GroupBy)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(o.Labels))
	for name := range o.Labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := o.Labels[name]
		n := utf8.RuneCountInString(value)
		if !usageLabelName.MatchString(name) || n < 1 || n > 64 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, usageError("A label filter is name:value, as labels are written.")
		}
		q = append(q, queryParam{"label", name + ":" + value})
	}
	if o.DeviceID != "" {
		if !deviceIDOK(o.DeviceID) {
			return nil, usageError("device_id is 1 to 128 printable ASCII characters, not starting with a space.")
		}
		q = append(q, queryParam{"device_id", o.DeviceID})
	}
	for i, id := range o.KeyIDs {
		if !usageKeyID.MatchString(id) {
			return nil, usageError("key_id is a key's id, key_ and 32 hex digits.")
		}
		if i == MaxUsageKeyIDs {
			return nil, usageError("At most 100 key_id filters.")
		}
		q = append(q, queryParam{"key_id", id})
	}
	switch o.Scope {
	case "":
	case "full", "decide", "keys":
		q = append(q, queryParam{"scope", o.Scope})
	default:
		return nil, usageError("scope is full, decide or keys.")
	}
	switch o.Status {
	case "":
	case "active", "expired", "revoked", "all":
		q = append(q, queryParam{"status", o.Status})
	default:
		return nil, usageError("status is active, expired, revoked or all.")
	}
	if o.IncludeEmpty != nil {
		q = append(q, queryParam{"include_empty", strconv.FormatBool(*o.IncludeEmpty)})
	}
	if o.Limit != nil {
		if *o.Limit < 1 || *o.Limit > MaxUsagePage {
			return nil, usageError("limit is a whole number from 1 to 1000.")
		}
		q = append(q, queryParam{"limit", strconv.Itoa(*o.Limit)})
	}
	if o.Cursor != "" {
		q = append(q, queryParam{"cursor", o.Cursor})
	}
	return q, nil
}
