package decisionnode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The API's fixed video limits, checked locally before any request.
const (
	// MaxVideos is the most videos one request (or batch line) may carry.
	MaxVideos = 4
	// MaxVideoFPS is the highest rate, in frames a second, a video may ask
	// its frames to be read at.
	MaxVideoFPS = 4
)

// Video is a video sent beside the state as the frames the caller sampled,
// each with its time. Every question sees every video, the images and the
// state. The server reads the frames as one video, in order and with their
// times, not as separate images. Only frames are sent: no video file, no
// video URL and no sound.
//
// Put a Video, or a video in its JSON wire form (such as a map[string]any),
// in Request.Videos.
type Video struct {
	// ID names the video: a non-empty string, unique across the request's
	// images and videos. Text in the state or the instructions can refer to
	// the video by it.
	ID string
	// Frames are the video's frames, at least one, their times strictly
	// increasing. Build each with VideoFrameData or VideoFrameBase64.
	Frames []VideoFrame
	// FPS caps the rate the frames are read at, in frames a second: above 0
	// and at most 4. Nil leaves it to the server (2).
	FPS *float64
	// Start and End bound the window [Start, End) read, on the frames' own
	// times: a frame is read when Start <= t < End. End must be after Start
	// (0 when nil). Nil leaves a bound out: by default every frame is read.
	Start, End *float64
}

// VideoFrame is one frame of a Video: an image (JPEG, PNG or WebP) and its
// time in seconds. Frames always travel inline as base64: they are never
// uploaded, never cached and never resized or re-encoded.
type VideoFrame struct {
	t         float64
	mediaType string
	data      []byte
	text      string
	isText    bool
}

// VideoFrameData is a frame from image bytes taken t seconds into the video.
// An empty mediaType is taken from the first bytes (JPEG, PNG or WebP); a
// given one must be image/jpeg, image/png or image/webp. The slice is not
// copied: do not change it while a call that uses the frame runs.
func VideoFrameData(t float64, data []byte, mediaType string) VideoFrame {
	return VideoFrame{t: t, data: data, mediaType: mediaType}
}

// VideoFrameBase64 is a frame from image bytes given as base64 text, sent
// exactly as given, taken t seconds into the video. mediaType is required:
// image/jpeg, image/png or image/webp.
func VideoFrameBase64(t float64, text, mediaType string) VideoFrame {
	return VideoFrame{t: t, text: text, isText: true, mediaType: mediaType}
}

// T returns the frame's time in seconds.
func (f VideoFrame) T() float64 { return f.t }

// MediaType returns the declared media type, else the one found in the
// frame's first bytes, else "".
func (f VideoFrame) MediaType() string {
	if f.mediaType != "" || f.isText {
		return f.mediaType
	}
	return sniffMediaType(f.data)
}

// encodedVideo is one video of a request: a typed Video, or a video in its
// JSON wire form, encoded once.
type encodedVideo struct {
	typed *Video
	raw   []byte
}

// encodeVideos reads a request's videos before any check: a value that is
// neither a Video nor a JSON object is an argument error. It returns the
// deepest nesting of the videos inside the body (0 for none).
func encodeVideos(videos []any) ([]encodedVideo, int, error) {
	if len(videos) == 0 {
		return nil, 0, nil
	}
	out := make([]encodedVideo, len(videos))
	depth := 0
	for i, v := range videos {
		switch x := v.(type) {
		case Video:
			out[i].typed = &x
			// {"frames": [{...}]}
			depth = max(depth, 3)
		case *Video:
			if x == nil {
				return nil, 0, fmt.Errorf("decisionnode: video %d is a nil *Video", i)
			}
			out[i].typed = x
			depth = max(depth, 3)
		default:
			raw, err := marshal(v)
			if err != nil {
				return nil, 0, encodeError(fmt.Errorf("video %d: %w", i, err))
			}
			if raw = bytes.TrimSpace(raw); len(raw) == 0 || raw[0] != '{' {
				return nil, 0, fmt.Errorf("decisionnode: video %d must be a decisionnode.Video or a JSON object, got %s", i, jsonKind(raw))
			}
			out[i].raw = raw
			depth = max(depth, jsonDepth(raw))
		}
	}
	// The body's own object and the videos array hold each video.
	return out, depth + 2, nil
}

// checkVideos checks a request's videos in the server's order, after its
// images: the count, then each video in turn, ids unique across the images
// and the videos. It returns the body's `,"videos":[...]` member, or nothing
// when there are no videos. A video the API would refuse is its own
// *BadRequestError.
func checkVideos(videos []encodedVideo, images []Image, lim limits) ([]byte, error) {
	if len(videos) == 0 {
		return nil, nil
	}
	most := MaxVideos
	if lim.maxVideos > 0 {
		most = lim.maxVideos
	}
	if len(videos) > most {
		msg := fmt.Sprintf("Too many videos: %d, the limit is %d.", len(videos), most)
		return nil, localBadRequest("api_usage_error", msg, fmt.Sprintf("%d videos, the limit is %d", len(videos), most))
	}
	seen := make(map[string]bool, len(images)+len(videos))
	for _, img := range images {
		seen[img.id] = true
	}
	types := make([][]string, len(videos))
	size := len(`,"videos":[]`) + len(videos)
	for i, v := range videos {
		if v.typed == nil {
			ms, _ := objectMembers(v.raw)
			if err := checkVideoJSON(i, ms, seen); err != nil {
				return nil, err
			}
			size += len(v.raw)
			continue
		}
		t, err := v.typed.check(i, seen)
		if err != nil {
			return nil, err
		}
		types[i] = t
		size += v.typed.wireSize()
	}
	out := make([]byte, 0, size)
	out = append(out, `,"videos":[`...)
	for i, v := range videos {
		if i > 0 {
			out = append(out, ',')
		}
		if v.typed == nil {
			out = append(out, v.raw...)
		} else {
			out = v.typed.appendWire(out, types[i])
		}
	}
	return append(out, ']'), nil
}

// check checks a typed video and returns the media type of each frame.
func (v *Video) check(i int, seen map[string]bool) ([]string, error) {
	if v.ID == "" {
		return nil, videoError(fmt.Sprintf("Video %d needs an id.", i), fmt.Sprintf("video %d has no id", i))
	}
	if err := videoID(v.ID, seen); err != nil {
		return nil, err
	}
	if len(v.Frames) == 0 {
		return nil, videoProblem(v.ID, "frames must be a non-empty list of frames")
	}
	types := make([]string, len(v.Frames))
	for n, f := range v.Frames {
		if !finite(f.t) || f.t < 0 {
			return nil, videoProblem(v.ID, fmt.Sprintf("frame %d: t must be a number of seconds, at least 0", n))
		}
		if n > 0 && f.t <= v.Frames[n-1].t {
			return nil, videoProblem(v.ID, fmt.Sprintf("frame %d: t must be larger than the frame before it (%s)", n, decimal(v.Frames[n-1].t)))
		}
		types[n] = f.MediaType()
		if !mediaOK[types[n]] {
			return nil, videoProblem(v.ID, fmt.Sprintf("frame %d: media_type must be one of image/jpeg, image/png, image/webp", n))
		}
	}
	if err := checkWindow(v.ID, v.FPS, v.Start, v.End); err != nil {
		return nil, err
	}
	return types, nil
}

// wireSize is about the size of the video's wire form, for one allocation.
func (v *Video) wireSize() int {
	n := 96 + 2*len(v.ID)
	for _, f := range v.Frames {
		n += 64 + 2*len(f.text) + base64.StdEncoding.EncodedLen(len(f.data))
	}
	return n
}

// appendWire writes a checked video: {"id", "fps", "start", "end",
// "frames"}, the optional numbers only when given, each frame {"t",
// "media_type", "data"}.
func (v *Video) appendWire(out []byte, types []string) []byte {
	out = appendIDMember(append(out, '{'), v.ID)
	for _, m := range []struct {
		name  string
		value *float64
	}{{"fps", v.FPS}, {"start", v.Start}, {"end", v.End}} {
		if m.value != nil {
			out = append(out, `,"`+m.name+`":`...)
			out = strconv.AppendFloat(out, *m.value, 'f', -1, 64)
		}
	}
	out = append(out, `,"frames":[`...)
	for n, f := range v.Frames {
		if n > 0 {
			out = append(out, ',')
		}
		out = append(out, `{"t":`...)
		out = strconv.AppendFloat(out, f.t, 'f', -1, 64)
		out = append(out, `,"media_type":`...)
		mt, _ := marshal(types[n])
		out = append(out, mt...)
		out = append(out, `,"data":`...)
		if f.isText {
			b, _ := marshal(f.text)
			out = append(out, b...)
		} else {
			out = append(out, '"')
			out = base64.StdEncoding.AppendEncode(out, f.data)
			out = append(out, '"')
		}
		out = append(out, '}')
	}
	return append(out, "]}"...)
}

// checkVideoJSON checks a video given in its JSON wire form by the same
// rules, in the server's order.
func checkVideoJSON(i int, v members, seen map[string]bool) error {
	var id string
	if json.Unmarshal(v.get("id"), &id) != nil || id == "" {
		return videoError(fmt.Sprintf("Video %d needs an id.", i), fmt.Sprintf("video %d has no id", i))
	}
	if err := videoID(id, seen); err != nil {
		return err
	}
	forms := 0
	for _, name := range []string{"data", "url", "frames"} {
		if !isNull(v.get(name)) {
			forms++
		}
	}
	if forms > 1 {
		return videoProblem(id, "needs exactly one of data, url and frames")
	}
	frames, ok := arrayElements(v.get("frames"))
	if !ok || len(frames) == 0 {
		return videoProblem(id, "frames must be a non-empty list of frames")
	}
	var before float64
	for n, raw := range frames {
		f, ok := objectMembers(raw)
		if !ok {
			return videoProblem(id, fmt.Sprintf("frame %d must be an object with t, media_type and data", n))
		}
		t, ok := jsonFloat(f.get("t"))
		if !ok || t < 0 {
			return videoProblem(id, fmt.Sprintf("frame %d: t must be a number of seconds, at least 0", n))
		}
		if n > 0 && t <= before {
			return videoProblem(id, fmt.Sprintf("frame %d: t must be larger than the frame before it (%s)", n, decimal(before)))
		}
		before = t
		if d := bytes.TrimSpace(f.get("data")); len(d) == 0 || d[0] != '"' {
			return videoProblem(id, fmt.Sprintf("frame %d: data must be base64 text", n))
		}
		var mt string
		if json.Unmarshal(f.get("media_type"), &mt) != nil || !mediaOK[mt] {
			return videoProblem(id, fmt.Sprintf("frame %d: media_type must be one of image/jpeg, image/png, image/webp", n))
		}
	}
	var bounds [3]*float64
	for k, name := range []string{"fps", "start", "end"} {
		raw := v.get(name)
		if isNull(raw) {
			continue
		}
		x, ok := jsonFloat(raw)
		if !ok {
			// Not a number: refused below as one out of range.
			x = math.NaN()
		}
		bounds[k] = &x
	}
	return checkWindow(id, bounds[0], bounds[1], bounds[2])
}

// checkWindow checks a video's rate and window.
func checkWindow(id string, fps, start, end *float64) error {
	if fps != nil && (!finite(*fps) || *fps <= 0 || *fps > MaxVideoFPS) {
		return videoProblem(id, fmt.Sprintf("fps must be a number above 0 and at most %d", MaxVideoFPS))
	}
	for _, b := range []struct {
		name  string
		value *float64
	}{{"start", start}, {"end", end}} {
		if b.value != nil && (!finite(*b.value) || *b.value < 0) {
			return videoProblem(id, b.name+" must be a number of seconds, at least 0")
		}
	}
	from := 0.0
	if start != nil {
		from = *start
	}
	if end != nil && *end <= from {
		return videoProblem(id, "end must be after start")
	}
	return nil
}

func videoID(id string, seen map[string]bool) error {
	if seen[id] {
		return videoError(fmt.Sprintf("Video id %s is used twice (ids are unique across images and videos).", pyRepr(id)),
			fmt.Sprintf("video id %s is used twice", pyRepr(id)))
	}
	seen[id] = true
	return nil
}

func videoProblem(id, problem string) error {
	return videoError(fmt.Sprintf("Video %s: %s.", pyRepr(id), problem), fmt.Sprintf("video %s: %s", pyRepr(id), problem))
}

func videoError(message, reason string) error {
	return localBadRequest("api_usage_error", message, reason)
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// jsonFloat reads a JSON number (never a boolean or a string), finite.
func jsonFloat(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !(raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return 0, false
	}
	x, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || !finite(x) {
		return 0, false
	}
	return x, true
}

// decimal writes a number as the API's messages do, as printf's %g: six
// significant digits without trailing zeros (1.5, 4, 1.69679e+09).
func decimal(x float64) string { return strconv.FormatFloat(x, 'g', 6, 64) }

// pyRepr quotes an id as the API's messages do: in single quotes, or in
// double quotes when it holds a single quote and no double quote, with
// backslashes, the quote and characters that do not print escaped.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i-1])
		case r == '\\' || r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f || (r > 0x7f && r < 0xa0):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == ' ' || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
