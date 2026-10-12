package decisionnode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Decision is the result of one decide call.
type Decision struct {
	// Answers holds one answer per question, by question id.
	Answers map[string]Answer
	// Model is the resolved model version that answered; for a fine-tuned
	// model, its name pinned to the version that answered
	// (decisionnode-flash@acme/fraud:3).
	Model string
	// Usage is the token count of the request.
	Usage Usage
	// RequestID is the x-request-id response header.
	RequestID string
	// LatencyMs is the server's own time for the request in milliseconds,
	// from the x-decisionnode-latency-ms header. Nil when the header is absent.
	LatencyMs *float64
	// Safety is what the safety check found, from the x-decisionnode-safety
	// header. Nil when the server sent no such header (a self-hosted server).
	Safety *Safety
	// Warnings are the server's notes on a request with videos, exactly as
	// sent: one when it read a video at a lower rate or resolution than
	// asked. Empty, never nil, when there are none.
	Warnings []string
	// Fallback says that the API answered a request for one model with
	// another, from the x-decisionnode-fallback and
	// x-decisionnode-requested-model headers. Nil on an ordinary answer.
	Fallback *Fallback
	// Evidence holds what each booster the request named read, in the
	// request's order; Evidence.Get("age") reads one by name. Nil when the
	// request named none.
	Evidence EvidenceList
	// Raw is the parsed response body.
	Raw map[string]any
	// Header holds the response headers.
	Header http.Header
}

// Fallback is how a request was answered by another model than the one it
// asked for: decisionnode-flash for decisionnode-moe when no worker could
// take the request at once.
type Fallback struct {
	// ServedModel is the model that answered, as Decision.Model.
	ServedModel string
	// RequestedModel is the model the request asked for.
	RequestedModel string
}

// Usage is the token count of a request and, for a request with videos or
// boosters, what was read of them. A value the server did not send is nil,
// never 0.
type Usage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
	// VideoFrames is the number of frames read over all the videos. Nil for
	// a request without videos.
	VideoFrames *int `json:"video_frames"`
	// VideoSeconds is the seconds of the videos' windows read, summed, as
	// sent. Nil for a request without videos.
	VideoSeconds *float64 `json:"video_seconds"`
	// BoosterTokens is the share of InputTokens the boosters' readings take.
	// Nil for a request without boosters.
	BoosterTokens *int `json:"booster_tokens"`
	// Boosters is the number of booster calls billed: one per booster and
	// image read with status ok. Nil for a request without boosters.
	Boosters *int `json:"boosters"`
	// Cost is what the answer cost, in USD, as the API sent it. Nil when
	// the server sent none.
	Cost *Cost `json:"cost"`
}

// Cost is what an answer cost, in USD, exactly as the API sent it: read
// USD, never add up the parts, since the API rounds each amount on its own.
type Cost struct {
	// USD is the answer's whole cost.
	USD float64 `json:"usd"`
	// ModelUSD is the model's part: the input tokens at the model's price.
	ModelUSD float64 `json:"model_usd"`
	// BoostersUSD is the boosters' part: the booster calls billed.
	BoostersUSD float64 `json:"boosters_usd"`
	// FreeTier is true when the answer came out of the free tier.
	FreeTier bool `json:"free_tier"`
}

// Answer is one answer of a Decision: a *TruthAnswer, a *ChoiceAnswer, a
// *ScoreAnswer, a *NumberAnswer, a *PointAnswer, a *BoxAnswer, or an
// *UnknownAnswer for a question type this version of the package does not
// know.
type Answer interface {
	// Type is the answer's type as sent by the server: "truth", "choice", "score", ...
	Type() string
	// Raw is the answer object as sent by the server.
	Raw() map[string]any
}

type answerBase struct {
	typ string
	raw map[string]any
}

func (a answerBase) Type() string        { return a.typ }
func (a answerBase) Raw() map[string]any { return a.raw }

// TruthAnswer answers a truth question. Type returns the answer's type as
// the server sent it.
type TruthAnswer struct {
	answerBase
	// Truth is the probability, 0 to 1, that the statement is true.
	Truth float64 `json:"truth"`
}

// NumberAnswer answers a number question.
type NumberAnswer struct {
	answerBase
	// Number is the most likely value of the grid, exactly as sent.
	Number float64 `json:"number"`
	// Expected is the probability-weighted mean over the whole grid.
	Expected float64 `json:"expected"`
	// Confidence is the probability of Number.
	Confidence float64 `json:"confidence"`
	// Probabilities holds the listed values' probabilities, keyed by the
	// value exactly as the server wrote it ("7", "3.5"). Values at 0.001 or
	// below are left out, so the list may sum to less than 1.
	Probabilities map[string]float64 `json:"probabilities"`
}

// ChoiceAnswer answers a choice question.
type ChoiceAnswer struct {
	answerBase
	// Choice is the option picked, byte for byte as sent.
	Choice string `json:"choice"`
	// Confidence is 0 for no better than a uniform guess, 1 for certain.
	Confidence float64 `json:"confidence"`
	// Probabilities holds one probability per option, by option name.
	Probabilities map[string]float64 `json:"probabilities"`
}

// ScoreAnswer answers a score question.
type ScoreAnswer struct {
	answerBase
	// Score is the expected level index, from 0 to the number of levels minus 1.
	Score float64 `json:"score"`
	// Confidence is 1 when all probability sits on one level.
	Confidence float64 `json:"confidence"`
	// Probabilities holds one probability per level, by level index.
	Probabilities map[int]float64 `json:"probabilities"`
	// Legend maps each level index to the level as it was sent.
	Legend map[int]any `json:"legend"`
}

// PointAnswer answers a point question: one spot in an image. Every number is
// exactly as sent: whole pixels, or fractions of the width and height with
// units "normalized".
type PointAnswer struct {
	answerBase
	// X and Y are the spot, from the top-left corner of the image, X to the
	// right and Y down.
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Present is the probability that what the instructions describe is in
	// the image at all. Under a low Present the spot is only the best guess.
	Present float64 `json:"present"`
	// Confidence is the probability that the spot lies on the thing, if it
	// is there.
	Confidence float64 `json:"confidence"`
	// Image is the id of the image looked in; Width and Height its size in
	// pixels.
	Image  string  `json:"image"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// BoxAnswer answers a box question: a rectangle in an image. Every number is
// exactly as sent, in the units of a PointAnswer.
type BoxAnswer struct {
	answerBase
	// X1 and Y1 are the top-left corner, X2 and Y2 the bottom-right one.
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
	// Present is the probability that what the instructions describe is in
	// the image at all.
	Present float64 `json:"present"`
	// Confidence is the probability that the box overlaps the thing's true
	// box by an intersection over union of 0.5 or more, if it is there.
	Confidence float64 `json:"confidence"`
	// Image is the id of the image looked in; Width and Height its size in
	// pixels.
	Image  string  `json:"image"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PointsAnswer answers a point question asked for several places (Points,
// PointsWith): up to the question's max spots, in the response's order,
// left to right by the centre, ties from the top. Every number is exactly as
// sent, in the units of a PointAnswer.
type PointsAnswer struct {
	answerBase
	// Count is how many points follow, 0 to the question's max.
	Count int `json:"count"`
	// Present is the probability that at least one such thing is in the
	// image.
	Present float64 `json:"present"`
	// Complete is the probability that nothing was left out: low when the
	// image holds more than max of them.
	Complete float64 `json:"complete"`
	// Points are the spots, each with its own confidence; empty when Count
	// is 0.
	Points []PointPlace `json:"points"`
	// Image is the id of the image looked in; Width and Height its size in
	// pixels.
	Image  string  `json:"image"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// PointPlace is one spot of a PointsAnswer.
type PointPlace struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Confidence is the probability that this spot lies on one of the
	// things the instructions describe.
	Confidence float64 `json:"confidence"`
}

// BoxesAnswer answers a box question asked for several places (Boxes,
// BoxesWith): up to the question's max rectangles, in the order and with
// the fields of a PointsAnswer.
type BoxesAnswer struct {
	answerBase
	// Count is how many boxes follow, 0 to the question's max.
	Count int `json:"count"`
	// Present is the probability that at least one such thing is in the
	// image.
	Present float64 `json:"present"`
	// Complete is the probability that nothing was left out.
	Complete float64 `json:"complete"`
	// Boxes are the rectangles, each with its own confidence; empty when
	// Count is 0.
	Boxes []BoxPlace `json:"boxes"`
	// Image is the id of the image looked in; Width and Height its size in
	// pixels.
	Image  string  `json:"image"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// BoxPlace is one rectangle of a BoxesAnswer: its top-left (X1, Y1) and
// bottom-right (X2, Y2) corners.
type BoxPlace struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
	// Confidence is the probability that this box is right, as a BoxAnswer
	// defines it.
	Confidence float64 `json:"confidence"`
}

// UnknownAnswer holds an answer of a type this version of the package does
// not know. Read its fields with Raw.
type UnknownAnswer struct {
	answerBase
}

// Answer returns the answer to the question with the given id, or nil.
func (d *Decision) Answer(id string) Answer { return d.Answers[id] }

// Truth returns the answer to the given question when it is a truth answer, else nil.
func (d *Decision) Truth(id string) *TruthAnswer {
	a, _ := d.Answers[id].(*TruthAnswer)
	return a
}

// Choice returns the answer to the given question when it is a choice answer, else nil.
func (d *Decision) Choice(id string) *ChoiceAnswer {
	a, _ := d.Answers[id].(*ChoiceAnswer)
	return a
}

// Score returns the answer to the given question when it is a score answer, else nil.
func (d *Decision) Score(id string) *ScoreAnswer {
	a, _ := d.Answers[id].(*ScoreAnswer)
	return a
}

// Number returns the answer to the given question when it is a number answer, else nil.
func (d *Decision) Number(id string) *NumberAnswer {
	a, _ := d.Answers[id].(*NumberAnswer)
	return a
}

// Point returns the answer to the given question when it is a point answer, else nil.
func (d *Decision) Point(id string) *PointAnswer {
	a, _ := d.Answers[id].(*PointAnswer)
	return a
}

// Box returns the answer to the given question when it is a box answer, else nil.
func (d *Decision) Box(id string) *BoxAnswer {
	a, _ := d.Answers[id].(*BoxAnswer)
	return a
}

// Points returns the answer to the given question when it is a several-places
// point answer, else nil.
func (d *Decision) Points(id string) *PointsAnswer {
	a, _ := d.Answers[id].(*PointsAnswer)
	return a
}

// Boxes returns the answer to the given question when it is a several-places
// box answer, else nil.
func (d *Decision) Boxes(id string) *BoxesAnswer {
	a, _ := d.Answers[id].(*BoxesAnswer)
	return a
}

// Safety is what the hosted API's safety check found for a request.
type Safety struct {
	// Checked is false when the check did not run; Reason then says why.
	Checked bool
	// Harm is the probability of the harm category.
	Harm float64
	// SelfHarm is the probability of the self_harm category.
	SelfHarm float64
	// Scores holds every category the header named, by name.
	Scores map[string]float64
	// Flagged lists, in header order, the categories above the key's
	// thresholds (flagged or blocked).
	Flagged []string
	// Reason is the code given when the check did not run.
	Reason string
	// Blocked names the category a session frame was refused for, from the
	// body form of the check. Empty otherwise.
	Blocked string
}

// ModelInfo is one entry of the model list.
type ModelInfo struct {
	Name        string
	Description string
	ReleaseDate string
	// Limits holds every other field of the entry: the model's published
	// limits, the objects sessions and batch included. The typed fields below
	// read the best-known of them; a field the server did not send is nil.
	Limits map[string]any
	// Raw is the entry as sent by the server.
	Raw map[string]any

	// Version is the pinned model version the name resolves to, and Alias
	// the alias that moves with releases, when the entry states them. Both
	// are also in Limits.
	Version string
	Alias   string

	// FineTuned is true for one of the workspace's fine-tuned models, whose
	// Name is <base>@<workspace>/<name>, pinned (":3") or not. Nil for a
	// catalogue model.
	FineTuned *bool
	// Base is a fine-tuned model's base model, by its pinned id.
	Base string
	// AdapterVersion is a fine-tuned model's version number; for a name
	// without a version, the version it resolves to. Nil for a catalogue
	// model.
	AdapterVersion *int
	// Status is a fine-tuned model's status: "deployed" (it answers
	// decisions), "ready" (trained and through its quality gate, not
	// deployed), or any other value exactly as sent.
	Status string

	// NumberOneQuestionValues is the most grid values answered as one question.
	NumberOneQuestionValues *int
	// NumberMaxGridValues is the most values a number grid may hold.
	NumberMaxGridValues *int
	// NumberDefaultRange is min and max when a number question leaves them out.
	NumberDefaultRange []float64
	// NumberDefaultStep is step when a number question leaves it out.
	NumberDefaultStep *float64
	// UploadMaxBytes is the largest upload.
	UploadMaxBytes *int64
	// UploadTTLSeconds is how long an upload may be named after its presign.
	UploadTTLSeconds *int
	// UploadMediaTypes are the media types an upload may have.
	UploadMediaTypes []string
	// Sessions holds the model's session limits.
	Sessions *ModelSessions
	// Batch holds the model's Batch API limits.
	Batch *ModelBatch
}

// ModelSessions is the sessions object of a model entry.
type ModelSessions struct {
	Enabled            bool `json:"enabled"`
	MaxPerKey          int  `json:"max_per_key"`
	DefaultWindow      int  `json:"default_window"`
	MaxWindow          int  `json:"max_window"`
	DefaultTTLSeconds  int  `json:"default_ttl_seconds"`
	MaxTTLSeconds      int  `json:"max_ttl_seconds"`
	IdleTimeoutSeconds int  `json:"idle_timeout_seconds"`
	MaxFrameTokens     int  `json:"max_frame_tokens"`
	MaxFrameImages     int  `json:"max_frame_images"`
	WindowTokens       int  `json:"window_tokens"`
	MaxPrefixTokens    int  `json:"max_prefix_tokens"`
}

// ModelBatch is the batch object of a model entry.
type ModelBatch struct {
	RequestsPerBatch           int     `json:"requests_per_batch"`
	BatchBytes                 int64   `json:"batch_bytes"`
	AddBytes                   int64   `json:"add_bytes"`
	ActiveBatchesPerKey        int     `json:"active_batches_per_key"`
	QueuedRequestsPerKey       int     `json:"queued_requests_per_key"`
	TimeToCompletionHours      int     `json:"time_to_completion_hours"`
	ResultRetentionDays        int     `json:"result_retention_days"`
	MaxCustomIDChars           int     `json:"max_custom_id_chars"`
	PricePerMillionInputTokens float64 `json:"price_per_million_input_tokens"`
	PriceFactor                float64 `json:"price_factor"`
}

func parseDecision(body []byte, header http.Header) (*Decision, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		Model    string                     `json:"model"`
		Answers  map[string]json.RawMessage `json:"answers"`
		Usage    Usage                      `json:"usage"`
		Evidence json.RawMessage            `json:"evidence"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, err
	}
	d := &Decision{
		Answers:   make(map[string]Answer, len(shape.Answers)),
		Model:     shape.Model,
		Usage:     shape.Usage,
		RequestID: header.Get("x-request-id"),
		Safety:    parseSafety(header.Values("x-decisionnode-safety")),
		Warnings:  []string{},
		Evidence:  parseEvidence(shape.Evidence),
		Raw:       raw,
		Header:    header,
	}
	if list, ok := raw["warnings"].([]any); ok {
		for _, w := range list {
			if s, ok := w.(string); ok {
				d.Warnings = append(d.Warnings, s)
			}
		}
	}
	if served := strings.TrimSpace(header.Get("x-decisionnode-fallback")); served != "" {
		d.Fallback = &Fallback{ServedModel: served, RequestedModel: strings.TrimSpace(header.Get("x-decisionnode-requested-model"))}
	}
	if v := strings.TrimSpace(header.Get("x-decisionnode-latency-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil {
			d.LatencyMs = &ms
		}
	}
	for id, data := range shape.Answers {
		d.Answers[id] = parseAnswer(data)
	}
	return d, nil
}

// parseAnswer reads one answer. An answer of a known type whose fields are
// missing or of the wrong type is returned as an *UnknownAnswer with its raw
// fields, as an answer of an unknown type is: the decision does not fail.
func parseAnswer(data json.RawMessage) Answer {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return &UnknownAnswer{}
	}
	typ, _ := raw["type"].(string)
	base := answerBase{typ: typ, raw: raw}
	unknown := &UnknownAnswer{answerBase: base}
	// A point or box answer with a list of places is the several-places
	// shape, told by the answer itself, so a batch line is typed the same way.
	if typ == "point" || typ == "box" {
		_, points := raw["points"]
		_, boxes := raw["boxes"]
		if points || boxes {
			return parsePlaces(data, base)
		}
	}
	switch typ {
	case "truth":
		a := &TruthAnswer{answerBase: base}
		if !hasFields(raw, "truth") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	case "noul":
		v, ok := raw["noul"].(float64)
		if !ok {
			return unknown
		}
		return &TruthAnswer{answerBase: base, Truth: v}
	case "number":
		a := &NumberAnswer{answerBase: base}
		if !hasFields(raw, "number", "expected", "confidence", "probabilities") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	case "choice":
		a := &ChoiceAnswer{answerBase: base}
		if !hasFields(raw, "choice", "confidence", "probabilities") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	case "score":
		a := &ScoreAnswer{answerBase: base}
		if !hasFields(raw, "score", "confidence", "probabilities", "legend") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	case "point":
		a := &PointAnswer{answerBase: base}
		if !hasFields(raw, "x", "y", "present", "confidence", "image", "width", "height") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	case "box":
		a := &BoxAnswer{answerBase: base}
		if !hasFields(raw, "x1", "y1", "x2", "y2", "present", "confidence", "image", "width", "height") || json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	default:
		return unknown
	}
}

// parsePlaces reads a several-places answer: a *PointsAnswer for a point,
// a *BoxesAnswer for a box. Its count must be a whole number of 0 or more,
// present, complete, width and height numbers, image a string, and its own
// list (points for a point, boxes for a box) objects whose every field is a
// number; anything else is an *UnknownAnswer.
func parsePlaces(data json.RawMessage, base answerBase) Answer {
	unknown := &UnknownAnswer{answerBase: base}
	list, fields := "points", []string{"x", "y", "confidence"}
	if base.typ == "box" {
		list, fields = "boxes", []string{"x1", "y1", "x2", "y2", "confidence"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if dec.Decode(&raw) != nil {
		return unknown
	}
	number := func(v any) bool { _, ok := v.(json.Number); return ok }
	count, isNum := raw["count"].(json.Number)
	n, isInt := jsonInt(json.RawMessage(count))
	if !isNum || !isInt || n.Sign() < 0 {
		return unknown
	}
	for _, k := range []string{"present", "complete", "width", "height"} {
		if !number(raw[k]) {
			return unknown
		}
	}
	if _, ok := raw["image"].(string); !ok {
		return unknown
	}
	places, ok := raw[list].([]any)
	if !ok {
		return unknown
	}
	for _, p := range places {
		place, ok := p.(map[string]any)
		if !ok {
			return unknown
		}
		for _, f := range fields {
			if !number(place[f]) {
				return unknown
			}
		}
	}
	if base.typ == "box" {
		a := &BoxesAnswer{answerBase: base}
		if json.Unmarshal(data, a) != nil {
			return unknown
		}
		return a
	}
	a := &PointsAnswer{answerBase: base}
	if json.Unmarshal(data, a) != nil {
		return unknown
	}
	return a
}

// hasFields reports whether every key is present and not null.
func hasFields(raw map[string]any, keys ...string) bool {
	for _, k := range keys {
		if raw[k] == nil {
			return false
		}
	}
	return true
}

func parseModels(body []byte) ([]ModelInfo, error) {
	var shape struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(shape.Models))
	for _, entry := range shape.Models {
		m := ModelInfo{Raw: entry, Limits: map[string]any{}}
		for k, v := range entry {
			switch k {
			case "name":
				m.Name, _ = v.(string)
			case "description":
				m.Description, _ = v.(string)
			case "release_date":
				m.ReleaseDate, _ = v.(string)
			default:
				m.Limits[k] = v
			}
		}
		m.Version, _ = entry["version"].(string)
		m.Alias, _ = entry["alias"].(string)
		if b, ok := entry["fine_tuned"].(bool); ok {
			m.FineTuned = &b
		}
		m.Base, _ = entry["base"].(string)
		m.AdapterVersion = intField(entry, "adapter_version")
		m.Status, _ = entry["status"].(string)
		m.NumberOneQuestionValues = intField(entry, "number_one_question_values")
		m.NumberMaxGridValues = intField(entry, "number_max_grid_values")
		if r, ok := entry["number_default_range"].([]any); ok {
			for _, x := range r {
				if f, ok := x.(float64); ok {
					m.NumberDefaultRange = append(m.NumberDefaultRange, f)
				}
			}
		}
		if f, ok := entry["number_default_step"].(float64); ok {
			m.NumberDefaultStep = &f
		}
		if f, ok := entry["upload_max_bytes"].(float64); ok {
			n := int64(f)
			m.UploadMaxBytes = &n
		}
		m.UploadTTLSeconds = intField(entry, "upload_ttl_seconds")
		if ts, ok := entry["upload_media_types"].([]any); ok {
			for _, x := range ts {
				if t, ok := x.(string); ok {
					m.UploadMediaTypes = append(m.UploadMediaTypes, t)
				}
			}
		}
		if obj, ok := entry["sessions"].(map[string]any); ok {
			m.Sessions = &ModelSessions{}
			remarshal(obj, m.Sessions)
		}
		if obj, ok := entry["batch"].(map[string]any); ok {
			m.Batch = &ModelBatch{}
			remarshal(obj, m.Batch)
		}
		out = append(out, m)
	}
	return out, nil
}

func intField(m map[string]any, key string) *int {
	f, ok := m[key].(float64)
	if !ok {
		return nil
	}
	n := int(f)
	return &n
}

// remarshal fills a typed struct from a parsed object, ignoring fields of
// the wrong type.
func remarshal(from any, to any) {
	b, err := json.Marshal(from)
	if err == nil {
		_ = json.Unmarshal(b, to)
	}
}

// parseSafetyBody reads the body form of the safety check (session open
// responses, replies and refused frames): checked, harm, self_harm, flagged,
// reason and, on a refused frame, blocked.
func parseSafetyBody(v any) *Safety {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	s := &Safety{Scores: map[string]float64{}, Flagged: []string{}}
	s.Checked, _ = m["checked"].(bool)
	s.Reason, _ = m["reason"].(string)
	s.Blocked, _ = m["blocked"].(string)
	for k, x := range m {
		if f, ok := x.(float64); ok {
			s.Scores[k] = f
		}
	}
	s.Harm = s.Scores["harm"]
	s.SelfHarm = s.Scores["self_harm"]
	if fl, ok := m["flagged"].([]any); ok {
		for _, x := range fl {
			if name, ok := x.(string); ok {
				s.Flagged = append(s.Flagged, name)
			}
		}
	}
	return s
}

// parseSafety reads the x-decisionnode-safety header: a list of name=value
// members with optional ;parameters, or unchecked;reason="<code>".
func parseSafety(values []string) *Safety {
	if len(values) == 0 {
		return nil
	}
	value := strings.TrimSpace(strings.Join(values, ", "))
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "unchecked") {
		s := &Safety{Checked: false}
		for _, param := range strings.Split(value, ";")[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.TrimSpace(k) == "reason" {
				s.Reason = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		return s
	}
	s := &Safety{Checked: true, Scores: map[string]float64{}, Flagged: []string{}}
	for _, member := range strings.Split(value, ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		parts := strings.Split(member, ";")
		name, num, _ := strings.Cut(parts[0], "=")
		name = strings.TrimSpace(name)
		if f, err := strconv.ParseFloat(strings.TrimSpace(num), 64); err == nil {
			s.Scores[name] = f
			switch name {
			case "harm":
				s.Harm = f
			case "self_harm":
				s.SelfHarm = f
			}
		}
		for _, p := range parts[1:] {
			if p = strings.TrimSpace(p); p == "flagged" || p == "blocked" {
				s.Flagged = append(s.Flagged, name)
				break
			}
		}
	}
	return s
}
