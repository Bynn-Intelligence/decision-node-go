package decisionnode_test

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

const tokenSharesBody = `{"model":"decisionnode-flash","answers":{"q":{"type":"truth","truth":0.9}},"usage":{"input_tokens":1300,"output_tokens":0}}`

// decideWith answers one Decide with tokenSharesBody and the headers given.
func decideWith(t *testing.T, headers map[string]string) *dn.Decision {
	t.Helper()
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, tokenSharesBody, headers)
	})
	d, err := client(t, srv.URL, dn.WithMaxRetries(0)).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.InputTokens == nil || *d.Usage.InputTokens != 1300 {
		t.Fatalf("input tokens changed: %v", d.Usage.InputTokens)
	}
	if _, ok := d.Raw["usage"].(map[string]any)["image_tokens"]; ok {
		t.Fatal("raw is the body as sent")
	}
	return d
}

func intPtrString(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

func TestImageAndVideoTokensFromTheirHeaders(t *testing.T) {
	d := decideWith(t, map[string]string{"x-decisionnode-image-tokens": "1024", "x-decisionnode-video-tokens": "256"})
	if d.Usage.ImageTokens == nil || *d.Usage.ImageTokens != 1024 || d.Usage.VideoTokens == nil || *d.Usage.VideoTokens != 256 {
		t.Fatalf("got image %s, video %s", intPtrString(d.Usage.ImageTokens), intPtrString(d.Usage.VideoTokens))
	}
	d = decideWith(t, nil)
	if d.Usage.ImageTokens != nil || d.Usage.VideoTokens != nil {
		t.Fatalf("absent: got image %s, video %s", intPtrString(d.Usage.ImageTokens), intPtrString(d.Usage.VideoTokens))
	}

	good := map[string]int{"512": 512, "0": 0, "  77 ": 77, "2147483647": 2147483647}
	bad := []string{"", "   ", "-1", "-0", "+5", "12.0", "1e3", "abc", "12 3", "0x10", "2147483648", "99999999999999999999999"}
	for _, name := range []string{"x-decisionnode-image-tokens", "x-decisionnode-video-tokens"} {
		read := func(value string) *int {
			d := decideWith(t, map[string]string{name: value})
			own, other := d.Usage.ImageTokens, d.Usage.VideoTokens
			if name == "x-decisionnode-video-tokens" {
				own, other = other, own
			}
			if other != nil {
				t.Fatalf("%s=%q set the other share: %d", name, value, *other)
			}
			return own
		}
		for value, want := range good {
			if got := read(value); got == nil || *got != want {
				t.Errorf("%s=%q: got %s, want %d", name, value, intPtrString(got), want)
			}
		}
		for _, value := range bad {
			if got := read(value); got != nil {
				t.Errorf("%s=%q: got %d, want nil", name, value, *got)
			}
		}
	}
}

func TestTokenSharesNeverReadFromTheBody(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"m","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":10,"image_tokens":4,"video_tokens":3}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.ImageTokens != nil || d.Usage.VideoTokens != nil {
		t.Fatalf("got image %s, video %s", intPtrString(d.Usage.ImageTokens), intPtrString(d.Usage.VideoTokens))
	}
}

func TestBatchResultTokenSharesReadEmpty(t *testing.T) {
	line := `{"custom_id":"a","status":200,"body":{"model":"decisionnode-flash","answers":{"q":{"type":"truth","truth":0.88}},"usage":{"input_tokens":1300,"output_tokens":0}},"headers":{"x-request-id":"req_1"}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, line+"\n")
	})
	got, err := client(t, srv.URL).Batches().Results(context.Background(), "batch_0123456789")
	if err != nil || len(got) != 1 || got[0].Decision == nil {
		t.Fatal(got, err)
	}
	u := got[0].Decision.Usage
	if !reflect.DeepEqual([]*int{u.ImageTokens, u.VideoTokens}, []*int{nil, nil}) {
		t.Fatalf("got image %s, video %s", intPtrString(u.ImageTokens), intPtrString(u.VideoTokens))
	}
}
