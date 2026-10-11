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
	var tooMany *dn.ValidationError
	if !errors.As(err, &tooMany) || tooMany.Status != 0 || len(tooMany.Entries) != 1 {
		t.Fatalf("17 images: %v", err)
	}
	entry := tooMany.Entries[0].(map[string]any)
	given := entry["input"].([]any)
	if entry["type"] != "too_long" || fmt.Sprint(entry["loc"]) != "[body images]" ||
		entry["msg"] != "List should have at most 16 items after validation, not 17" || len(given) != 17 ||
		fmt.Sprint(given[0]) != "map[id:i0 media_type:image/png]" ||
		fmt.Sprint(entry["ctx"]) != "map[actual_length:17 field_type:List max_length:16]" {
		t.Fatalf("the entry: %v", entry)
	}
	// One presign takes 16 files: Upload refuses the 17th with the presign's 400.
	_, err = c.Upload(context.Background(), imgs)
	var bad *dn.BadRequestError
	if !errors.As(err, &bad) || bad.Error() != "files: 1 to 16 files, got 17." || bad.ErrorType != "api_usage_error" {
		t.Fatalf("upload of 17: %v", err)
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
	// Five videos are the API's 422 too_long, listed after a question's entries.
	five := []any{ok, dn.Video{ID: "b", Frames: ok.Frames}, dn.Video{ID: "c", Frames: ok.Frames}, dn.Video{ID: "d", Frames: ok.Frames},
		map[string]any{"id": "e", "frames": []any{}}}
	err := c.CheckDecide(dn.Request{Questions: dn.Questions{dn.Q("route", map[string]any{"type": "choice", "instructions": "?"})}, Videos: five})
	var tooMany *dn.ValidationError
	if !errors.As(err, &tooMany) || len(tooMany.Entries) != 2 {
		t.Fatalf("five videos: %v", err)
	}
	if e := tooMany.Entries[1].(map[string]any); e["type"] != "too_long" || fmt.Sprint(e["loc"]) != "[body videos]" ||
		len(e["input"].([]any)) != 5 || tooMany.Entries[0].(map[string]any)["type"] != "missing" {
		t.Fatalf("five videos: %v", tooMany.Entries)
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
	err = c.CheckDecide(dn.Request{Questions: oneTruth(), Videos: []any{"dock.mp4"}})
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
		respond(w, 200, `{"model":"decisionnode-moe","answers":{"check":{"type":"truth","truth":0.5}}}`, nil)
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
		respond(w, 200, `{"model":"decisionnode-moe","answers":{"q":{"type":"truth","truth":0.5}},"usage":{"input_tokens":1300,"output_tokens":0,"video_frames":20,"video_seconds":10.000000000000002},"warnings":["Video 'dock' was read at 1 frame a second."]}`, nil)
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
	if dn.MaxImages != 16 || dn.MaxVideos != 4 {
		t.Fatal(dn.MaxImages, dn.MaxVideos)
	}
}

func b64(b []byte) string {
	out, _ := json.Marshal(b)
	return strings.Trim(string(out), `"`)
}

func TestPointAndBoxWireFormsAndAnswers(t *testing.T) {
	srv, rec := server(t, func(w http.ResponseWriter, r *http.Request, n int) {
		respond(w, 200, `{"model":"decisionnode-moe","answers":{
			"pick":{"type":"point","x":812,"y":455,"present":0.97,"confidence":0.91,"image":"belt","width":1920,"height":1080},
			"damage":{"type":"box","x1":0.1563,"y1":0.1111,"x2":0.2708,"y2":0.3796,"present":0.95,"confidence":0.88,"image":"belt","width":1920,"height":1080},
			"odd":{"type":"point","x":true,"y":455,"present":0.97,"confidence":0.91,"image":"belt","width":1920,"height":1080}}}`, nil)
	})
	d, err := client(t, srv.URL).Decide(context.Background(), dn.Request{
		Images: []dn.Image{dn.ImageURL("belt", "https://files.example.com/line-3/cam-2/0917.jpg")},
		Questions: dn.Questions{
			dn.Q("pick", dn.Point("Point to the yellow package closest to the robot arm.")),
			dn.Q("damage", dn.BoxWith("Draw a box around the damaged package.", "belt", "normalized")),
			dn.Q("odd", map[string]any{"type": "point", "instructions": "?"}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(rec.bodies[0])
	for _, want := range []string{
		`"pick":{"type":"point","instructions":"Point to the yellow package closest to the robot arm."}`,
		`"damage":{"type":"box","instructions":"Draw a box around the damaged package.","image":"belt","units":"normalized"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("want %s in %s", want, body)
		}
	}
	p, b := d.Point("pick"), d.Box("damage")
	if p == nil || p.X != 812 || p.Y != 455 || p.Present != 0.97 || p.Confidence != 0.91 || p.Image != "belt" || p.Width != 1920 || p.Height != 1080 {
		t.Fatalf("point: %+v", p)
	}
	if b == nil || b.X1 != 0.1563 || b.Y2 != 0.3796 || b.Image != "belt" {
		t.Fatalf("box: %+v", b)
	}
	if _, ok := d.Answer("odd").(*dn.UnknownAnswer); !ok {
		t.Fatalf("a boolean coordinate: %T", d.Answer("odd"))
	}
}

func TestPointAndBoxLocalChecks(t *testing.T) {
	c := dn.NewClient(dn.WithAPIKey("dn_test"))
	belt := dn.ImageURL("belt", "https://files.example.com/belt.jpg")
	dock := dn.ImageURL("dock", "https://files.example.com/dock.jpg")
	entries := func(req dn.Request) []any {
		t.Helper()
		var v *dn.ValidationError
		if err := c.CheckDecide(req); !errors.As(err, &v) || v.Status != 0 {
			t.Fatalf("got %v", err)
		}
		return v.Entries
	}
	types := func(es []any) string {
		var out []string
		for _, e := range es {
			m := e.(map[string]any)
			loc := fmt.Sprint(m["loc"])
			out = append(out, m["type"].(string)+" "+loc)
		}
		return strings.Join(out, "; ")
	}
	got := types(entries(dn.Request{Questions: dn.Questions{dn.Q("pick", dn.Point("Where?"))}}))
	if got != "image_required [body questions pick point]" {
		t.Error(got)
	}
	got = types(entries(dn.Request{
		Images: []dn.Image{belt, dock},
		Videos: []any{dn.Video{ID: "cam", Frames: []dn.VideoFrame{dn.VideoFrameBase64(0, "AA==", "image/png")}}},
		Questions: dn.Questions{
			dn.Q("pick", dn.PointWith("Where?", "cam", "inches")),
			dn.Q("any", dn.Truth("Damaged?")),
			dn.Q("damage", dn.Box("Where?")),
			dn.Q("raw", map[string]any{"type": "box", "instructions": "?", "image": 7}),
		},
	}))
	want := "unknown_image [body questions pick point image]; literal_error [body questions pick point units]; " +
		"missing [body questions damage box image]; string_type [body questions raw box image]"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	// One image needs no image field; the instructions are the plain 400.
	if err := c.CheckDecide(dn.Request{Images: []dn.Image{belt}, Questions: dn.Questions{dn.Q("pick", dn.Point("Where?"))}}); err != nil {
		t.Fatal(err)
	}
	var bad *dn.BadRequestError
	err := c.CheckDecide(dn.Request{Images: []dn.Image{belt}, Questions: dn.Questions{dn.Q("pick", dn.Box(nil))}})
	if !errors.As(err, &bad) || bad.Error() != "Box question must have instructions: pick" || bad.ErrorType != "" {
		t.Fatalf("no instructions: %v", err)
	}
	// A session's images come with its frames: no image rule at open.
	if err := c.CheckSession(dn.SessionRequest{Questions: dn.Questions{dn.Q("pick", dn.Point("Where?"))}}); err != nil {
		t.Fatal(err)
	}
}
