package decisionnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The boosters the API offers today. A booster is named by a plain string,
// so a booster added on the server works with this version too.
const (
	// BoosterAge reads how old the person in a picture is.
	BoosterAge = "age"
	// BoosterAIGenerated reads how likely a picture was made by an image
	// generator, and who signed its Content Credentials.
	BoosterAIGenerated = "ai_generated"
	// BoosterFaceDetection finds the faces in a picture.
	BoosterFaceDetection = "face_detection"
	// BoosterWantedPerson reads whether a face matches an entry of a
	// wanted-persons list. It is off unless enabled for the workspace.
	BoosterWantedPerson = "wanted_person"
	// BoosterOCR reads the text in a picture, or answers a prompt about it.
	BoosterOCR = "ocr"
)

// OCRBooster is the ocr booster with a prompt: what to read or answer from
// the picture's text, at most 500 characters (the API's limit, not checked
// here). Sent as {"name": "ocr", "options": {"prompt": ...}}.
func OCRBooster(prompt string) Booster {
	return Booster{Name: BoosterOCR, Options: map[string]any{"prompt": prompt}}
}

// WantedPersonBooster is the wanted_person booster with a match threshold.
// Sent as {"name": "wanted_person", "options": {"threshold": ...}}.
func WantedPersonBooster(threshold float64) Booster {
	return Booster{Name: BoosterWantedPerson, Options: map[string]any{"threshold": threshold}}
}

// Booster asks a booster to read the request's images before the decision,
// with settings. Its reading is added to what the model sees as evidence and
// comes back in Decision.Evidence. A booster with no settings is named by its
// name in Request.Boosters ("age"); a Booster with no field but Name is sent
// the same way.
type Booster struct {
	// Name is the booster's name, such as BoosterAge.
	Name string
	// Images are the ids of the request's images the booster reads. Nil
	// leaves the field out: the booster reads every image of the request.
	Images []string
	// Require, set with Ptr(true), makes the request fail with a 529
	// *OverloadedError (ErrorType booster_unavailable) when the booster
	// fails or does not answer in time. Without it, or with Ptr(false), the
	// decision runs without that reading, and its Evidence says why. Nil
	// leaves the field out.
	Require *bool
	// Options holds the booster's settings. Nil leaves the field out.
	Options map[string]any
}

// MarshalJSON writes the booster in its wire form: {"name", "images"?,
// "require"?, "options"?}, or its bare name when no option is set.
func (b Booster) MarshalJSON() ([]byte, error) {
	if b.Images == nil && b.Require == nil && b.Options == nil {
		return marshal(b.Name)
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	if err := writeMember(&buf, "name", b.Name); err != nil {
		return nil, err
	}
	if b.Images != nil {
		buf.WriteByte(',')
		if err := writeMember(&buf, "images", b.Images); err != nil {
			return nil, err
		}
	}
	if b.Require != nil {
		buf.WriteString(`,"require":` + strconv.FormatBool(*b.Require))
	}
	if b.Options != nil {
		buf.WriteByte(',')
		if err := writeMember(&buf, "options", b.Options); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// encodeBoosters writes a request's boosters as the body's
// `,"boosters":[...]` member, in the caller's order, and returns how deep the
// member nests inside the body. No boosters writes nothing, so the body is
// the body of a request without them. Nothing about the boosters is checked
// here: which boosters exist and which a workspace may use is the server's
// to judge.
func encodeBoosters(boosters []any) ([]byte, int, error) {
	if len(boosters) == 0 {
		return nil, 0, nil
	}
	var buf bytes.Buffer
	buf.WriteString(`,"boosters":[`)
	for i, b := range boosters {
		if p, ok := b.(*Booster); ok {
			if p == nil {
				return nil, 0, fmt.Errorf("decisionnode: booster %d is a nil *Booster", i)
			}
			b = *p
		}
		raw, err := marshal(b)
		if err != nil {
			return nil, 0, encodeError(fmt.Errorf("booster %d: %w", i, err))
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(raw)
	}
	buf.WriteByte(']')
	// The body's own object holds the boosters array.
	return buf.Bytes(), jsonDepth(buf.Bytes()) + 1, nil
}

// EvidenceList is a decision's evidence: what each booster the request named
// read, one entry per booster, in the request's order.
type EvidenceList []*Evidence

// Get returns the evidence of the booster with the given name, or nil.
func (l EvidenceList) Get(name string) *Evidence {
	for _, e := range l {
		if e.Booster == name {
			return e
		}
	}
	return nil
}

// Evidence is what one booster read for a request.
type Evidence struct {
	// Booster is the booster's name.
	Booster string
	// Version is the version of the booster that read, such as "age-v3".
	Version string
	// Status is "ok", "timeout" (no answer within the booster's time),
	// "error" or "skipped" (none of the request's images is one it reads),
	// or any other value exactly as sent.
	Status string
	// LatencyMs is how long the booster took, in milliseconds. Nil when the
	// server sent none.
	LatencyMs *float64
	// BilledCalls is the calls of this booster billed on this request: one
	// per image read with status ok, 0 for a booster that timed out or
	// failed. Nil when the server sent none.
	BilledCalls *int
	// Results holds one reading per picture the booster was to read: an
	// *AgeReading, an *AIGeneratedReading, or an *UnknownReading. A picture
	// it did not read (it timed out or failed: {"image"}) or could not
	// decode ({"image", "readable": false}) is an *UnknownReading, and
	// Status says why. Empty, never nil, when there are none.
	Results []Reading
	// Raw is the evidence object as sent.
	Raw map[string]any
}

// Reading is one booster's reading of one image: an *AgeReading, an
// *AIGeneratedReading, or an *UnknownReading for a booster this version of
// the package does not know or a reading with a field missing or of the
// wrong type.
type Reading interface {
	// Raw is the reading as sent by the server.
	Raw() map[string]any
}

type readingBase struct{ raw map[string]any }

func (r readingBase) Raw() map[string]any { return r.raw }

// AgeReading is the age booster's reading of one picture. The largest face
// is read. With Faces 0 the picture shows no face and the age fields are nil.
type AgeReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// Faces is how many faces were found.
	Faces int
	// AgeYears is the age read, and Low and High its range, in years.
	AgeYears, Low, High *int
	// Confidence is 0 to 1.
	Confidence *float64
}

// AIGeneratedReading is the ai_generated booster's reading of one picture.
type AIGeneratedReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// Generated is the probability, 0 to 1, that the picture was made by an
	// image generator.
	Generated float64
	// Confidence is 0 to 1.
	Confidence float64
	// Method is "inference" (read from the pixels), "c2pa" (the file's
	// Content Credentials say so), or any other value exactly as sent.
	Method string
	// TopGenerator is the likeliest generator. Nil unless the picture reads
	// as likely generated, and on every "c2pa" reading.
	TopGenerator *TopGenerator
	// C2PASigner is who signed the file's Content Credentials. Nil unless
	// the file carries Content Credentials that are trusted.
	C2PASigner *string
	// C2PA is "untrusted" when the file carries Content Credentials that are
	// not trusted, or any other value exactly as sent. Nil when the server
	// sent none.
	C2PA *string
}

// TopGenerator is the likeliest image generator of an AIGeneratedReading.
type TopGenerator struct {
	Name        string
	Probability float64
}

// UnknownReading holds a reading this version of the package does not know,
// or one with a field missing or of the wrong type. Read it with Raw.
type UnknownReading struct{ readingBase }

// parseEvidence reads the body's evidence object, one entry per booster in
// the body's order. Nil when the body has none.
func parseEvidence(data json.RawMessage) EvidenceList {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	out := EvidenceList{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return out
		}
		name, _ := t.(string)
		var x any
		if dec.Decode(&x) != nil {
			return out
		}
		e := &Evidence{Booster: name, Results: []Reading{}}
		m, _ := x.(map[string]any)
		e.Raw = m
		e.Version, _ = m["version"].(string)
		e.Status, _ = m["status"].(string)
		if f, ok := m["latency_ms"].(float64); ok {
			e.LatencyMs = &f
		}
		if n, ok := wholeNumber(m["billed_calls"]); ok {
			e.BilledCalls = &n
		}
		if list, ok := m["results"].([]any); ok {
			for _, r := range list {
				e.Results = append(e.Results, parseReading(name, r))
			}
		}
		out = append(out, e)
	}
	return out
}

// parseReading reads one reading of a booster. A reading of a known booster
// with a field missing or of the wrong type is an *UnknownReading, as a
// reading of an unknown booster is: the decision does not fail. Fields this
// version does not know are ignored.
func parseReading(booster string, v any) Reading {
	raw, _ := v.(map[string]any)
	base := readingBase{raw: raw}
	unknown := &UnknownReading{base}
	if raw == nil {
		return unknown
	}
	image, ok := raw["image"].(string)
	if !ok {
		return unknown
	}
	switch booster {
	case BoosterAge:
		faces, ok := wholeNumber(raw["faces"])
		if !ok {
			return unknown
		}
		r := &AgeReading{readingBase: base, Image: image, Faces: faces}
		for _, f := range []struct {
			key string
			to  **int
		}{{"age_years", &r.AgeYears}, {"low", &r.Low}, {"high", &r.High}} {
			switch x := raw[f.key].(type) {
			case nil:
			case float64:
				n, ok := wholeNumber(x)
				if !ok {
					return unknown
				}
				*f.to = &n
			default:
				return unknown
			}
		}
		switch x := raw["confidence"].(type) {
		case nil:
		case float64:
			r.Confidence = &x
		default:
			return unknown
		}
		if faces > 0 && (r.AgeYears == nil || r.Low == nil || r.High == nil || r.Confidence == nil) {
			return unknown
		}
		return r
	case BoosterAIGenerated:
		generated, ok1 := raw["generated"].(float64)
		confidence, ok2 := raw["confidence"].(float64)
		method, ok3 := raw["method"].(string)
		if !ok1 || !ok2 || !ok3 {
			return unknown
		}
		r := &AIGeneratedReading{readingBase: base, Image: image, Generated: generated, Confidence: confidence, Method: method}
		// The API always writes top_generator and c2pa_signer, null or not.
		_, hasTop := raw["top_generator"]
		_, hasSigner := raw["c2pa_signer"]
		if !hasTop || !hasSigner {
			return unknown
		}
		switch x := raw["top_generator"].(type) {
		case nil:
		case map[string]any:
			name, ok1 := x["name"].(string)
			p, ok2 := x["probability"].(float64)
			if !ok1 || !ok2 {
				return unknown
			}
			r.TopGenerator = &TopGenerator{Name: name, Probability: p}
		default:
			return unknown
		}
		switch x := raw["c2pa_signer"].(type) {
		case nil:
		case string:
			r.C2PASigner = &x
		default:
			return unknown
		}
		switch x := raw["c2pa"].(type) {
		case nil:
		case string:
			r.C2PA = &x
		default:
			return unknown
		}
		return r
	}
	return unknown
}

// wholeNumber reads a JSON number that holds a whole value.
func wholeNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// BoosterStatus is one booster's entry of the x-decisionnode-boosters
// response header: how it ended and how long it took.
type BoosterStatus struct {
	// Name is the booster's name.
	Name string
	// Status is "ok", "timeout", "error" or "skipped", or any other value
	// exactly as sent.
	Status string
	// LatencyMs is the booster's time in milliseconds. Nil when the header
	// gave none.
	LatencyMs *float64
}

// ParseBoosters reads the x-decisionnode-boosters header of a response
// (age=ok:212,ai_generated=timeout:800) into one entry per booster, in the
// request's order. Nil when the header is absent.
func ParseBoosters(header http.Header) []BoosterStatus {
	values := header.Values("x-decisionnode-boosters")
	if len(values) == 0 {
		return nil
	}
	out := []BoosterStatus{}
	for _, member := range strings.Split(strings.Join(values, ","), ",") {
		member = strings.TrimSpace(member)
		if member == "" {
			continue
		}
		name, value, _ := strings.Cut(member, "=")
		status, latency, hasLatency := strings.Cut(strings.TrimSpace(value), ":")
		s := BoosterStatus{Name: strings.TrimSpace(name), Status: strings.TrimSpace(status)}
		if hasLatency {
			if ms, err := strconv.ParseFloat(strings.TrimSpace(latency), 64); err == nil {
				s.LatencyMs = &ms
			}
		}
		out = append(out, s)
	}
	return out
}
