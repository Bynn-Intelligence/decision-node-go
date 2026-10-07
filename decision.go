package decisionnode

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Decision is the result of one decide call.
type Decision struct {
	// Answers holds one answer per question, by question id.
	Answers map[string]Answer
	// Model is the resolved model version that answered.
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
	// Raw is the parsed response body.
	Raw map[string]any
	// Header holds the response headers.
	Header http.Header
}

// Usage is the token count of a request. A count the server did not send is
// nil, never 0.
type Usage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// Answer is one answer of a Decision: a *TruthAnswer, a *ChoiceAnswer, a
// *ScoreAnswer, or an *UnknownAnswer for a question type this version of the
// package does not know.
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

// TruthAnswer answers a truth question.
type TruthAnswer struct {
	answerBase
	// Truth is the probability, 0 to 1, that the statement is true.
	Truth float64 `json:"truth"`
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
}

// ModelInfo is one entry of the model list.
type ModelInfo struct {
	Name        string
	Description string
	ReleaseDate string
	// Limits holds every other field of the entry: the model's published limits.
	Limits map[string]any
	// Raw is the entry as sent by the server.
	Raw map[string]any
}

func parseDecision(body []byte, header http.Header) (*Decision, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var shape struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   Usage                      `json:"usage"`
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
		Raw:       raw,
		Header:    header,
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
	switch typ {
	case "truth":
		a := &TruthAnswer{answerBase: base}
		if !hasFields(raw, "truth") || json.Unmarshal(data, a) != nil {
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
	default:
		return unknown
	}
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
		out = append(out, m)
	}
	return out, nil
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
