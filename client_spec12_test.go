package decisionnode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

func dockVideo(t *testing.T) dn.Video {
	jpg := fixture(t, "3x2.jpg")
	return dn.Video{
		ID:     "dock",
		FPS:    dn.Ptr(2.0),
		Frames: []dn.VideoFrame{dn.VideoFrameData(30, jpg, ""), dn.VideoFrameData(30.5, jpg, "")},
	}
}

func TestSixteenImagesInlineAndTheSeventeenthRefused(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL)
	png := fixture(t, "3x2.png")
	var imgs []dn.Image
	for i := 0; i < 17; i++ {
		imgs = append(imgs, dn.ImageData(fmt.Sprintf("i%d", i), png, ""))
	}
	_, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Images: imgs})
	var bad *dn.BadRequestError
	if !errors.As(err, &bad) || bad.Error() != "Too many images: 17, the limit is 16." || bad.ErrorType != "api_usage_error" || bad.Status != 0 {
		t.Fatalf("17 images: %v", err)
	}
	if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Images: imgs[:16]}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.decides) != 1 || s.presigns != 0 || strings.Count(string(s.decides[0]), `"media_type":"image/png"`) != 16 {
		t.Fatalf("%d decisions, %d presigns", len(s.decides), s.presigns)
	}
}

func TestSixteenUploadsInOnePresign(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL)
	var imgs []dn.Image
	for i := 0; i < 16; i++ {
		// Distinct bytes, so none is a cache hit of another.
		imgs = append(imgs, dn.ImageData(fmt.Sprintf("i%d", i), bigPNG(t, 40_000+i), ""))
	}
	if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Images: imgs}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presigns != 1 || len(s.puts) != 16 || strings.Count(string(s.decides[0]), `"upload_id"`) != 16 {
		t.Fatalf("%d presigns, %d puts", s.presigns, len(s.puts))
	}
}

func TestVideoWireForm(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL)
	v := dockVideo(t)
	v.Start, v.End = dn.Ptr(30.0), dn.Ptr(31.0)
	text := dn.VideoFrameBase64(0.25, "AAEC", "image/webp")
	wire := map[string]any{"id": "yard", "frames": []any{map[string]any{"t": 1, "media_type": "image/png", "data": "AA=="}}}
	_, err := c.Decide(context.Background(), dn.Request{
		State:     "Warehouse camera 4, loading dock.",
		Questions: dn.Questions{dn.Q("fall", dn.Truth("Does a person fall in the video?"))},
		Videos:    []any{v, &dn.Video{ID: "gate", Frames: []dn.VideoFrame{text}}, wire},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var body struct {
		Images []any            `json:"images"`
		Videos []map[string]any `json:"videos"`
	}
	if err := json.Unmarshal(s.decides[0], &body); err != nil {
		t.Fatal(err)
	}
	if body.Images != nil || len(body.Videos) != 3 || s.presigns != 0 {
		t.Fatalf("body: %.300s", s.decides[0])
	}
	jpg := fixture(t, "3x2.jpg")
	want := []string{
		fmt.Sprintf(`{"id":"dock","fps":2,"start":30,"end":31,"frames":[{"t":30,"media_type":"image/jpeg","data":%q},{"t":30.5,"media_type":"image/jpeg","data":%q}]}`,
			b64(jpg), b64(jpg)),
		`{"id":"gate","frames":[{"t":0.25,"media_type":"image/webp","data":"AAEC"}]}`,
		`{"frames":[{"data":"AA==","media_type":"image/png","t":1}],"id":"yard"}`,
	}
	for i, w := range want {
		if !strings.Contains(string(s.decides[0]), w) {
			t.Errorf("video %d: want %s in %.400s", i, w, s.decides[0])
		}
	}
}

func TestVideoLocalChecks(t *testing.T) {
	c := dn.NewClient(dn.WithAPIKey("dn_test"))
	jpg := fixture(t, "3x2.jpg")
	frame := func(t float64) dn.VideoFrame { return dn.VideoFrameData(t, jpg, "") }
	ok := dn.Video{ID: "dock", Frames: []dn.VideoFrame{frame(0)}}
	cases := []struct {
		name   string
		images []dn.Image
		videos []any
		msg    string
	}{
		{"five videos", nil, []any{ok, dn.Video{ID: "b", Frames: ok.Frames}, dn.Video{ID: "c", Frames: ok.Frames}, dn.Video{ID: "d", Frames: ok.Frames}, dn.Video{ID: "e", Frames: ok.Frames}},
			"Too many videos: 5, the limit is 4."},
		{"no id", nil, []any{dn.Video{Frames: ok.Frames}}, "Video 0 needs an id."},
		{"id of an image", []dn.Image{dn.ImageData("dock", jpg, "")}, []any{ok}, "Video id 'dock' is used twice (ids are unique across images and videos)."},
		{"id twice", nil, []any{ok, ok}, "Video id 'dock' is used twice (ids are unique across images and videos)."},
		{"no frames", nil, []any{dn.Video{ID: "dock"}}, "Video 'dock': frames must be a non-empty list of frames."},
		{"negative t", nil, []any{dn.Video{ID: "dock", Frames: []dn.VideoFrame{frame(-1)}}}, "Video 'dock': frame 0: t must be a number of seconds, at least 0."},
		{"t not after", nil, []any{dn.Video{ID: "dock", Frames: []dn.VideoFrame{frame(0.5), frame(1.5), frame(1.5)}}},
			"Video 'dock': frame 2: t must be larger than the frame before it (1.5)."},
		{"gif frame", nil, []any{dn.Video{ID: "dock", Frames: []dn.VideoFrame{dn.VideoFrameData(0, []byte("GIF89a...."), "")}}},
			"Video 'dock': frame 0: media_type must be one of image/jpeg, image/png, image/webp."},
		{"base64 without a type", nil, []any{dn.Video{ID: "dock", Frames: []dn.VideoFrame{dn.VideoFrameBase64(0, "AAAA", "")}}},
			"Video 'dock': frame 0: media_type must be one of image/jpeg, image/png, image/webp."},
		{"fps 5", nil, []any{dn.Video{ID: "dock", Frames: ok.Frames, FPS: dn.Ptr(5.0)}}, "Video 'dock': fps must be a number above 0 and at most 4."},
		{"fps 0", nil, []any{dn.Video{ID: "dock", Frames: ok.Frames, FPS: dn.Ptr(0.0)}}, "Video 'dock': fps must be a number above 0 and at most 4."},
		{"negative start", nil, []any{dn.Video{ID: "dock", Frames: ok.Frames, Start: dn.Ptr(-1.0)}}, "Video 'dock': start must be a number of seconds, at least 0."},
		{"end 0", nil, []any{dn.Video{ID: "dock", Frames: ok.Frames, End: dn.Ptr(0.0)}}, "Video 'dock': end must be after start."},
		{"end before start", nil, []any{dn.Video{ID: "dock", Frames: ok.Frames, Start: dn.Ptr(3.0), End: dn.Ptr(2.0)}}, "Video 'dock': end must be after start."},
		{"a quote in the id", nil, []any{dn.Video{ID: "it's", Frames: nil}}, `Video "it's": frames must be a non-empty list of frames.`},
		{"wire: url form", nil, []any{map[string]any{"id": "cam", "url": "https://example.com/a.mp4"}}, "Video 'cam': frames must be a non-empty list of frames."},
		{"wire: two forms", nil, []any{map[string]any{"id": "cam", "url": "https://example.com/a.mp4", "frames": []any{}}}, "Video 'cam': needs exactly one of data, url and frames."},
		{"wire: frame not an object", nil, []any{map[string]any{"id": "cam", "frames": []any{1}}}, "Video 'cam': frame 0 must be an object with t, media_type and data."},
		{"wire: t a string", nil, []any{map[string]any{"id": "cam", "frames": []any{map[string]any{"t": "0", "media_type": "image/png", "data": "AA=="}}}},
			"Video 'cam': frame 0: t must be a number of seconds, at least 0."},
		{"wire: t a boolean", nil, []any{map[string]any{"id": "cam", "frames": []any{map[string]any{"t": true, "media_type": "image/png", "data": "AA=="}}}},
			"Video 'cam': frame 0: t must be a number of seconds, at least 0."},
		{"wire: data not text", nil, []any{map[string]any{"id": "cam", "frames": []any{map[string]any{"t": 0, "media_type": "image/png", "data": 1}}}},
			"Video 'cam': frame 0: data must be base64 text."},
		{"wire: fps a string", nil, []any{map[string]any{"id": "cam", "fps": "2", "frames": []any{map[string]any{"t": 0, "media_type": "image/png", "data": "AA=="}}}},
			"Video 'cam': fps must be a number above 0 and at most 4."},
	}
	for _, tc := range cases {
		err := c.CheckDecide(dn.Request{Questions: oneTruth(), Images: tc.images, Videos: tc.videos})
		var bad *dn.BadRequestError
		if !errors.As(err, &bad) || bad.Error() != tc.msg || bad.ErrorType != "api_usage_error" || bad.Status != 0 || bad.RequestID != "" {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.msg)
		}
	}
	// Not checked locally: the frame count, the bytes against their type.
	many := make([]dn.VideoFrame, 65)
	for i := range many {
		many[i] = dn.VideoFrameData(float64(i)/2, []byte("not checked"), "image/jpeg")
	}
	if err := c.CheckDecide(dn.Request{Questions: oneTruth(), Videos: []any{dn.Video{ID: "dock", Frames: many}}}); err != nil {
		t.Errorf("65 frames: %v", err)
	}
	// A value that is not a video is an argument error, not an API error.
	err := c.CheckDecide(dn.Request{Questions: oneTruth(), Videos: []any{"dock.mp4"}})
	var apiErr *dn.Error
	if err == nil || errors.As(err, &apiErr) {
		t.Errorf("a string video: %v", err)
	}
}

func TestVideosInBatchLinesAndHelpers(t *testing.T) {
	s := newStorageServer(t)
	c := client(t, s.srv.URL)
	if _, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth(), Videos: []any{dockVideo(t)}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	body := string(s.decides[0])
	s.mu.Unlock()
	if !strings.Contains(body, `"videos":[{"id":"dock","fps":2,"frames":[{"t":30,`) {
		t.Fatalf("decide body: %.300s", body)
	}
	helper, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"decisionnode-1.0","answers":{"check":{"type":"truth","truth":0.5}}}`, nil)
	})
	if _, err := client(t, helper.URL).Check(context.Background(), dn.CheckRequest{Instructions: "A fall?", Videos: []any{dockVideo(t)}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec.bodies[0]), `"videos":[{"id":"dock"`) {
		t.Fatalf("check body: %.300s", rec.bodies[0])
	}
	err := c.CheckBatchCreate([]dn.BatchRequest{{CustomID: dn.Ptr("a"), Request: dn.Request{Questions: oneTruth(), Videos: []any{dn.Video{ID: "x"}}}}})
	var bad *dn.BadRequestError
	if !errors.As(err, &bad) || bad.Error() != "Video 'x': frames must be a non-empty list of frames." {
		t.Fatalf("batch line: %v", err)
	}
}

func TestVideoUsageAndWarnings(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"decisionnode-1.0","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":1300,"output_tokens":0,"video_frames":20,"video_seconds":10.000000000000002},"warnings":["Video 'dock' was read at 1 frame a second."]}`, nil)
	})
	c := client(t, srv.URL)
	d, err := c.Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil {
		t.Fatal(err)
	}
	if *d.Usage.VideoFrames != 20 || *d.Usage.VideoSeconds != 10.000000000000002 || len(d.Warnings) != 1 || d.Warnings[0] != "Video 'dock' was read at 1 frame a second." {
		t.Fatalf("%+v %v", d.Usage, d.Warnings)
	}
	plain, _ := server(t, func(w http.ResponseWriter, r *http.Request, n int) { respond(w, 200, truthBody, nil) })
	d, err = client(t, plain.URL).Decide(context.Background(), dn.Request{Questions: oneTruth()})
	if err != nil || d.Usage.VideoFrames != nil || d.Usage.VideoSeconds != nil || d.Warnings == nil || len(d.Warnings) != 0 {
		t.Fatalf("no videos: %+v %#v %v", d.Usage, d.Warnings, err)
	}
	if dn.SpecVersion != "1.2" || dn.MaxImages != 16 || dn.MaxVideos != 4 {
		t.Fatal(dn.SpecVersion, dn.MaxImages, dn.MaxVideos)
	}
}

func b64(b []byte) string {
	out, _ := json.Marshal(b)
	return strings.Trim(string(out), `"`)
}
