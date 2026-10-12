package decisionnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"
)

// The API's fixed question limits, checked locally before any request.
const (
	// MaxChoiceOptions is the most options a choice question may have.
	MaxChoiceOptions = 255
	// MaxScoreLevels is the most levels a score question may have.
	MaxScoreLevels = 10
	// MaxNumberGridValues is the most values a number question's grid may hold.
	MaxNumberGridValues = 4096
	// maxNumberDecimals is the most decimals of a number question's min and step.
	maxNumberDecimals = 6
	// MinPlaces and MaxPlaces bound the max of a point or box question asked
	// for several places.
	MinPlaces = 2
	MaxPlaces = 16
)

var (
	two53     = new(big.Rat).SetInt64(1 << 53)
	negTwo53  = new(big.Rat).Neg(two53)
	ratZero   = new(big.Rat)
	ratTen    = big.NewInt(10)
	mediaOK   = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}
	staleKind = map[string]bool{"upload_expired": true, "upload_consumed": true, "upload_not_found": true, "upload_incomplete": true}
)

// questionCheck gathers the faults of a request's questions in the order the
// API reports them: every 422 entry of every question first, else the first
// 400.
type questionCheck struct {
	entries  []any
	reason   string
	deferred error
	// imageIDs are the request's image ids, for the point and box rules,
	// when places is set; a session's questions are checked without them.
	imageIDs []string
	places   bool
}

func (qc *questionCheck) entry(kind string, loc []any, msg string, input any, ctx map[string]any, reason string) {
	e := map[string]any{"type": kind, "loc": loc, "msg": msg, "input": input}
	if ctx != nil {
		e["ctx"] = ctx
	}
	if qc.reason == "" {
		qc.reason = reason
	}
	qc.entries = append(qc.entries, e)
}

func (qc *questionCheck) refuse(err error) {
	if qc.deferred == nil {
		qc.deferred = err
	}
}

// err returns the error for the faults found, or nil.
func (qc *questionCheck) err() error {
	if len(qc.entries) > 0 {
		reason := qc.reason
		if len(qc.entries) > 1 {
			reason += fmt.Sprintf(" and %d more", len(qc.entries)-1)
		}
		return localValidation(reason, qc.entries...)
	}
	return qc.deferred
}

// question checks one question in its JSON form against the API's fixed
// rules, in the server's order. It returns the question's type, "" when it has none that is read.
func (qc *questionCheck) question(id string, raw []byte, lim limits) (typ string) {
	base := []any{"body", "questions", id}
	q, ok := objectMembers(raw)
	if !ok {
		qc.entry("model_type", base, "Input should be a valid dictionary or object", json.RawMessage(raw), nil,
			fmt.Sprintf("question '%s': must be an object", id))
		return ""
	}
	if isNull(q.get("type")) {
		qc.entry("missing", append(base, "type"), "Field required", json.RawMessage(raw), nil,
			fmt.Sprintf("question '%s': type: Field required", id))
		return ""
	}
	if json.Unmarshal(q.get("type"), &typ) != nil || !knownTypes[typ] {
		qc.refuse(localBadRequest("api_usage_error", "Invalid request.",
			fmt.Sprintf("question '%s': unknown type %s; the types are %s", id, compactJSON(q.get("type")), "truth, choice, score, number, point and box")))
		return ""
	}
	loc := []any{"body", "questions", id, typ}
	if v := q.get("instructions"); !isNull(v) && !isSDL(v) {
		qc.sdl(append(loc, "instructions"), v, fmt.Sprintf("question '%s': instructions must be a string, an object or an array", id))
	}
	criteria := q.get("criteria")
	switch typ {
	case "choice":
		most := MaxChoiceOptions
		if lim.maxChoiceOptions > 0 {
			most = lim.maxChoiceOptions
		}
		opts, isObj := objectMembers(criteria)
		switch {
		case isNull(criteria):
			qc.entry("missing", append(loc, "criteria"), "Field required", json.RawMessage(raw), nil,
				fmt.Sprintf("question '%s': criteria: Field required", id))
		case !isObj:
			qc.entry("dict_type", append(loc, "criteria"), "Input should be a valid dictionary", json.RawMessage(criteria), nil,
				fmt.Sprintf("question '%s': criteria must be an object of options", id))
		default:
			for _, m := range opts {
				if !isNull(m.value) && !isSDL(m.value) {
					qc.sdl(append(append([]any(nil), loc...), "criteria", m.name), m.value,
						fmt.Sprintf("question '%s': option %q must be a string, an object or an array", id, m.name))
				}
			}
			switch n := len(opts); {
			case n == 0:
				qc.refuse(localBadRequest("", "Choice question must have at least one choice: "+id,
					fmt.Sprintf("question '%s': a choice needs at least one option", id)))
			case n > most:
				qc.refuse(localBadRequest("", fmt.Sprintf("Too many choices. Must have at most %d choices.", most),
					fmt.Sprintf("question '%s': a choice takes up to %d options, got %d", id, most, n)))
			}
		}
	case "score":
		most := MaxScoreLevels
		if lim.maxScoreLevels > 0 {
			most = lim.maxScoreLevels
		}
		if isNull(criteria) {
			qc.entry("missing", append(loc, "criteria"), "Field required", json.RawMessage(raw), nil,
				fmt.Sprintf("question '%s': criteria: Field required", id))
			return typ
		}
		levels, ok := arrayElements(criteria)
		if !ok {
			levels, ok = indexedLevels(criteria)
		}
		if !ok {
			qc.entry("list_type", append(loc, "criteria"), "Input should be a valid list", json.RawMessage(criteria), nil,
				fmt.Sprintf("question '%s': criteria must be a list of levels", id))
			return typ
		}
		if len(levels) == 0 {
			qc.entry("too_short", append(loc, "criteria"), "List should have at least 1 item after validation, not 0",
				json.RawMessage(criteria), nil, fmt.Sprintf("question '%s': a score needs at least one level", id))
			return typ
		}
		for i, level := range levels {
			if !isSDL(level) {
				qc.sdl(append(append([]any(nil), loc...), "criteria", i), level,
					fmt.Sprintf("question '%s': level %d must be a string, an object or an array", id, i))
			}
		}
		if len(levels) > most {
			qc.refuse(localBadRequest("", fmt.Sprintf("Too many score levels. Must have at most %d levels.", most),
				fmt.Sprintf("question '%s': a score takes up to %d levels, got %d", id, most, len(levels))))
		}
	case "truth", "noul":
		sides, isObj := objectMembers(criteria)
		if !isNull(criteria) && !isObj {
			qc.entry("dict_type", append(loc, "criteria"), "Input should be a valid dictionary", json.RawMessage(criteria), nil,
				fmt.Sprintf("question '%s': criteria must be an object", id))
			return typ
		}
		for _, side := range []string{"true", "false"} {
			if v := sides.get(side); !isNull(v) && !isSDL(v) {
				qc.sdl(append(append([]any(nil), loc...), "criteria", side), v,
					fmt.Sprintf("question '%s': criteria.%s must be a string, an object or an array", id, side))
			}
		}
		if isNull(q.get("instructions")) && isNull(sides.get("true")) && isNull(sides.get("false")) {
			name := "Truth"
			if typ == "noul" {
				name = "Noul"
			}
			qc.refuse(localBadRequest("", name+" question must have criteria or instructions: "+id,
				fmt.Sprintf("question '%s': a %s question needs instructions or criteria", id, typ)))
		}
	case "number":
		if lim.gridHeld && lim.maxGridValues == 0 {
			qc.refuse(localBadRequest("api_usage_error", fmt.Sprintf("Model %s does not answer number questions.", lim.model),
				fmt.Sprintf("question '%s': model %s does not answer number questions", id, lim.model)))
			return typ
		}
		most := MaxNumberGridValues
		if lim.maxGridValues > 0 {
			most = lim.maxGridValues
		}
		qc.number(id, q.asMap(), raw, loc, most)
	case "point", "box":
		qc.place(id, typ, q, raw, loc)
	}
	return typ
}

// unitsOK are the units a point or box answers in.
var unitsOK = map[string]bool{"pixels": true, "normalized": true}

// place checks a point or box question: its image among the request's
// (422 entries), its units (a 422 entry), then its instructions (the plain
// 400).
func (qc *questionCheck) place(id, typ string, q members, raw []byte, loc []any) {
	at := func(field string) []any { return append(append([]any(nil), loc...), field) }
	if qc.places {
		image := q.get("image")
		var name string
		trimmed := bytes.TrimSpace(image)
		isString := len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &name) == nil
		switch {
		case len(qc.imageIDs) == 0:
			qc.entry("image_required", loc, fmt.Sprintf("A %s question needs an image in the request", typ), json.RawMessage(raw), nil,
				fmt.Sprintf("question '%s': a %s question needs an image in the request", id, typ))
		case isNull(image):
			if len(qc.imageIDs) >= 2 {
				qc.entry("missing", at("image"), "Field required", json.RawMessage(raw), nil,
					fmt.Sprintf("question '%s': image: Field required with %d images", id, len(qc.imageIDs)))
			}
		case !isString:
			qc.entry("string_type", at("image"), "Input should be a valid string", json.RawMessage(image), nil,
				fmt.Sprintf("question '%s': image must be a string", id))
		case !containsString(qc.imageIDs, name):
			qc.entry("unknown_image", at("image"), "Input should be the id of an image of the request", json.RawMessage(image), nil,
				fmt.Sprintf("question '%s': image %s is not an image of the request", id, compactJSON(image)))
		}
	}
	if units := q.get("units"); !isNull(units) {
		var u string
		if json.Unmarshal(units, &u) != nil || !unitsOK[u] {
			qc.entry("literal_error", at("units"), "Input should be 'pixels' or 'normalized'", json.RawMessage(units),
				map[string]any{"expected": "'pixels' or 'normalized'"}, fmt.Sprintf("question '%s': units must be pixels or normalized", id))
		}
	}
	if most := q.get("max"); !isNull(most) {
		qc.placesMax(id, most, at("max"))
	}
	if isNull(q.get("instructions")) {
		name := strings.ToUpper(typ[:1]) + typ[1:]
		qc.refuse(localBadRequest("", fmt.Sprintf("%s question must have instructions: %s", name, id),
			fmt.Sprintf("question '%s': a %s question needs instructions", id, typ)))
	}
}

// sdl adds the three entries the API gives a value that must be a string,
// an object or an array.
func (qc *questionCheck) sdl(loc []any, v json.RawMessage, reason string) {
	for _, e := range []struct{ kind, suffix, msg string }{
		{"string_type", "str", "Input should be a valid string"},
		{"dict_type", "dict[any,any]", "Input should be a valid dictionary"},
		{"list_type", "list[any]", "Input should be a valid list"},
	} {
		qc.entry(e.kind, append(append([]any(nil), loc...), e.suffix), e.msg, json.RawMessage(v), nil, reason)
	}
}

// knownTypes are the question types the API takes (the older name of truth
// included, for a question sent in its wire form).
var knownTypes = map[string]bool{"truth": true, "noul": true, "choice": true, "score": true, "number": true, "point": true, "box": true}

// isSDL reports a JSON string, object or array.
func isSDL(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && (raw[0] == '"' || raw[0] == '{' || raw[0] == '[')
}

// member is one member of a JSON object, in document order.
type member struct {
	name  string
	value json.RawMessage
}

type members []member

func (ms members) get(name string) json.RawMessage {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].name == name {
			return ms[i].value
		}
	}
	return nil
}

func (ms members) asMap() map[string]json.RawMessage {
	m := make(map[string]json.RawMessage, len(ms))
	for _, x := range ms {
		m[x.name] = x.value
	}
	return m
}

// objectMembers reads a JSON object's members in order.
func objectMembers(raw json.RawMessage) (members, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	var out members
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, member{name: t.(string), value: v})
	}
	if out == nil {
		out = members{}
	}
	return out, true
}

func arrayElements(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	return a, true
}

// indexedLevels reads levels sent as an object keyed "0" to "K-1".
func indexedLevels(raw json.RawMessage) ([]json.RawMessage, bool) {
	ms, ok := objectMembers(raw)
	if !ok {
		return nil, false
	}
	m := ms.asMap()
	out := make([]json.RawMessage, len(m))
	for i := range out {
		v, ok := m[fmt.Sprint(i)]
		if !ok {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// number checks a number question's grid as the API builds it:
// exact decimals from the JSON text, one 422 entry per faulty field, then the
// decimal bound, min above max and the grid size, then the instructions.
func (qc *questionCheck) number(id string, q map[string]json.RawMessage, raw []byte, loc []any, most int) {
	defaults := map[string]string{"min": "0", "max": "255", "step": "1"}
	values := map[string]*big.Rat{}
	ok := true
	for _, name := range []string{"min", "max", "step"} {
		sent := q[name]
		floc := append(append([]any(nil), loc...), name)
		if isNull(sent) {
			values[name], _ = new(big.Rat).SetString(defaults[name])
			continue
		}
		v, isNum := jsonNumber(sent)
		if !isNum {
			qc.entry("float_type", floc, "Input should be a valid number", json.RawMessage(sent), nil,
				fmt.Sprintf("question '%s': %s must be a number, got %s", id, name, jsonKind(sent)))
			ok = false
			continue
		}
		switch {
		case name != "step" && v.Cmp(negTwo53) < 0:
			qc.entry("greater_than_equal", floc, "Input should be greater than or equal to -9007199254740992", json.RawMessage(sent),
				map[string]any{"ge": json.Number("-9007199254740992")}, fmt.Sprintf("question '%s': %s must be within plus or minus 2^53", id, name))
			ok = false
			continue
		case name != "step" && v.Cmp(two53) > 0:
			qc.entry("less_than_equal", floc, "Input should be less than or equal to 9007199254740992", json.RawMessage(sent),
				map[string]any{"le": json.Number("9007199254740992")}, fmt.Sprintf("question '%s': %s must be within plus or minus 2^53", id, name))
			ok = false
			continue
		case name == "step" && v.Cmp(ratZero) <= 0:
			qc.entry("greater_than", floc, "Input should be greater than 0", json.RawMessage(sent),
				map[string]any{"gt": json.Number("0")}, fmt.Sprintf("question '%s': step must be above 0, got %s", id, compactJSON(sent)))
			ok = false
			continue
		case name != "max" && decimalPlaces(v) > maxNumberDecimals:
			qc.entry("decimal_max_places", floc, fmt.Sprintf("Decimal input should have no more than %d decimal places", maxNumberDecimals),
				json.RawMessage(sent), map[string]any{"decimal_places": maxNumberDecimals},
				fmt.Sprintf("question '%s': %s has %d decimals, at most %d", id, name, decimalPlaces(v), maxNumberDecimals))
			ok = false
			continue
		}
		values[name] = v
	}
	if !ok {
		return
	}
	lo, hi, step := values["min"], values["max"], values["step"]
	d := max(decimalPlaces(lo), decimalPlaces(step))
	if d > 0 {
		// Every value must fit 15 significant digits: 10^(15-d) - 10^-d.
		bound := new(big.Rat).Sub(new(big.Rat).SetInt(pow10(15-d)), new(big.Rat).SetFrac(big.NewInt(1), pow10(d)))
		btext := decimalText(bound)
		negBound := new(big.Rat).Neg(bound)
		for _, name := range []string{"min", "max"} {
			val := values[name]
			sent := json.RawMessage(q[name])
			if isNull(q[name]) {
				sent = json.RawMessage(defaults[name])
			}
			if val.Cmp(bound) > 0 {
				qc.entry("less_than_equal", append(append([]any(nil), loc...), name), "Input should be less than or equal to "+btext, sent,
					map[string]any{"le": json.Number(btext)},
					fmt.Sprintf("question '%s': %s must be within plus or minus %s on a grid with %d decimals, so every value fits 15 significant digits", id, name, btext, d))
				return
			}
			if val.Cmp(negBound) < 0 {
				qc.entry("greater_than_equal", append(append([]any(nil), loc...), name), "Input should be greater than or equal to -"+btext, sent,
					map[string]any{"ge": json.Number("-" + btext)},
					fmt.Sprintf("question '%s': %s must be within plus or minus %s on a grid with %d decimals, so every value fits 15 significant digits", id, name, btext, d))
				return
			}
		}
	}
	if lo.Cmp(hi) > 0 {
		maxSent, minSent := json.RawMessage(defaults["max"]), json.RawMessage(defaults["min"])
		if !isNull(q["max"]) {
			maxSent = q["max"]
		}
		if !isNull(q["min"]) {
			minSent = q["min"]
		}
		qc.entry("less_than_equal", append(append([]any(nil), loc...), "min"), "Input should be less than or equal to "+compactJSON(maxSent), minSent,
			map[string]any{"le": maxSent}, fmt.Sprintf("question '%s': min %s is above max %s", id, compactJSON(minSent), compactJSON(maxSent)))
		return
	}
	// N = floor((max - min) / step) + 1, exactly.
	span := new(big.Rat).Quo(new(big.Rat).Sub(hi, lo), step)
	n := new(big.Int).Quo(span.Num(), span.Denom())
	n.Add(n, big.NewInt(1))
	if n.Cmp(big.NewInt(int64(most))) > 0 {
		qc.entry("too_long", loc, fmt.Sprintf("Grid should have at most %d values, not %s", most, n),
			json.RawMessage(raw), map[string]any{"field_type": "Grid", "max_length": most, "actual_length": json.Number(n.String())},
			fmt.Sprintf("question '%s': the grid holds %s values, the limit is %d; narrow the range or widen the step", id, n, most))
		return
	}
	var text string
	if isNull(q["instructions"]) || (json.Unmarshal(q["instructions"], &text) == nil && text == "") {
		qc.refuse(localBadRequest("", "Number question must have instructions: "+id,
			fmt.Sprintf("question '%s': a number question needs instructions", id)))
	}
}

// checkImages checks a request's images against the API's fixed rules: ids
// present, at most 64 characters and unique, URLs https and fetchable by the server's rules, local
// bytes JPEG, PNG or WebP by their first bytes and matching a declared type,
// at most 10 MB and 40 megapixels. Their count is one of the request's 422
// entries (countEntries).
func checkImages(images []Image, lim limits) error {
	seen := make(map[string]bool, len(images))
	for i, img := range images {
		if img.id == "" {
			return usageError(fmt.Sprintf("Image %d needs an id.", i))
		}
		if utf8.RuneCountInString(img.id) > MaxImageIDChars {
			return usageError(fmt.Sprintf("Image %d: its id is longer than %d characters.", i, MaxImageIDChars))
		}
		if seen[img.id] {
			return usageError("Duplicate image id: " + img.id)
		}
		seen[img.id] = true
		if err := checkImage(img, lim); err != nil {
			return err
		}
	}
	return nil
}

// checkImage checks one image, whatever request or frame it travels in.
func checkImage(img Image, lim limits) error {
	switch {
	case img.url != "":
		return checkImageURL(img.id, img.url)
	case img.uploadID != "":
		if img.mediaType != "" && !mediaOK[img.mediaType] {
			return usageError(fmt.Sprintf("Image %s: media_type must be image/jpeg, image/png or image/webp.", img.id))
		}
		return nil
	case img.src == nil:
		return usageError(fmt.Sprintf("Image %s needs exactly one of data, url or upload_id.", img.id))
	}
	biggest := MaxImageBytes
	if lim.maxImageBytes > 0 {
		biggest = lim.maxImageBytes
	}
	found, w, h, dimsOK := img.src.info()
	switch {
	case found == "":
		return usageError(fmt.Sprintf("Image %s is not a JPEG, PNG or WebP image.", img.id))
	case img.mediaType != "" && img.mediaType != found:
		return usageError(fmt.Sprintf("Image %s could not be decoded as %s.", img.id, img.mediaType))
	case img.src.oversize || len(img.src.data) > biggest:
		return usageError(fmt.Sprintf("Image %s is larger than 10 MB.", img.id))
	case !dimsOK:
		return usageError(fmt.Sprintf("Image %s: its header cannot be read.", img.id))
	case int64(w)*int64(h) > MaxImagePixels:
		return usageError(fmt.Sprintf("Image %s is %dx%d pixels, over 40 megapixels.", img.id, w, h))
	}
	return nil
}

// countEntries adds the API's 422 too_long entry for more images than a
// request takes (16, or the held limit) and for more videos (4), after the
// request's other entries, the images' before the videos'. The entry's input
// is the list in its wire form, a local image as {"id", "media_type"}: nothing
// is encoded or uploaded to build an error.
func (qc *questionCheck) countEntries(images []Image, videos []encodedVideo, lim limits) {
	mostImages, mostVideos := MaxImages, MaxVideos
	if lim.maxImages > 0 {
		mostImages = lim.maxImages
	}
	if lim.maxVideos > 0 {
		mostVideos = lim.maxVideos
	}
	if n := len(images); n > mostImages {
		given := make([]any, n)
		for i, img := range images {
			given[i] = imageInput(img)
		}
		qc.tooLong("images", given, mostImages)
	}
	if n := len(videos); n > mostVideos {
		given := make([]any, n)
		for i, v := range videos {
			if v.typed == nil {
				given[i] = json.RawMessage(v.raw)
				continue
			}
			types := make([]string, len(v.typed.Frames))
			for j, f := range v.typed.Frames {
				types[j] = f.MediaType()
			}
			given[i] = json.RawMessage(v.typed.appendWire(nil, types))
		}
		qc.tooLong("videos", given, mostVideos)
	}
}

func (qc *questionCheck) tooLong(name string, given []any, most int) {
	qc.entry("too_long", []any{"body", name}, fmt.Sprintf("List should have at most %d items after validation, not %d", most, len(given)),
		given, map[string]any{"field_type": "List", "max_length": most, "actual_length": len(given)},
		fmt.Sprintf("%d %s, the limit is %d", len(given), name, most))
}

// imageInput is an image as a 422 entry lists it: its wire form, a local
// image's bytes left out.
func imageInput(img Image) map[string]any {
	out := map[string]any{"id": img.id}
	switch {
	case img.url != "":
		out["url"] = img.url
		return out
	case img.uploadID != "":
		out["upload_id"] = img.uploadID
	}
	if t := img.MediaType(); t != "" {
		out["media_type"] = t
	}
	return out
}

// blockedRanges are the addresses the server never fetches an image from.
var blockedRanges = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001:db8::/32", "fd00:ec2::254/128",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// blockedAddress reports a literal IP address in a blocked range, an
// IPv4-mapped or NAT64 address judged by the IPv4 address it embeds. A DNS
// name is the server's to resolve and judge.
func blockedAddress(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.WithZone("")
	switch {
	case addr.Is4In6():
		addr = addr.Unmap()
	case addr.Is6() && nat64.Contains(addr):
		b := addr.As16()
		addr = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	for _, p := range blockedRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// checkImageURL applies the server's URL rules that need no network: https
// (else api_usage_error), then image_url_refused for the length, user
// information, a port other than 443, no host or a blocked literal address.
func checkImageURL(id, raw string) error {
	if !strings.HasPrefix(raw, "https://") {
		return usageError(fmt.Sprintf("Image %s: only https URLs are accepted.", id))
	}
	reason := ""
	u, err := url.Parse(raw)
	switch {
	case len(raw) > MaxImageURLChars:
		reason = fmt.Sprintf("the URL is %d characters, over the limit of %d", len(raw), MaxImageURLChars)
	case err != nil:
		reason = "the URL cannot be read"
	case u.User != nil:
		reason = "the URL carries user information"
	case u.Port() != "" && u.Port() != "443":
		reason = "the URL uses a port other than 443"
	case u.Hostname() == "":
		reason = "the URL has no host"
	case blockedAddress(u.Hostname()):
		reason = "the URL resolves to a private address"
	}
	if reason != "" {
		msg := fmt.Sprintf("Image '%s': %s.", id, reason)
		return localBadRequest("image_url_refused", msg, msg)
	}
	return nil
}

// placesMax checks the max of a point or box question asked for several
// places: a whole number, then at least MinPlaces, then at most MaxPlaces.
func (qc *questionCheck) placesMax(id string, most json.RawMessage, loc []any) {
	n, isInt := jsonInt(most)
	switch {
	case !isInt:
		qc.entry("int_type", loc, "Input should be a valid integer", most, nil,
			fmt.Sprintf("question '%s': max must be a whole number", id))
	case n.Cmp(big.NewInt(MinPlaces)) < 0:
		qc.entry("greater_than_equal", loc, fmt.Sprintf("Input should be greater than or equal to %d", MinPlaces), most,
			map[string]any{"ge": MinPlaces}, fmt.Sprintf("question '%s': max must be at least %d", id, MinPlaces))
	case n.Cmp(big.NewInt(MaxPlaces)) > 0:
		qc.entry("less_than_equal", loc, fmt.Sprintf("Input should be less than or equal to %d", MaxPlaces), most,
			map[string]any{"le": MaxPlaces}, fmt.Sprintf("question '%s': max must be at most %d", id, MaxPlaces))
	}
}

// jsonInt reads a JSON integer literal (no fraction, no exponent), of any
// size; ok is false for any other value.
func jsonInt(v json.RawMessage) (n *big.Int, ok bool) {
	t := string(bytes.TrimSpace(v))
	if t == "" || strings.ContainsAny(t, ".eE") {
		return nil, false
	}
	n, ok = new(big.Int).SetString(t, 10)
	return n, ok && t[0] != '+'
}

// limits are the values the client holds from GET /v1/models for one model,
// which replace or add to the documented bounds of the local checks. The
// zero value holds nothing, and every check then uses the documented bound or
// is left to the server.
type limits struct {
	model            string
	maxChoiceOptions int
	maxScoreLevels   int
	maxImages        int
	maxImageBytes    int
	maxVideos        int
	gridHeld         bool
	maxGridValues    int
	sessionsHeld     bool
	sessionsEnabled  bool
	maxWindow        int
	maxTTLSeconds    int
	requestsPerBatch int
	maxCustomIDChars int
}

func limitsOf(name string, m ModelInfo) limits {
	l := limits{model: name}
	num := func(key string) (int, bool) {
		f, ok := m.Raw[key].(float64)
		return int(f), ok
	}
	l.maxChoiceOptions, _ = num("max_choice_options")
	l.maxScoreLevels, _ = num("max_score_levels")
	l.maxImages, _ = num("max_images")
	l.maxImageBytes, _ = num("max_image_bytes")
	// The API publishes the image and video limits in objects of their own.
	nested := func(group, key string) int {
		g, _ := m.Raw[group].(map[string]any)
		f, _ := g[key].(float64)
		return int(f)
	}
	if l.maxImages == 0 {
		l.maxImages = nested("images", "max_images")
	}
	if l.maxImageBytes == 0 {
		l.maxImageBytes = nested("images", "max_image_bytes")
	}
	l.maxVideos = nested("videos", "max_videos")
	l.maxGridValues, l.gridHeld = num("number_max_grid_values")
	if m.Sessions != nil {
		if enabled, ok := m.Raw["sessions"].(map[string]any)["enabled"].(bool); ok {
			l.sessionsHeld, l.sessionsEnabled = true, enabled
		}
		l.maxWindow, l.maxTTLSeconds = m.Sessions.MaxWindow, m.Sessions.MaxTTLSeconds
	}
	if m.Batch != nil {
		l.requestsPerBatch, l.maxCustomIDChars = m.Batch.RequestsPerBatch, m.Batch.MaxCustomIDChars
	}
	return l
}

// maxNesting is the most objects and arrays a body may hold inside each
// other, its own object included.
const maxNesting = 128

// jsonDepth returns how many objects and arrays sit inside each other in a
// JSON text, read without decoding it.
func jsonDepth(b []byte) int {
	depth, deepest, inString := 0, 0, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inString:
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

func checkNesting(depth int) error {
	if depth > maxNesting {
		return localBadRequest("api_usage_error", "Invalid request.",
			fmt.Sprintf("the request nests %d objects and arrays inside each other; the limit is %d", depth, maxNesting))
	}
	return nil
}

func usageError(message string) error { return localBadRequest("api_usage_error", message, message) }

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// jsonNumber reads a JSON number literal as an exact rational.
func jsonNumber(raw json.RawMessage) (*big.Rat, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !(raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) {
		return nil, false
	}
	return new(big.Rat).SetString(string(raw))
}

func jsonKind(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0:
		return "nothing"
	case raw[0] == '"':
		return "a string"
	case raw[0] == 't' || raw[0] == 'f':
		return "a boolean"
	case raw[0] == '{':
		return "an object"
	}
	return "an array"
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

func pow10(n int) *big.Int { return new(big.Int).Exp(ratTen, big.NewInt(int64(n)), nil) }

// decimalPlaces is the number of digits after the point of an exact decimal
// once trailing zeros are dropped: 0.5 has 1, 7 has 0. A decimal's
// denominator is 2^a * 5^b, and its places are the larger of a and b.
func decimalPlaces(r *big.Rat) int {
	d := new(big.Int).Set(r.Denom())
	twos, fives := 0, 0
	two, five, rem := big.NewInt(2), big.NewInt(5), new(big.Int)
	for d.Cmp(big.NewInt(1)) > 0 {
		q, m := new(big.Int).QuoRem(d, two, rem)
		if m.Sign() == 0 {
			d, twos = q, twos+1
			continue
		}
		q, m = new(big.Int).QuoRem(d, five, rem)
		if m.Sign() == 0 {
			d, fives = q, fives+1
			continue
		}
		break
	}
	return max(twos, fives)
}

// decimalText writes an exact decimal in its shortest plain form.
func decimalText(r *big.Rat) string {
	s := r.FloatString(decimalPlaces(r))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}
