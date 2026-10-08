package decisionnode

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestImageDimensionsFromHeaders(t *testing.T) {
	cases := []struct {
		file      string
		mediaType string
		w, h      int
	}{
		{"3x2.png", "image/png", 3, 2},
		{"3x2.jpg", "image/jpeg", 3, 2},
		{"3x2-lossless.webp", "image/webp", 3, 2},
		{"3x2-lossy.webp", "image/webp", 3, 2},
		{"7000x6000-extended.webp", "image/webp", 7000, 6000},
		{"8000x6000-header.png", "image/png", 8000, 6000},
	}
	for _, c := range cases {
		b, err := os.ReadFile(filepath.Join("testdata", c.file))
		if err != nil {
			t.Fatal(err)
		}
		if got := sniffMediaType(b); got != c.mediaType {
			t.Errorf("%s: media type %q, want %q", c.file, got, c.mediaType)
		}
		w, h, ok := imageDimensions(b, c.mediaType)
		if !ok || w != c.w || h != c.h {
			t.Errorf("%s: got %dx%d (%v), want %dx%d", c.file, w, h, ok, c.w, c.h)
		}
	}
}

func TestJPEGHeaderVariants(t *testing.T) {
	// A progressive frame (SOF2) after an APP0 segment and fill bytes.
	b := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00, 0xFF, 0xFF, 0xC2, 0x00, 0x11, 0x08, 0x01, 0x2C, 0x02, 0x58, 0x03}
	if w, h, ok := jpegDimensions(b); !ok || w != 600 || h != 300 {
		t.Fatalf("got %dx%d %v", w, h, ok)
	}
	// Start of scan before any frame header, and a truncated header.
	for _, bad := range [][]byte{{0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02, 0, 0}, {0xFF, 0xD8, 0xFF, 0xC0, 0x00}} {
		if _, _, ok := jpegDimensions(bad); ok {
			t.Fatalf("%x: read a size from a broken header", bad)
		}
	}
}

func TestCheckImage(t *testing.T) {
	png, _ := os.ReadFile(filepath.Join("testdata", "3x2.png"))
	huge, _ := os.ReadFile(filepath.Join("testdata", "8000x6000-header.png"))
	long := "https://example.com/" + string(make([]byte, MaxImageURLChars))
	for name, img := range map[string]Image{
		"gif":        ImageData("g", []byte("GIF89a\x01\x00\x01\x00"), ""),
		"mismatch":   ImageData("m", png, "image/jpeg"),
		"too large":  ImageData("l", append(append([]byte(nil), png...), make([]byte, MaxImageBytes)...), ""),
		"megapixels": ImageData("h", huge, ""),
		"plain http": ImageURL("u", "http://example.com/a.png"),
		"long url":   ImageURL("u", long),
		"bad type":   ImageUpload("u", "upl_x", "image/gif"),
	} {
		var bad *BadRequestError
		want := "api_usage_error"
		if name == "long url" {
			want = "image_url_refused"
		}
		if err := checkImage(img, limits{}); !errors.As(err, &bad) || bad.Status != 0 || bad.ErrorType != want {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if err := checkImage(ImageData("ok", png, "image/png"), limits{}); err != nil {
		t.Fatal(err)
	}
	var seventeen []Image
	for i := 0; i < 17; i++ {
		seventeen = append(seventeen, ImageURL(fmt.Sprint(i), "https://x/a"))
	}
	if err := checkImages(seventeen[:16], limits{}); err != nil {
		t.Fatal(err)
	}
	if err := checkImages(seventeen, limits{}); err == nil {
		t.Fatal("seventeen images accepted")
	}
	if err := checkImages(seventeen[:5], limits{maxImages: 4}); err == nil {
		t.Fatal("five images accepted under a held limit of 4")
	}
	if err := checkImages([]Image{ImageURL("a", "https://x/a"), ImageURL("a", "https://x/b")}, limits{}); err == nil {
		t.Fatal("a repeated id accepted")
	}
}

func TestQuestionChecks(t *testing.T) {
	many := make([]string, MaxChoiceOptions+1)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	bad := map[string]struct {
		q          any
		validation bool
	}{
		"empty choice":     {Choice("?", []string{}), false},
		"too many options": {Choice("?", many), false},
		"empty score":      {Score("?", []string{}), true},
		"eleven levels":    {Score("?", []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}), false},
		"truth no text":    {Truth(nil), false},
		"wire truth":       {map[string]any{"type": "noul"}, false},
		"grid too long":    {NumberWith("?", 0, 1000, 0.1), true},
		"min above max":    {NumberWith("?", 60, 50, nil), true},
		"step zero":        {NumberWith("?", 0, 10, 0), true},
		"seven decimals":   {NumberWith("?", 0.0000001, 1, nil), true},
		"over 2^53":        {NumberWith("?", nil, 1e16, nil), true},
		"string bound":     {NumberWith("?", "5", nil, nil), true},
		"no instructions":  {NumberWith("", 0, 10, nil), false},
	}
	for name, c := range bad {
		_, err := checkQs(Questions{Q("q", c.q)}, false)
		var v *ValidationError
		var b *BadRequestError
		switch {
		case c.validation && (!errors.As(err, &v) || len(v.Entries) == 0 || v.Reason == ""):
			t.Errorf("%s: want a ValidationError with entries, got %v", name, err)
		case !c.validation && !errors.As(err, &b):
			t.Errorf("%s: want a BadRequestError, got %v", name, err)
		}
	}
	for name, q := range map[string]any{
		"default grid":  Number("How many?"),
		"largest grid":  NumberWith("?", 0, 4095, nil),
		"decimal grid":  NumberWith("?", json.Number("0.25"), 2, 0.5),
		"truth by side": TruthWith(nil, "yes", nil),
	} {
		if _, err := checkQs(Questions{Q("q", q)}, false); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The grid entry carries the API's shape.
	_, err := checkQs(Questions{Q("cars", NumberWith("?", 0, 1000, 0.1))}, false)
	var v *ValidationError
	errors.As(err, &v)
	e := v.Entries[0].(map[string]any)
	if e["type"] != "too_long" || v.Reason != "question 'cars': the grid holds 10001 values, the limit is 4096; narrow the range or widen the step" {
		t.Fatalf("got %v, %q", e, v.Reason)
	}
}

func TestSystemoneWritesTruthAsNoul(t *testing.T) {
	got, err := checkQs(Questions{
		Q("a", Truth("?")),
		Q("b", json.RawMessage(`{"instructions":"?", "type" : "truth","criteria":{"true":"y"}}`)),
		Q("c", Choice("?", []string{"x"})),
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"type":"noul","instructions":"?"},"b":{"instructions":"?","type":"noul","criteria":{"true":"y"}},"c":{"type":"choice","instructions":"?","criteria":{"x":null}}}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	spaced := replaceMember([]byte(`{"instructions":"?", "type" : "truth","n":{"type":"truth"}}`), "type", []byte(`"noul"`))
	if string(spaced) != `{"instructions":"?", "type" : "noul","n":{"type":"truth"}}` {
		t.Fatalf("got %s", spaced)
	}
}

func TestDecimals(t *testing.T) {
	for in, want := range map[string]int{"0.5": 1, "7": 0, "0.25": 2, "1e-3": 3, "0.50": 1, "-2.125": 3} {
		r, _ := new(big.Rat).SetString(in)
		if got := decimalPlaces(r); got != want {
			t.Errorf("%s: %d decimals, want %d", in, got, want)
		}
	}
	r, _ := new(big.Rat).SetString("999999999.9999990")
	if got := decimalText(r); got != "999999999.999999" {
		t.Fatalf("got %s", got)
	}
}

func TestUploadCacheMarginAndEviction(t *testing.T) {
	c := newUploadCache()
	k := func(i int) cacheKey { return cacheKey{sum: [32]byte{byte(i), byte(i >> 8)}, mediaType: "image/png"} }
	c.mu.Lock()
	c.putLocked(k(1), "upl_soon", time.Now().Add(4*time.Minute))
	c.putLocked(k(2), "upl_live", time.Now().Add(time.Hour))
	c.mu.Unlock()
	if _, ok := c.get(k(1), time.Now()); ok {
		t.Fatal("an entry within 5 minutes of its expiry was used")
	}
	if hit, ok := c.get(k(2), time.Now()); !ok || hit.ID != "upl_live" {
		t.Fatal("a live entry was not used")
	}
	c.mu.Lock()
	for i := 10; i < 10+cacheEntries; i++ {
		c.putLocked(k(i), "upl", time.Now().Add(time.Hour))
	}
	c.mu.Unlock()
	if _, ok := c.get(k(2), time.Now()); ok || len(c.entries) != cacheEntries {
		t.Fatalf("the least recently used entry stayed, %d entries", len(c.entries))
	}
	c.drop([]string{"upl"})
	if !c.empty() {
		t.Fatal("drop left entries")
	}
}

func TestRetryPolicies(t *testing.T) {
	five := &ServerError{&Error{Status: 500}}
	busy := &OverloadedError{errorBase: &Error{Status: 529}}
	lost := &ConnectionError{&Error{}}
	limit := &SessionLimitError{errorBase: &Error{Status: 429, ErrorType: "session_limit_exceeded"}}
	for _, c := range []struct {
		err    error
		policy retryPolicy
		want   bool
	}{
		{five, retryDefault, true}, {five, retryMutation, false},
		{busy, retryMutation, true}, {lost, retryDefault, true}, {lost, retryMutation, false},
		{limit, retryDefault, false},
	} {
		if got := retryable(c.err, c.policy); got != c.want {
			t.Errorf("%T under %d: got %v", c.err, c.policy, got)
		}
	}
	var rate *RateLimitError
	if !errors.As(error(limit), &rate) {
		t.Fatal("a SessionLimitError is not a RateLimitError")
	}
}

// checkQs encodes and checks questions as a request would.
func checkQs(qs Questions, systemone bool) ([]byte, error) {
	var qc questionCheck
	b, err := encodeQuestions(qs, systemone, limits{}, &qc)
	if err != nil {
		return nil, err
	}
	return b, qc.err()
}

func TestImageURLRules(t *testing.T) {
	for url, want := range map[string]string{
		"http://example.com/a.jpg":            "api_usage_error",
		"https://user:pw@example.com/a.jpg":   "image_url_refused",
		"https://example.com:8443/a.jpg":      "image_url_refused",
		"https://169.254.169.254/a.jpg":       "image_url_refused",
		"https://[::1]/a.jpg":                 "image_url_refused",
		"https://[::ffff:10.0.0.1]/a.jpg":     "image_url_refused",
		"https://[64:ff9b::7f00:1]/a.jpg":     "image_url_refused",
		"https://example.com:443/a.jpg":       "",
		"https://8.8.8.8/a.jpg":               "",
		"https://files.example.com/a.jpg?x=1": "",
	} {
		err := checkImageURL("u", url)
		var bad *BadRequestError
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: refused: %v", url, err)
		case want != "" && (!errors.As(err, &bad) || bad.ErrorType != want):
			t.Errorf("%s: got %v, want %s", url, err, want)
		}
	}
}

func TestJSONDepth(t *testing.T) {
	for in, want := range map[string]int{`"x"`: 0, `{}`: 1, `{"a":[{"b":"[[["}]}`: 3, `["\\\"[",[[]]]`: 3} {
		if got := jsonDepth([]byte(in)); got != want {
			t.Errorf("%s: depth %d, want %d", in, got, want)
		}
	}
}

func TestHeldImageAndVideoLimits(t *testing.T) {
	if l := limitsOf("m", ModelInfo{Raw: map[string]any{}}); l.maxImages != 0 || l.maxVideos != 0 {
		t.Fatalf("%+v", l)
	}
	l := limitsOf("m", ModelInfo{Raw: map[string]any{
		"images": map[string]any{"max_images": float64(8), "max_image_bytes": float64(1024)},
		"videos": map[string]any{"max_videos": float64(2)},
	}})
	if l.maxImages != 8 || l.maxImageBytes != 1024 || l.maxVideos != 2 {
		t.Fatalf("%+v", l)
	}
	if l := limitsOf("m", ModelInfo{Raw: map[string]any{"max_images": float64(4), "images": map[string]any{"max_images": float64(8)}}}); l.maxImages != 4 {
		t.Fatalf("the top-level limit first: %+v", l)
	}
	if got := decimal(1696789012.5); got != "1.69679e+09" {
		t.Fatal(got)
	}
	if got := pyRepr("it's"); got != `"it's"` {
		t.Fatal(got)
	}
}
