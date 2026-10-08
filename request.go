package decisionnode

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// Images are sent beside the state, up to 16. Every question sees every
	// image.
	Images []Image
	// Videos are sent beside the state and the images, up to 4, each as the
	// frames the caller sampled: a Video, or a video in its JSON wire form
	// (a map[string]any, a json.RawMessage). Every question sees every video.
	Videos []any
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

// Question is a question built by Truth, TruthWith, Choice, Score, Number,
// NumberWith, Point, PointWith, Box or BoxWith.
type Question struct {
	// Type is "truth", "choice", "score", "number", "point" or "box".
	Type string
	// Instructions says what to decide: a string or any JSON value. Nil leaves
	// it out.
	Instructions any
	// Criteria is the wire form of the options, levels or truth criteria.
	Criteria any
	// Min, Max and Step are a number question's grid: any JSON number (an
	// int, a float64, a json.Number). Nil leaves the field out, so the
	// server's default applies: 0, 255 and 1.
	Min, Max, Step any
	// Image is the id of the image a point or box question looks in,
	// required when the request has two or more images. Units is "pixels"
	// (the server's default) or "normalized". "" leaves a field out.
	Image, Units string

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
	for _, f := range []struct {
		name  string
		value any
	}{{"min", q.Min}, {"max", q.Max}, {"step", q.Step}} {
		if f.value != nil {
			buf.WriteByte(',')
			if err := writeMember(&buf, f.name, f.value); err != nil {
				return nil, err
			}
		}
	}
	for _, f := range []struct{ name, value string }{{"image", q.Image}, {"units", q.Units}} {
		if f.value != "" {
			buf.WriteByte(',')
			if err := writeMember(&buf, f.name, f.value); err != nil {
				return nil, err
			}
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

// Number builds a number question on the default grid, the whole numbers 0
// to 255: the probability of each value, the most likely one, and the
// expected value.
func Number(instructions any) Question {
	return Question{Type: "number", Instructions: instructions}
}

// NumberWith builds a number question on the grid min, min+step, ... up to
// max. Each of min, max and step is any JSON number (an int, a float64, a
// json.Number for an exact decimal), or nil to leave it out so the server's
// default applies (0, 255 and 1). A grid holds at most 4,096 values; min and
// step have at most 6 decimals. A grid the API would refuse is refused
// locally with a *ValidationError, before anything is sent.
func NumberWith(instructions, min, max, step any) Question {
	return Question{Type: "number", Instructions: instructions, Min: min, Max: max, Step: step}
}

// Point builds a point question: one spot in an image, its x and y, with the
// probability that what the instructions describe is there at all
// (present) and that the spot lies on it (confidence). The request needs an
// image; with two or more, name the one to look in with PointWith.
func Point(instructions any) Question {
	return Question{Type: "point", Instructions: instructions}
}

// PointWith builds a point question that looks in the image with this id
// (required when the request has two or more images) and answers in units,
// "pixels" (the default) or "normalized" (fractions of the width and height).
// "" leaves either out.
func PointWith(instructions any, image, units string) Question {
	return Question{Type: "point", Instructions: instructions, Image: image, Units: units}
}

// Box builds a box question: a rectangle in an image, its top-left (x1, y1)
// and bottom-right (x2, y2) corners, with present and confidence as a point
// has. The request needs an image; with two or more, name the one to look in
// with BoxWith.
func Box(instructions any) Question {
	return Question{Type: "box", Instructions: instructions}
}

// BoxWith builds a box question that looks in the image with this id and
// answers in units, like PointWith.
func BoxWith(instructions any, image, units string) Question {
	return Question{Type: "box", Instructions: instructions, Image: image, Units: units}
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

// Ptr returns a pointer to v, for the optional fields that tell a value of 0
// or "" from one not given: SessionRequest.Window, Frame.Seq,
// BatchRequest.CustomID, BatchListOptions.Limit.
func Ptr[T any](v T) *T { return &v }

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
