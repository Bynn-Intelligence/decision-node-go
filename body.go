package decisionnode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// prepared is a request checked against the API's fixed rules and encoded up
// to its images, which are placed last: inline, by URL or by upload_id, as
// the inline threshold and the upload cache decide at send time.
type prepared struct {
	// head is the body without its videos, its images and its closing brace.
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
func (c *Client) prepare(req Request, systemone bool, customID *string) (*prepared, error) {
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
	var qc questionCheck
	stateJSON, err := marshal(state)
	if err != nil {
		return nil, encodeError(err)
	}
	if !isSDL(stateJSON) && !isNull(stateJSON) {
		qc.sdl([]any{"body", "state"}, stateJSON, "state must be a string, an object or an array")
	}
	questions, err := encodeQuestions(req.Questions, systemone, lim, &qc)
	if err != nil {
		return nil, err
	}
	videos, videoDepth, err := encodeVideos(req.Videos)
	if err != nil {
		return nil, err
	}
	// The server's order: the nesting limit, the 422 entries, the first 400,
	// then the images, then the videos.
	depth := max(jsonDepth(stateJSON)+1, jsonDepth(questions)+1, videoDepth)
	if len(req.Images) > 0 {
		depth = max(depth, 3)
	}
	if err := checkNesting(depth); err != nil {
		return nil, err
	}
	if err := qc.err(); err != nil {
		return nil, err
	}
	if err := checkImages(req.Images, lim); err != nil {
		return nil, err
	}
	videosJSON, err := checkVideos(videos, req.Images, lim)
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
	return &prepared{head: head.Bytes(), videos: videosJSON, images: req.Images}, nil
}

func encodeError(err error) error {
	return fmt.Errorf("decisionnode: the request cannot be encoded as JSON: %w", err)
}

// encodeQuestions writes the questions as one JSON object, in order, and
// checks each against the API's fixed rules into qc. For /v1/systemone a
// truth question is written with that path's wire type, noul, and checked as
// that path checks it. Only an encoding problem is returned here.
func encodeQuestions(qs Questions, systemone bool, lim limits, qc *questionCheck) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, q := range qs {
		value := q.Question
		if b, ok := value.(Question); ok && systemone && b.Type == "truth" {
			b.Type = "noul"
			value = b
		}
		raw, err := marshal(value)
		if err != nil {
			return nil, encodeError(fmt.Errorf("question %q: %w", q.ID, err))
		}
		if systemone {
			if ms, ok := objectMembers(raw); ok && string(bytes.TrimSpace(ms.get("type"))) == `"truth"` {
				raw = replaceMember(raw, "type", []byte(`"noul"`))
			}
		}
		qc.question(q.ID, raw, systemone, lim)
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

// replaceMember returns a JSON object with the value of one top-level member
// replaced, every other byte kept as it was.
func replaceMember(obj []byte, name string, value []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return obj
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return obj
		}
		key, _ := t.(string)
		start := int(dec.InputOffset())
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return obj
		}
		end := int(dec.InputOffset())
		if key != name {
			continue
		}
		// Skip the colon and any space between the key and the value.
		for start < end && (obj[start] == ':' || obj[start] == ' ' || obj[start] == '\t' || obj[start] == '\n' || obj[start] == '\r') {
			start++
		}
		out := make([]byte, 0, len(obj)-(end-start)+len(value))
		out = append(out, obj[:start]...)
		out = append(out, value...)
		return append(out, obj[end:]...)
	}
	return obj
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
