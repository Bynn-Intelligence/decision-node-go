package decisionnode

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
)

// prepared is a request checked against the API's fixed rules and encoded up
// to its images, which are placed last: inline, by URL or by upload_id, as
// the inline threshold and the upload cache decide at send time.
type prepared struct {
	// head is the body without its videos, its images and its closing brace:
	// up to its questions and boosters.
	head []byte
	// videos is the `,"videos":[...]` member, or empty. Frames always go
	// inline: they are never uploaded or cached.
	videos []byte
	images []Image
}

// body returns the full body with the images' wire forms.
func (p *prepared) body(wires [][]byte) []byte {
	n := len(p.head) + len(p.videos) + 1
	if len(wires) > 0 {
		n += len(`,"images":[]`) + len(wires) - 1
		for _, w := range wires {
			n += len(w)
		}
	}
	out := make([]byte, 0, n)
	out = append(out, p.head...)
	out = append(out, p.videos...)
	if len(wires) > 0 {
		out = append(out, `,"images":[`...)
		for i, w := range wires {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, w...)
		}
		out = append(out, ']')
	}
	return append(out, '}')
}

// prepare checks a decision request and encodes it, without sending
// anything. A problem with the arguments themselves (no questions, a value
// that cannot be encoded) is a plain error; a request the API would refuse
// for its content is the API's own error class. customID, when not nil,
// makes the body a batch line.
func (c *Client) prepare(req Request, customID *string) (*prepared, error) {
	// A batch line's custom_id is checked by checkLines before this.
	if len(req.Questions) == 0 {
		return nil, errors.New("decisionnode: questions must hold at least one question")
	}
	model := c.model
	if req.Model != "" {
		model = req.Model
	}
	state := req.State
	if state == nil {
		state = ""
	}
	lim := c.limitsFor(model)
	// A point or box question looks in one of the request's images.
	qc := questionCheck{places: true, imageIDs: make([]string, len(req.Images))}
	for i, img := range req.Images {
		qc.imageIDs[i] = img.id
	}
	stateJSON, err := marshal(state)
	if err != nil {
		return nil, encodeError(err)
	}
	if !isSDL(stateJSON) && !isNull(stateJSON) {
		qc.sdl([]any{"body", "state"}, stateJSON, "state must be a string, an object or an array")
	}
	questions, err := encodeQuestions(req.Questions, lim, &qc)
	if err != nil {
		return nil, err
	}
	videos, videoDepth, err := encodeVideos(req.Videos)
	if err != nil {
		return nil, err
	}
	boosters, boosterDepth, err := encodeBoosters(req.Boosters)
	if err != nil {
		return nil, err
	}
	// The server's order: the nesting limit, the 422 entries, the first 400,
	// then the images, then the videos.
	depth := max(jsonDepth(stateJSON)+1, jsonDepth(questions)+1, videoDepth, boosterDepth)
	if len(req.Images) > 0 {
		depth = max(depth, 3)
	}
	if err := checkNesting(depth); err != nil {
		return nil, err
	}
	// The counts are 422 entries, after the state's and the questions'.
	qc.countEntries(req.Images, videos, lim)
	if err := qc.err(); err != nil {
		return nil, err
	}
	if err := checkImages(req.Images, lim); err != nil {
		return nil, err
	}
	videosJSON, err := checkVideos(videos, req.Images)
	if err != nil {
		return nil, err
	}
	var head bytes.Buffer
	head.WriteByte('{')
	if customID != nil {
		if err := writeMember(&head, "custom_id", *customID); err != nil {
			return nil, encodeError(err)
		}
		head.WriteByte(',')
	}
	if err := writeMember(&head, "model", model); err != nil {
		return nil, encodeError(err)
	}
	head.WriteString(`,"state":`)
	head.Write(stateJSON)
	head.WriteString(`,"questions":`)
	head.Write(questions)
	head.Write(boosters)
	// The body without its images, before any upload: images only add to it,
	// and the whole body is checked again once they are placed.
	if err := checkBodySize(head.Len() + len(videosJSON) + 1); err != nil {
		return nil, err
	}
	return &prepared{head: head.Bytes(), videos: videosJSON, images: req.Images}, nil
}

func encodeError(err error) error {
	return fmt.Errorf("decisionnode: the request cannot be encoded as JSON: %w", err)
}

// encodeQuestions writes the questions as one JSON object, in order, and
// checks each against the API's fixed rules into qc. Only an encoding problem
// is returned here.
func encodeQuestions(qs Questions, lim limits, qc *questionCheck) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, q := range qs {
		raw, err := marshal(q.Question)
		if err != nil {
			return nil, encodeError(fmt.Errorf("question %q: %w", q.ID, err))
		}
		qc.question(q.ID, raw, lim)
		key, err := marshal(q.ID)
		if err != nil {
			return nil, encodeError(err)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(raw)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// inlineWire is a local image sent as base64: {"id", "media_type", "data"},
// or without its id in a session frame.
func inlineWire(img Image, withID bool) []byte {
	data := img.src.data
	out := make([]byte, 0, base64.StdEncoding.EncodedLen(len(data))+len(img.id)+64)
	out = append(out, '{')
	if withID {
		out = appendIDMember(out, img.id)
		out = append(out, ',')
	}
	out = append(out, `"media_type":"`...)
	out = append(out, img.wireMediaType()...)
	out = append(out, `","data":"`...)
	out = base64.StdEncoding.AppendEncode(out, data)
	return append(out, `"}`...)
}

// uploadWire names an upload: {"id", "upload_id"} with the declared media
// type when one was given, or {"upload_id"} in a session frame.
func uploadWire(id, uploadID, mediaType string, withID bool) []byte {
	out := []byte{'{'}
	if withID {
		out = appendIDMember(out, id)
		out = append(out, ',')
	}
	out = append(out, `"upload_id":`...)
	b, _ := marshal(uploadID)
	out = append(out, b...)
	if mediaType != "" {
		out = append(out, `,"media_type":`...)
		b, _ = marshal(mediaType)
		out = append(out, b...)
	}
	return append(out, '}')
}

// urlWire is an image by URL: {"id", "url"}.
func urlWire(id, u string) []byte {
	out := appendIDMember([]byte{'{'}, id)
	out = append(out, `,"url":`...)
	b, _ := marshal(u)
	out = append(out, b...)
	return append(out, '}')
}

func appendIDMember(out []byte, id string) []byte {
	b, _ := marshal(id)
	out = append(out, `"id":`...)
	return append(out, b...)
}
