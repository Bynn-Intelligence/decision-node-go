package decisionnode

import (
	"net/http"
	"testing"
	"time"
)

func TestParseSafety(t *testing.T) {
	if s := parseSafety(nil); s != nil {
		t.Fatalf("no header: got %+v", s)
	}
	s := parseSafety([]string{"harm=0.912;flagged, self_harm=0.004"})
	if !s.Checked || s.Harm != 0.912 || s.SelfHarm != 0.004 || len(s.Flagged) != 1 || s.Flagged[0] != "harm" {
		t.Fatalf("flagged: got %+v", s)
	}
	s = parseSafety([]string{"harm=0.964;blocked, self_harm=0.95;flagged"})
	if len(s.Flagged) != 2 || s.Flagged[0] != "harm" || s.Flagged[1] != "self_harm" {
		t.Fatalf("blocked and flagged: got %+v", s.Flagged)
	}
	s = parseSafety([]string{`unchecked;reason="no-model-question"`})
	if s.Checked || s.Reason != "no-model-question" {
		t.Fatalf("unchecked: got %+v", s)
	}
	s = parseSafety([]string{"harm=0.010", "self_harm=0.020"})
	if s.Harm != 0.01 || s.SelfHarm != 0.02 || len(s.Flagged) != 0 {
		t.Fatalf("two header lines: got %+v", s)
	}
}

func TestRetryAfter(t *testing.T) {
	cases := []struct {
		h    http.Header
		want time.Duration
		ok   bool
	}{
		{http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"2"}}, 1500 * time.Millisecond, true},
		{http.Header{"Retry-After": {"2"}}, 2 * time.Second, true},
		{http.Header{"Retry-After-Ms": {"20.5"}}, 20500 * time.Microsecond, true},
		{http.Header{"Retry-After-Ms": {"soon"}, "Retry-After": {"1"}}, time.Second, true},
		{http.Header{}, 0, false},
	}
	for _, c := range cases {
		got, ok := retryAfter(c.h)
		if got != c.want || ok != c.ok {
			t.Errorf("%v: got %v %v, want %v %v", c.h, got, ok, c.want, c.ok)
		}
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got, ok := retryAfter(http.Header{"Retry-After": {future}}); !ok || got <= 0 || got > 3*time.Second {
		t.Errorf("HTTP date: got %v %v", got, ok)
	}
}

func TestBackoff(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		want := 500 * time.Millisecond << attempt
		if want > 8*time.Second || want <= 0 {
			want = 8 * time.Second
		}
		for i := 0; i < 50; i++ {
			got := backoff(attempt)
			if got < want*3/4 || got > want {
				t.Fatalf("attempt %d: %v outside [%v, %v]", attempt, got, want*3/4, want)
			}
		}
	}
}

func TestSniffMediaType(t *testing.T) {
	cases := map[string]string{
		"\xff\xd8\xff\xe0rest":         "image/jpeg",
		"\x89PNG\r\n\x1a\nrest":        "image/png",
		"RIFF\x00\x00\x00\x00WEBPVP8 ": "image/webp",
		"GIF89a":                       "",
		"":                             "",
	}
	for in, want := range cases {
		if got := sniffMediaType([]byte(in)); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestNewErrorBodies(t *testing.T) {
	err := newError(400, http.Header{"X-Decisionnode-Error": {"too long"}}, []byte(`{"detail":{"error_type":"max_tokens_exceeded"}}`))
	cl, ok := err.(*ContextLimitError)
	if !ok || cl.ErrorType != "max_tokens_exceeded" || cl.Error() != "" || cl.Reason != "too long" {
		t.Fatalf("got %#v", err)
	}
	err = newError(400, http.Header{}, []byte(`{"detail":"Too many choices. Must have at most 255 choices."}`))
	if br, ok := err.(*BadRequestError); !ok || br.Error() != "Too many choices. Must have at most 255 choices." || br.ErrorType != "" {
		t.Fatalf("got %#v", err)
	}
	err = newError(502, http.Header{}, []byte(`<html>Bad gateway</html>`))
	if se, ok := err.(*ServerError); !ok || se.Body != nil || se.Status != 502 {
		t.Fatalf("got %#v", err)
	}
	for _, status := range []int{301, 302, 307, 405, 408, 409, 418} {
		err = newError(status, http.Header{}, []byte(`{"detail":{"error_type":"other","message":"no"}}`))
		if e, ok := err.(*Error); !ok || e.Status != status || e.Error() != "no" {
			t.Fatalf("%d: got %#v", status, err)
		}
	}
	err = newError(403, http.Header{}, []byte(`{"detail":{"error_type":"permission_error","message":"no"}}`))
	if _, ok := err.(*PermissionDeniedError); !ok {
		t.Fatalf("got %#v", err)
	}
}

func TestRetryAfterCappedAt60s(t *testing.T) {
	cases := []http.Header{
		{"Retry-After-Ms": {"600000"}},
		{"Retry-After": {"3600"}},
		{"Retry-After": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}},
		{"Retry-After-Ms": {"1e300"}},
	}
	for _, h := range cases {
		if d, ok := retryAfter(h); !ok || d != 60*time.Second {
			t.Errorf("%v: got %v, %v", h, d, ok)
		}
	}
	d, ok := retryAfter(http.Header{"Retry-After": {time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)}})
	if !ok || d <= time.Second || d > 3*time.Second {
		t.Errorf("date: got %v, %v", d, ok)
	}
}
