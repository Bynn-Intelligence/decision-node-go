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
	// BoosterFaceRecognition matches the faces in a picture against a
	// collection of faces: one of the workspace's own or a public one.
	BoosterFaceRecognition = "face_recognition"
	// BoosterOCR reads the text in a picture, or answers a prompt about it.
	BoosterOCR = "ocr"
	// BoosterDepth is Depth Vision: the distance in meters to what is in a
	// picture, the nearest obstacle straight ahead, seven directions from
	// left to right, and a grid. Estimates from one picture, for decisions
	// and planning, never a safety stop.
	BoosterDepth = "bynn_depth"
	// BoosterSegment is Segment Vision: finds what a prompt names in a
	// picture, each match's outline, box and centre, and with depth on its
	// distance and size in meters. It needs a prompt: see SegmentBooster.
	BoosterSegment = "bynn_segment"
)

// OCRBooster is the ocr booster with a prompt: a task, an instruction or a
// JSON schema for what to read from the picture, at most 2,000 characters
// (the API's limit, not checked here). Sent as {"name": "ocr", "options":
// {"prompt": ...}}.
func OCRBooster(prompt string) Booster {
	return Booster{Name: BoosterOCR, Options: map[string]any{"prompt": prompt}}
}

// FaceRecognitionBooster is the face_recognition booster searching a
// collection: a public collection's name or the id of one of the
// workspace's own. A threshold of 0 to 1 sets the lowest score a match may
// have; nil leaves the server's default (0.5). Sent as {"name":
// "face_recognition", "options": {"collection": ..., "threshold": ...}}, as
// given: the API checks both.
func FaceRecognitionBooster(collection string, threshold *float64) Booster {
	opts := map[string]any{"collection": collection}
	if threshold != nil {
		opts["threshold"] = *threshold
	}
	return Booster{Name: BoosterFaceRecognition, Options: opts}
}

// DepthBooster is Depth Vision with its grid of gridSize by gridSize cells,
// 2 to 16 (the server's default is 8, and the API checks the value). Sent as
// {"name": "bynn_depth", "options": {"grid_size": ...}}; name BoosterDepth
// alone for the default grid.
func DepthBooster(gridSize int) Booster {
	return Booster{Name: BoosterDepth, Options: map[string]any{"grid_size": gridSize}}
}

// SegmentOptions are Segment Vision's settings. Prompts is required; the
// rest is sent only when set. The API checks every value.
type SegmentOptions struct {
	// Prompts are what to find, 1 to 4 phrases of at most 120 characters
	// each, such as "landing pad". One phrase is sent as a string.
	Prompts []string
	// MaxInstances is the most matches per picture, 1 to 16 (8).
	MaxInstances *int
	// MinScore is the lowest score a match may have, 0 to 1 (0.25).
	MinScore *float64
	// PolygonPoints is the most points an outline has, 3 to 64 (24).
	PolygonPoints *int
	// Depth reads each match's distance and size in meters (true).
	Depth *bool
}

// SegmentBooster is Segment Vision with its prompt and settings, sent as
// {"name": "bynn_segment", "options": {"prompt": ..., ...}}.
func SegmentBooster(o SegmentOptions) Booster {
	opts := map[string]any{}
	if len(o.Prompts) == 1 {
		opts["prompt"] = o.Prompts[0]
	} else {
		opts["prompt"] = o.Prompts
	}
	if o.MaxInstances != nil {
		opts["max_instances"] = *o.MaxInstances
	}
	if o.MinScore != nil {
		opts["min_score"] = *o.MinScore
	}
	if o.PolygonPoints != nil {
		opts["polygon_points"] = *o.PolygonPoints
	}
	if o.Depth != nil {
		opts["depth"] = *o.Depth
	}
	return Booster{Name: BoosterSegment, Options: opts}
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

// namesBooster reports whether an encoded boosters member names the booster,
// by its bare name or in the object form {"name": ...}.
func namesBooster(member []byte, name string) bool {
	if len(member) == 0 {
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(member, []byte(`,"boosters":`)), &list) != nil {
		return false
	}
	for _, raw := range list {
		var bare string
		if json.Unmarshal(raw, &bare) == nil && bare == name {
			return true
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Name == name {
			return true
		}
	}
	return false
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
	// Results holds one reading per picture the booster was to read: a
	// reading of its booster's type (Reading lists them) or an
	// *UnknownReading. A picture
	// it did not read (it timed out or failed: {"image"}) or could not
	// decode ({"image", "readable": false}) is an *UnknownReading, and
	// Status says why. Empty, never nil, when there are none.
	Results []Reading
	// Raw is the evidence object as sent.
	Raw map[string]any
}

// Reading is one booster's reading of one image: an *AgeReading, an
// *AIGeneratedReading, a *FaceDetectionReading, a *FaceRecognitionReading, an
// *OCRReading, a *DepthReading, a *SegmentReading, or an *UnknownReading for
// a booster this version of the package does not know or a reading with a
// field missing or of the wrong type.
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

// FaceBox is a face's box by its corners, exactly as sent: whole pixels of
// the image, or fractions of its width and height when every point and box
// question of the request asks for normalized units.
type FaceBox struct {
	X1, Y1, X2, Y2 float64
}

// FaceDetection is one face a face_detection reading found.
type FaceDetection struct {
	Box FaceBox
	// Confidence is 0 to 1.
	Confidence float64
}

// FaceDetectionReading is the face_detection booster's reading of one
// picture: Faces, how many faces it found, and Detections, each face's box
// and confidence in the order sent. The list is capped (at 64), so Faces can
// be larger than len(Detections).
type FaceDetectionReading struct {
	readingBase
	// Image is the id of the image read.
	Image      string
	Faces      int
	Detections []FaceDetection
}

// FaceMatch is one face of a face_recognition reading that matches an item
// of the collection searched: its box, the match score, the collection and
// the item's reference, and Name (a public figure's) or Label (an item of
// the workspace's own collection that has one), "" when not sent.
type FaceMatch struct {
	Box FaceBox
	// Score is 0 to 1.
	Score      float64
	Collection string
	Reference  string
	Name       string
	Label      string
	// CollectionName is an own collection's display name or its id, or a
	// public collection's id; "" when not sent.
	CollectionName string
	// Metadata is an own item's metadata as sent; nil when not sent.
	Metadata map[string]any
}

// FaceRecognitionReading is the face_recognition booster's reading of one
// picture: Faces, how many faces were searched, and Matches, the faces that
// match an item of the collection, by score.
type FaceRecognitionReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// CollectionName is an own collection's display name; "" when not sent
	// (a public collection).
	CollectionName string
	Faces          int
	Matches        []FaceMatch
}

// OCRReading is the ocr booster's reading of one picture: the text it shows,
// or the answer to the prompt, exactly as sent ("" when the picture shows no
// text).
type OCRReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// Text is the text read.
	Text string
	// Truncated is true when the reading was cut at the context limit.
	Truncated bool
}

// DepthReading is Depth Vision's reading of one picture, in meters. The
// distances are estimates from one picture, for decisions and planning,
// never a safety stop.
type DepthReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// Ahead is the distance to the nearest obstacle straight ahead (the
	// middle third of the picture's width), the floor left out; nil when
	// none is found (null or not sent).
	Ahead *float64
	// Directions is the same reading in 7 equal strips of the picture, left
	// to right, each nil when none is found. Always 7 entries: any other
	// count makes the reading an *UnknownReading.
	Directions []*float64
	// Ground is true when a floor or ground was found and left out of Ahead
	// and Directions.
	Ground bool
	// Grid is GridRows rows of GridCols distances, row by row from the top
	// left.
	GridRows, GridCols int
	Grid               [][]float64
	// DepthMin, DepthMax and DepthMedian are the nearest, farthest and
	// median distances in the picture.
	DepthMin, DepthMax, DepthMedian float64
	// Width and Height are the picture's size in pixels.
	Width, Height int
}

// SegmentReading is Segment Vision's reading of one picture: what each
// prompt found. Coordinates are fractions of the picture's width (X) and
// height (Y) from its top left; distances and sizes are meters, estimates
// from one picture, never a safety stop.
type SegmentReading struct {
	readingBase
	// Image is the id of the image read.
	Image string
	// Width and Height are the picture's size in pixels.
	Width, Height int
	// Prompts are the phrases as read.
	Prompts []string
	// Instances are the matches, by score; empty when none was found.
	Instances []SegmentInstance
	// DepthSource is "bynn_depth" when the distances were read, "" with
	// depth off or unavailable.
	DepthSource string
	// AssumedHFOVDeg is the horizontal field of view, in degrees, the sizes
	// assume; nil when not sent.
	AssumedHFOVDeg *float64
}

// SegmentPoint is a point of the picture, as fractions from its top left.
type SegmentPoint struct{ X, Y float64 }

// SegmentBox is a match's box, from its top left (X1, Y1) to its bottom
// right (X2, Y2).
type SegmentBox struct{ X1, Y1, X2, Y2 float64 }

// SegmentInstance is one match of a SegmentReading.
type SegmentInstance struct {
	// Label is the phrase it matched.
	Label string
	// Score is 0 to 1.
	Score float64
	Box   SegmentBox
	// Polygon is its outline.
	Polygon []SegmentPoint
	// Area is its share of the picture.
	Area     float64
	Centroid SegmentPoint
	// Opening is the largest hole it encloses (a gate's, a doorway's, a
	// ring's); nil when there is none.
	Opening *SegmentOpening
	// Depth is nil with depth off.
	Depth *SegmentDepth
	// SizeM is its size in meters; nil with depth off.
	SizeM *SegmentSize
}

// SegmentOpening is the hole a match encloses.
type SegmentOpening struct {
	Polygon  []SegmentPoint
	Centroid SegmentPoint
	// Width and Height are fractions of the picture's.
	Width, Height float64
	// Offset is the opening's centre less the box's, each -1 to 1.
	Offset SegmentPoint
	Area   float64
}

// SegmentDepth is a match's distance in meters: the median inside its
// outline, inside its opening and on the opening's rim, each nil when none.
type SegmentDepth struct {
	Median, OpeningMedian, OpeningRimMedian *float64
}

// SegmentSize is a match's size in meters, from the assumed field of view.
type SegmentSize struct {
	Width, Height float64
	// Opening is the opening's size; nil when there is none.
	Opening *SegmentOpeningSize
}

// SegmentOpeningSize is an opening's size in meters.
type SegmentOpeningSize struct {
	Width, Height float64
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
	case BoosterFaceDetection:
		faces, ok := wholeNumber(raw["faces"])
		list, isList := raw["detections"].([]any)
		if !ok || faces < 0 || !isList {
			return unknown
		}
		r := &FaceDetectionReading{readingBase: base, Image: image, Faces: faces, Detections: []FaceDetection{}}
		for _, x := range list {
			d, _ := x.(map[string]any)
			box, okBox := faceBox(d["box"])
			conf, okConf := d["confidence"].(float64)
			if !okBox || !okConf {
				return unknown
			}
			r.Detections = append(r.Detections, FaceDetection{Box: box, Confidence: conf})
		}
		return r
	case BoosterFaceRecognition:
		faces, ok := wholeNumber(raw["faces"])
		list, isList := raw["matches"].([]any)
		if !ok || faces < 0 || !isList {
			return unknown
		}
		colName, okColName := optionalString(raw["collection_name"])
		if !okColName {
			return unknown
		}
		r := &FaceRecognitionReading{readingBase: base, Image: image, CollectionName: colName, Faces: faces, Matches: []FaceMatch{}}
		for _, x := range list {
			m, _ := x.(map[string]any)
			box, okBox := faceBox(m["box"])
			score, okScore := m["score"].(float64)
			collection, okCollection := m["collection"].(string)
			ref, okRef := m["reference"].(string)
			name, okName := optionalString(m["name"])
			label, okLabel := optionalString(m["label"])
			matchColName, okMatchColName := optionalString(m["collection_name"])
			metadata, okMetadata := m["metadata"].(map[string]any)
			if m["metadata"] == nil {
				okMetadata = true
			}
			if !okBox || !okScore || !okCollection || !okRef || !okName || !okLabel || !okMatchColName || !okMetadata {
				return unknown
			}
			r.Matches = append(r.Matches, FaceMatch{Box: box, Score: score, Collection: collection, Reference: ref, Name: name, Label: label,
				CollectionName: matchColName, Metadata: metadata})
		}
		return r
	case BoosterOCR:
		// The API always sends both.
		text, ok1 := raw["text"].(string)
		truncated, ok2 := raw["truncated"].(bool)
		if !ok1 || !ok2 {
			return unknown
		}
		return &OCRReading{readingBase: base, Image: image, Text: text, Truncated: truncated}
	case BoosterDepth:
		if r := parseDepth(raw, image); r != nil {
			r.readingBase = base
			return r
		}
	case BoosterSegment:
		if r := parseSegment(raw, image); r != nil {
			r.readingBase = base
			return r
		}
	}
	return unknown
}

// parseDepth reads a Depth Vision reading; nil when a field is missing or
// of the wrong type. Ahead may be null or left out; Directions has exactly 7
// entries; the grid's sides and the picture's are whole numbers of 0 or
// more.
func parseDepth(raw map[string]any, image string) *DepthReading {
	ahead, okAhead := nullableNumber(raw, "ahead")
	if _, present := raw["ahead"]; !present {
		ahead, okAhead = nil, true
	}
	ground, okGround := raw["ground"].(bool)
	rows, okRows := size(raw["grid_rows"])
	cols, okCols := size(raw["grid_cols"])
	nearest, okMin := raw["depth_min"].(float64)
	farthest, okMax := raw["depth_max"].(float64)
	median, okMedian := raw["depth_median"].(float64)
	width, okWidth := size(raw["width"])
	height, okHeight := size(raw["height"])
	dirs, okDirs := raw["directions"].([]any)
	grid, okGrid := raw["grid"].([]any)
	if !okAhead || !okGround || !okRows || !okCols || !okMin || !okMax || !okMedian || !okWidth || !okHeight || !okDirs || len(dirs) != 7 || !okGrid {
		return nil
	}
	r := &DepthReading{Image: image, Ahead: ahead, Ground: ground, GridRows: rows, GridCols: cols, DepthMin: nearest, DepthMax: farthest,
		DepthMedian: median, Width: width, Height: height, Directions: make([]*float64, 0, len(dirs)), Grid: make([][]float64, 0, len(grid))}
	for _, d := range dirs {
		switch x := d.(type) {
		case nil:
			r.Directions = append(r.Directions, nil)
		case float64:
			r.Directions = append(r.Directions, &x)
		default:
			return nil
		}
	}
	for _, row := range grid {
		cells, ok := row.([]any)
		if !ok {
			return nil
		}
		out := make([]float64, 0, len(cells))
		for _, c := range cells {
			f, ok := c.(float64)
			if !ok {
				return nil
			}
			out = append(out, f)
		}
		r.Grid = append(r.Grid, out)
	}
	return r
}

// parseSegment reads a Segment Vision reading; nil when a field is missing
// or of the wrong type.
func parseSegment(raw map[string]any, image string) *SegmentReading {
	width, okWidth := size(raw["width"])
	height, okHeight := size(raw["height"])
	prompts, okPrompts := raw["prompts"].([]any)
	instances, okInstances := raw["instances"].([]any)
	source, okSource := optionalString(raw["depth_source"])
	hfov, okHFOV := nullableNumber(raw, "assumed_hfov_deg")
	if _, present := raw["assumed_hfov_deg"]; !present {
		hfov, okHFOV = nil, true
	}
	if !okWidth || !okHeight || !okPrompts || !okInstances || !okSource || !okHFOV {
		return nil
	}
	r := &SegmentReading{Image: image, Width: width, Height: height, Prompts: make([]string, 0, len(prompts)),
		Instances: make([]SegmentInstance, 0, len(instances)), DepthSource: source, AssumedHFOVDeg: hfov}
	for _, p := range prompts {
		s, ok := p.(string)
		if !ok {
			return nil
		}
		r.Prompts = append(r.Prompts, s)
	}
	for _, x := range instances {
		in, ok := segmentInstance(x)
		if !ok {
			return nil
		}
		r.Instances = append(r.Instances, in)
	}
	return r
}

func segmentInstance(v any) (SegmentInstance, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return SegmentInstance{}, false
	}
	label, okLabel := m["label"].(string)
	score, okScore := m["score"].(float64)
	area, okArea := m["area"].(float64)
	box, okBox := numbers(m["box"], 4)
	polygon, okPolygon := segmentPoints(m["polygon"])
	centroid, okCentroid := segmentPoint(m["centroid"])
	if !okLabel || !okScore || !okArea || !okBox || !okPolygon || !okCentroid {
		return SegmentInstance{}, false
	}
	in := SegmentInstance{Label: label, Score: score, Box: SegmentBox{box[0], box[1], box[2], box[3]}, Polygon: polygon, Area: area, Centroid: centroid}
	switch o := m["opening"].(type) {
	case nil:
	case map[string]any:
		poly, ok1 := segmentPoints(o["polygon"])
		c, ok2 := segmentPoint(o["centroid"])
		w, ok3 := o["width"].(float64)
		h, ok4 := o["height"].(float64)
		off, ok5 := segmentPoint(o["offset"])
		a, ok6 := o["area"].(float64)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
			return SegmentInstance{}, false
		}
		in.Opening = &SegmentOpening{Polygon: poly, Centroid: c, Width: w, Height: h, Offset: off, Area: a}
	default:
		return SegmentInstance{}, false
	}
	switch d := m["depth"].(type) {
	case nil:
	case map[string]any:
		median, ok1 := optionalNumber(d["median"])
		opening, ok2 := optionalNumber(d["opening_median"])
		rim, ok3 := optionalNumber(d["opening_rim_median"])
		if !ok1 || !ok2 || !ok3 {
			return SegmentInstance{}, false
		}
		in.Depth = &SegmentDepth{Median: median, OpeningMedian: opening, OpeningRimMedian: rim}
	default:
		return SegmentInstance{}, false
	}
	switch z := m["size_m"].(type) {
	case nil:
	case map[string]any:
		w, ok1 := z["width"].(float64)
		h, ok2 := z["height"].(float64)
		if !ok1 || !ok2 {
			return SegmentInstance{}, false
		}
		in.SizeM = &SegmentSize{Width: w, Height: h}
		switch o := z["opening"].(type) {
		case nil:
		case map[string]any:
			ow, ok1 := o["width"].(float64)
			oh, ok2 := o["height"].(float64)
			if !ok1 || !ok2 {
				return SegmentInstance{}, false
			}
			in.SizeM.Opening = &SegmentOpeningSize{Width: ow, Height: oh}
		default:
			return SegmentInstance{}, false
		}
	default:
		return SegmentInstance{}, false
	}
	return in, true
}

// nullableNumber reads a field that must be there, a number or null.
func nullableNumber(m map[string]any, key string) (*float64, bool) {
	v, present := m[key]
	if !present {
		return nil, false
	}
	switch x := v.(type) {
	case nil:
		return nil, true
	case float64:
		return &x, true
	}
	return nil, false
}

// optionalNumber reads a field that is a number, null or not sent.
func optionalNumber(v any) (*float64, bool) {
	switch x := v.(type) {
	case nil:
		return nil, true
	case float64:
		return &x, true
	}
	return nil, false
}

// size reads a whole number of 0 or more: a count of cells or pixels.
func size(v any) (int, bool) {
	n, ok := wholeNumber(v)
	return n, ok && n >= 0
}

// numbers reads a list of exactly n numbers.
func numbers(v any, n int) ([]float64, bool) {
	list, ok := v.([]any)
	if !ok || len(list) != n {
		return nil, false
	}
	out := make([]float64, n)
	for i, x := range list {
		f, ok := x.(float64)
		if !ok {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

func segmentPoint(v any) (SegmentPoint, bool) {
	xy, ok := numbers(v, 2)
	if !ok {
		return SegmentPoint{}, false
	}
	return SegmentPoint{X: xy[0], Y: xy[1]}, true
}

func segmentPoints(v any) ([]SegmentPoint, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]SegmentPoint, 0, len(list))
	for _, x := range list {
		p, ok := segmentPoint(x)
		if !ok {
			return nil, false
		}
		out = append(out, p)
	}
	return out, true
}

// optionalString reads a field that is a string or absent (or null): ""
// when absent, ok false for any other value.
func optionalString(v any) (string, bool) {
	if v == nil {
		return "", true
	}
	s, ok := v.(string)
	return s, ok
}

// faceBox reads a box's four corners, each a JSON number.
func faceBox(v any) (FaceBox, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return FaceBox{}, false
	}
	var corners [4]float64
	for i, k := range []string{"x1", "y1", "x2", "y2"} {
		f, ok := m[k].(float64)
		if !ok {
			return FaceBox{}, false
		}
		corners[i] = f
	}
	return FaceBox{X1: corners[0], Y1: corners[1], X2: corners[2], Y2: corners[3]}, true
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
