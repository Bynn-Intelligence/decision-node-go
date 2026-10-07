package decisionnode

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
)

// Request is one decide call.
type Request struct {
	// State is what is judged: a string, or any value that encodes to a JSON
	// object or array (a struct, a map, a slice, json.RawMessage). A string is
	// sent as it is. Nil sends the empty state "".
	State any
	// Questions holds the questions, by id, in the order they are sent.
	Questions Questions
	// Images are sent beside the state. Every question sees every image.
	Images []Image
	// Model overrides the client's model for this call.
	Model string
}

// Questions is an ordered list of questions, each with its id. Build it with Q:
//
//	decisionnode.Questions{
//		decisionnode.Q("route", decisionnode.Choice("Where should this go?", []string{"billing", "bug"})),
//		decisionnode.Q("escalate", decisionnode.Truth("Escalate to a human now?")),
//	}
type Questions []NamedQuestion

// NamedQuestion is one question with its id.
type NamedQuestion struct {
	// ID is any string. The answer comes back under the same id.
	ID string
	// Question is a Question from Truth, TruthWith, Choice or Score, or any
	// value in the wire form, such as a map[string]any or a json.RawMessage.
	Question any
}

// Q pairs a question with its id.
func Q(id string, question any) NamedQuestion {
	return NamedQuestion{ID: id, Question: question}
}

// QuestionsFrom turns a map of id to question into Questions, sorted by id.
func QuestionsFrom[V any](m map[string]V) Questions {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make(Questions, 0, len(m))
	for _, id := range ids {
		out = append(out, Q(id, m[id]))
	}
	return out
}

// MarshalJSON writes the questions as one JSON object, in order.
func (qs Questions) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, q := range qs {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeMember(&buf, q.ID, q.Question); err != nil {
			return nil, fmt.Errorf("question %q: %w", q.ID, err)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Question is a question built by Truth, TruthWith, Choice or Score.
type Question struct {
	// Type is "truth", "choice" or "score".
	Type string
	// Instructions says what to decide: a string or any JSON value. Nil leaves
	// it out.
	Instructions any
	// Criteria is the wire form of the options, levels or truth criteria.
	Criteria any

	hasCriteria bool
}

// MarshalJSON writes the question in its wire form.
func (q Question) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	if err := writeMember(&buf, "type", q.Type); err != nil {
		return nil, err
	}
	if q.Instructions != nil {
		buf.WriteByte(',')
		if err := writeMember(&buf, "instructions", q.Instructions); err != nil {
			return nil, err
		}
	}
	if q.hasCriteria || q.Criteria != nil {
		buf.WriteByte(',')
		if err := writeMember(&buf, "criteria", q.Criteria); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Truth builds a truth question: the probability, 0 to 1, that the statement
// or the answer to the question is true.
func Truth(instructions any) Question {
	return Question{Type: "truth", Instructions: instructions}
}

// TruthWith builds a truth question with criteria: what a yes (ifTrue) and a
// no (ifFalse) mean. Either side may be nil; when both are nil no criteria
// are sent.
func TruthWith(instructions, ifTrue, ifFalse any) Question {
	q := Truth(instructions)
	var crit Options
	if ifTrue != nil {
		crit = append(crit, ChoiceOption{Name: "true", Description: ifTrue})
	}
	if ifFalse != nil {
		crit = append(crit, ChoiceOption{Name: "false", Description: ifFalse})
	}
	if len(crit) > 0 {
		q.Criteria = crit
	}
	return q
}

// Choice builds a choice question: pick exactly one option. options is
// Options (ordered names with descriptions), a map of name to description
// (sent sorted by name), or a slice of names (each description null).
func Choice(instructions any, options any) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: choiceCriteria(options), hasCriteria: true}
}

// Score builds a score question: place the state on an ordered scale. levels
// is a slice of levels, low to high; each level is a string or any JSON value.
func Score(instructions any, levels any) Question {
	crit := levels
	if crit == nil {
		crit = []any{}
	}
	return Question{Type: "score", Instructions: instructions, Criteria: crit, hasCriteria: true}
}

// Options is an ordered set of choice options, sent in this order.
type Options []ChoiceOption

// ChoiceOption is one choice option. Description may be nil.
type ChoiceOption struct {
	Name        string
	Description any
}

// MarshalJSON writes the options as one JSON object, in order.
func (o Options) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, opt := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeMember(&buf, opt.Name, opt.Description); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func choiceCriteria(options any) any {
	switch o := options.(type) {
	case nil:
		return Options{}
	case Options, json.RawMessage:
		return o
	case []string:
		out := make(Options, len(o))
		for i, name := range o {
			out[i] = ChoiceOption{Name: name}
		}
		return out
	}
	v := reflect.ValueOf(options)
	if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return options
		}
		out := make(Options, v.Len())
		for i := range out {
			name := v.Index(i).Interface()
			if s, ok := name.(string); ok {
				out[i] = ChoiceOption{Name: s}
			} else {
				out[i] = ChoiceOption{Name: fmt.Sprint(name)}
			}
		}
		return out
	}
	return options
}

// Image is an image sent beside the state. Build it with ImageFile,
// ImageData or ImageURL.
type Image struct {
	id        string
	data      []byte
	mediaType string
	url       string
}

// ImageFile reads an image file now, when the Image is made, so an unreadable
// file fails here rather than at the call. The media type is taken from the
// file's first bytes: JPEG, PNG or WebP; any other format is an error.
func ImageFile(id, path string) (Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Image{}, fmt.Errorf("decisionnode: image %q: %w", id, err)
	}
	mediaType := sniffMediaType(data)
	if mediaType == "" {
		return Image{}, fmt.Errorf("decisionnode: image %q is not a JPEG, PNG or WebP image", id)
	}
	return Image{id: id, data: data, mediaType: mediaType}, nil
}

// ImageData sends image bytes. An empty mediaType is taken from the bytes'
// first bytes (JPEG, PNG or WebP); bytes of any other format make the call
// fail with an argument error before anything is sent.
func ImageData(id string, data []byte, mediaType string) Image {
	return Image{id: id, data: data, mediaType: mediaType}
}

// ImageURL sends an https URL for the server to fetch.
func ImageURL(id, url string) Image { return Image{id: id, url: url} }

// ID returns the image's id.
func (img Image) ID() string { return img.id }

func (img Image) wire() (map[string]any, error) {
	if img.url != "" {
		return map[string]any{"id": img.id, "url": img.url}, nil
	}
	mediaType := img.mediaType
	if mediaType == "" {
		mediaType = sniffMediaType(img.data)
		if mediaType == "" {
			return nil, fmt.Errorf("decisionnode: image %q is not a JPEG, PNG or WebP image", img.id)
		}
	}
	return map[string]any{"id": img.id, "media_type": mediaType, "data": base64.StdEncoding.EncodeToString(img.data)}, nil
}

func sniffMediaType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// wireImage keeps the key order id, media_type, data on the wire.
type wireImage map[string]any

func (w wireImage) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	for _, k := range []string{"id", "media_type", "data", "url"} {
		v, ok := w[k]
		if !ok {
			continue
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		if err := writeMember(&buf, k, v); err != nil {
			return nil, err
		}
		n++
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// requestBody is the wire form of a decide request, in this key order.
type requestBody struct {
	Model     string      `json:"model"`
	State     any         `json:"state"`
	Questions Questions   `json:"questions"`
	Images    []wireImage `json:"images,omitempty"`
}

func buildBody(req Request, model string) (*requestBody, error) {
	if len(req.Questions) == 0 {
		return nil, errors.New("decisionnode: questions must hold at least one question")
	}
	if req.Model != "" {
		model = req.Model
	}
	state := req.State
	if state == nil {
		state = ""
	}
	body := &requestBody{Model: model, State: state, Questions: req.Questions}
	for _, img := range req.Images {
		w, err := img.wire()
		if err != nil {
			return nil, err
		}
		body.Images = append(body.Images, wireImage(w))
	}
	return body, nil
}

// marshal encodes v as compact JSON without escaping <, > and &.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func writeMember(buf *bytes.Buffer, key string, value any) error {
	k, err := marshal(key)
	if err != nil {
		return err
	}
	v, err := marshal(value)
	if err != nil {
		return err
	}
	buf.Write(k)
	buf.WriteByte(':')
	buf.Write(v)
	return nil
}
