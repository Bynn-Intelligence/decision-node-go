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
	// head is the body without its images and its closing brace.
	head   []byte
	images []Image
}

// body returns the full body with the images' wire forms.
func (p *prepared) body(wires [][]byte) []byte {
	n := len(p.head) + 1
	if len(wires) > 0 {
		n += len(`,"images":[]`) + len(wires) - 1
		for _, w := range wires {
			n += len(w)
		}
	}
	out := make([]byte, 0, n)
	out = append(out, p.head...)
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
	questions, err := encodeQuestions(req.Questions, systemone)
	if err != nil {
		return nil, err
	}
	if err := checkImages(req.Images); err != nil {
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
	head.WriteByte(',')
	if err := writeMember(&head, "state", state); err != nil {
		return nil, encodeError(err)
	}
	head.WriteString(`,"questions":`)
	head.Write(questions)
	return &prepared{head: head.Bytes(), images: req.Images}, nil
}

func encodeError(err error) error {
	return fmt.Errorf("decisionnode: the request cannot be encoded as JSON: %w", err)
}

// encodeQuestions writes the questions as one JSON object, in order, and
// checks each against the API's fixed rules. For /v1/systemone a truth
// question is written with that path's wire type, noul.
func encodeQuestions(qs Questions, systemone bool) ([]byte, error) {
	var buf bytes.Buffer
	var qc questionCheck
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
		typ := qc.question(q.ID, raw)
		if systemone && typ == "truth" {
			raw = replaceMember(raw, "type", []byte(`"noul"`))
		}
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
	if err := qc.err(); err != nil {
		return nil, err
	}
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
