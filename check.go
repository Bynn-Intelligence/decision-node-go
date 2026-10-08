package decisionnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"strings"
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
// rules. A field of a type the API would refuse with a 422 of its own is left
// to the server; only the rules below are checked here.
func (qc *questionCheck) question(id string, raw []byte) (typ string) {
	var q map[string]json.RawMessage
	if json.Unmarshal(raw, &q) != nil {
		return ""
	}
	if json.Unmarshal(q["type"], &typ) != nil {
		return ""
	}
	loc := []any{"body", "questions", id, typ}
	switch typ {
	case "choice":
		if n, ok := countObject(q["criteria"]); ok {
			switch {
			case n == 0:
				qc.refuse(localBadRequest("", "Choice question must have at least one choice: "+id,
					fmt.Sprintf("question '%s': a choice needs at least one option", id)))
			case n > MaxChoiceOptions:
				qc.refuse(localBadRequest("", fmt.Sprintf("Too many choices. Must have at most %d choices.", MaxChoiceOptions),
					fmt.Sprintf("question '%s': a choice takes up to %d options, got %d", id, MaxChoiceOptions, n)))
			}
		}
	case "score":
		n, ok := countArray(q["criteria"])
		if !ok {
			n, ok = countObject(q["criteria"])
		}
		if ok {
			switch {
			case n == 0:
				qc.entry("too_short", append(loc, "criteria"), "List should have at least 1 item after validation, not 0",
					json.RawMessage(q["criteria"]), nil, fmt.Sprintf("question '%s': a score needs at least one level", id))
			case n > MaxScoreLevels:
				qc.refuse(localBadRequest("", fmt.Sprintf("Too many score levels. Must have at most %d levels.", MaxScoreLevels),
					fmt.Sprintf("question '%s': a score takes up to %d levels, got %d", id, MaxScoreLevels, n)))
			}
		}
	case "truth", "noul":
		var crit map[string]json.RawMessage
		if isNull(q["criteria"]) || json.Unmarshal(q["criteria"], &crit) == nil {
			if isNull(q["instructions"]) && isNull(crit["true"]) && isNull(crit["false"]) {
				name := "Truth"
				if typ == "noul" {
					name = "Noul"
				}
				qc.refuse(localBadRequest("", name+" question must have criteria or instructions: "+id,
					fmt.Sprintf("question '%s': a %s question needs instructions or criteria", id, typ)))
			}
		}
	case "number":
		qc.number(id, q, raw, loc)
	}
	return typ
}

// number checks a number question's grid as the API builds it:
// exact decimals from the JSON text, one 422 entry per faulty field, then the
// decimal bound, min above max and the grid size, then the instructions.
func (qc *questionCheck) number(id string, q map[string]json.RawMessage, raw []byte, loc []any) {
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
	if n.Cmp(big.NewInt(MaxNumberGridValues)) > 0 {
		qc.entry("too_long", loc, fmt.Sprintf("Grid should have at most %d values, not %s", MaxNumberGridValues, n),
			json.RawMessage(raw), map[string]any{"field_type": "Grid", "max_length": MaxNumberGridValues, "actual_length": json.Number(n.String())},
			fmt.Sprintf("question '%s': the grid holds %s values, the limit is %d; narrow the range or widen the step", id, n, MaxNumberGridValues))
		return
	}
	var text string
	if isNull(q["instructions"]) || (json.Unmarshal(q["instructions"], &text) == nil && text == "") {
		qc.refuse(localBadRequest("", "Number question must have instructions: "+id,
			fmt.Sprintf("question '%s': a number question needs instructions", id)))
	}
}

// checkImages checks a request's images against the API's fixed rules: at
// most 4 (URLs included), ids present and unique, URLs https and at most
// 2,048 characters, local bytes JPEG, PNG or WebP by their first bytes and
// matching a declared type, at most 10 MB and 40 megapixels.
func checkImages(images []Image) error {
	if len(images) > MaxImages {
		return localBadRequest("api_usage_error", fmt.Sprintf("Too many images. Must have at most %d images.", MaxImages),
			fmt.Sprintf("a request takes up to %d images, got %d", MaxImages, len(images)))
	}
	seen := make(map[string]bool, len(images))
	for i, img := range images {
		if img.id == "" {
			return usageError(fmt.Sprintf("Image %d needs an id.", i))
		}
		if seen[img.id] {
			return usageError("Duplicate image id: " + img.id)
		}
		seen[img.id] = true
		if err := checkImage(img); err != nil {
			return err
		}
	}
	return nil
}

// checkImage checks one image, whatever request or frame it travels in.
func checkImage(img Image) error {
	switch {
	case img.url != "":
		if !httpsURL(img.url) || len(img.url) > MaxImageURLChars {
			return usageError(fmt.Sprintf("Image %s: only https URLs of at most %d characters are accepted.", img.id, MaxImageURLChars))
		}
		return nil
	case img.uploadID != "":
		if img.mediaType != "" && !mediaOK[img.mediaType] {
			return usageError(fmt.Sprintf("Image %s: media_type must be image/jpeg, image/png or image/webp.", img.id))
		}
		return nil
	case img.src == nil:
		return usageError(fmt.Sprintf("Image %s needs exactly one of data, url or upload_id.", img.id))
	}
	found, w, h, dimsOK := img.src.info()
	switch {
	case found == "":
		return usageError(fmt.Sprintf("Image %s is not a JPEG, PNG or WebP image.", img.id))
	case img.mediaType != "" && img.mediaType != found:
		return usageError(fmt.Sprintf("Image %s could not be decoded as %s.", img.id, img.mediaType))
	case img.src.oversize || len(img.src.data) > MaxImageBytes:
		return usageError(fmt.Sprintf("Image %s is larger than 10 MB.", img.id))
	case !dimsOK:
		return usageError(fmt.Sprintf("Image %s: its header cannot be read.", img.id))
	case int64(w)*int64(h) > MaxImagePixels:
		return usageError(fmt.Sprintf("Image %s is %dx%d pixels, over 40 megapixels.", img.id, w, h))
	}
	return nil
}

func usageError(message string) error { return localBadRequest("api_usage_error", message, message) }

// httpsURL accepts an absolute https URL with a host.
func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// countObject returns the number of members of a JSON object.
func countObject(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return 0, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return 0, false
	}
	return len(m), true
}

// countArray returns the number of elements of a JSON array.
func countArray(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return 0, false
	}
	var a []json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return 0, false
	}
	return len(a), true
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
