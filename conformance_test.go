package decisionnode_test

// The conformance suite: language-neutral cases run against the suite's mock
// server. Point DECISIONNODE_CONFORMANCE_DIR at a checkout of the suite. When
// it is unset the test fails: it is never skipped.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

var envVars = []string{"DECISIONNODE_API_KEY", "DECISIONNODE_BASE_URL", "DECISIONNODE_MODEL"}

var exactKeys = map[string]bool{"probabilities": true, "legend": true, "raw": true, "entries": true, "safety": true, "body": true, "limits": true}

func suiteDir(t *testing.T) string {
	dir := os.Getenv("DECISIONNODE_CONFORMANCE_DIR")
	if dir == "" {
		t.Fatal("DECISIONNODE_CONFORMANCE_DIR is not set: point it at a checkout of the conformance suite")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

type mock struct {
	url string
}

func startMock(t *testing.T, root string) *mock {
	python := os.Getenv("PYTHON")
	if python == "" {
		python = "python3"
	}
	// The mock runs unchanged, with a listen backlog large enough for the
	// first burst of decide_many connections. With the standard library's
	// default of 5, some systems reset connections beyond the fifth, which the
	// client then retries as connection errors and the attempt count drifts.
	launcher := "import runpy, socketserver, sys; socketserver.TCPServer.request_queue_size = 128; " +
		"sys.argv = sys.argv[1:]; runpy.run_path(sys.argv[0], run_name='__main__')"
	cmd := exec.Command(python, "-c", launcher, filepath.Join(root, "mock", "server.py"), "--port", "0", "--cases", filepath.Join(root, "cases"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the mock: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read the mock's first line: %v", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "listening on ") {
		t.Fatalf("the mock did not start: %q", line)
	}
	go func() { _, _ = io.Copy(io.Discard, r) }()
	return &mock{url: strings.TrimPrefix(line, "listening on ")}
}

type mockRecord struct {
	Time       float64           `json:"time"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
	Status     *int              `json:"status"`
	Headers    map[string]string `json:"headers"`
	BodyText   string            `json:"body_text"`
	BodyBase64 string            `json:"body_base64"`
}

type mockLog struct {
	Requests    []mockRecord `json:"requests"`
	MaxInFlight float64      `json:"max_in_flight"`
	// Pongs counts the pongs the mock received on a session's socket; they
	// are not requests.
	Pongs int `json:"-"`
}

func (m *mock) requests(t *testing.T, session string) mockLog {
	return m.query(t, "session="+url.QueryEscape(session))
}

// requestsAt returns the requests any session made to path.
func (m *mock) requestsAt(t *testing.T, path string) mockLog {
	return m.query(t, "session=*&path="+url.QueryEscape(path))
}

func (m *mock) query(t *testing.T, query string) mockLog {
	resp, err := http.Get(m.url + "/_mock/requests?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out mockLog
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	kept := out.Requests[:0]
	for _, r := range out.Requests {
		if r.Method == "PONG" {
			out.Pongs++
			continue
		}
		kept = append(kept, r)
	}
	out.Requests = kept
	return out
}

func (m *mock) clockNow(t *testing.T, sid string) time.Time {
	resp, err := http.Get(m.url + "/_mock/clock?session=" + url.QueryEscape(sid))
	if err != nil {
		t.Error(err)
		return time.Now()
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Epoch float64 `json:"epoch_seconds"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return time.Unix(0, int64(out.Epoch*float64(time.Second)))
}

func (m *mock) post(t *testing.T, path string, body any) {
	resp, err := http.Post(m.url+path, "application/json", bytes.NewReader(raw(body)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// ---------------------------------------------------------------- ordered JSON

// object is a JSON object that keeps its key order.
type object struct {
	keys []string
	vals map[string]any
}

func (o *object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func parseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return readValue(dec)
}

func readValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			o := &object{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				val, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				if _, seen := o.vals[k]; !seen {
					o.keys = append(o.keys, k)
				}
				o.vals[k] = val
			}
			_, err := dec.Token()
			return o, err
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := readValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			_, err := dec.Token()
			return arr, err
		}
	}
	return tok, nil
}

// plain turns an ordered value into the shapes encoding/json produces.
func plain(v any) any {
	switch x := v.(type) {
	case *object:
		m := make(map[string]any, len(x.keys))
		for _, k := range x.keys {
			m[k] = plain(x.vals[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case json.Number:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	}
	return v
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func get(o *object, key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[key]
	return v, ok
}

func str(v any) string { s, _ := v.(string); return s }

func pointer(doc any, ptr string) (any, bool) {
	if ptr == "" {
		return doc, true
	}
	cur := doc
	for _, part := range strings.Split(ptr[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case *object:
			v, ok := c.vals[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// ---------------------------------------------------------------- comparison

// patternOf is the regular expression of an expected {"$pattern": regex}.
func patternOf(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", false
	}
	p, ok := m["$pattern"].(string)
	return p, ok
}

func same(a, b any) bool {
	// An expected {"$pattern": regex} matches a string the regex finds.
	if p, ok := patternOf(a); ok {
		s, ok := b.(string)
		return ok && regexp.MustCompile(p).MatchString(s)
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !same(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !same(x[i], y[i]) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case nil:
		return b == nil
	}
	return false
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 200 {
		return string(b[:197]) + "..."
	}
	return string(b)
}

func match(expected, actual any, path string) error {
	em, eok := expected.(map[string]any)
	am, aok := actual.(map[string]any)
	if _, pattern := patternOf(expected); eok && aok && !pattern {
		keys := make([]string, 0, len(em))
		for k := range em {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			av, ok := am[k]
			if isAbsent(em[k]) {
				// Suite 1.17: the key must not be there at all.
				if ok {
					return fmt.Errorf("%s.%s: expected no such key, got %s", path, k, short(av))
				}
				continue
			}
			if !ok {
				return fmt.Errorf("%s.%s: missing (expected %s)", path, k, short(em[k]))
			}
			if exactKeys[k] {
				if !same(em[k], av) {
					return fmt.Errorf("%s.%s: expected %s, got %s", path, k, short(em[k]), short(av))
				}
			} else if err := match(em[k], av, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	}
	if !same(expected, actual) {
		return fmt.Errorf("%s: expected %s, got %s", path, short(expected), short(actual))
	}
	return nil
}

// isAbsent is the expectation {"$absent": true}.
func isAbsent(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) == 1 && m["$absent"] == true
}

// canon passes a value through JSON so it holds only the shapes same knows.
func canon(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func normSafety(s *dn.Safety) any {
	if s == nil {
		return nil
	}
	if !s.Checked {
		return map[string]any{"checked": false, "reason": nilIfEmpty(s.Reason)}
	}
	out := map[string]any{"checked": true, "harm": nil, "self_harm": nil, "flagged": s.Flagged}
	for k, v := range s.Scores {
		out[k] = v
	}
	return out
}

func normAnswer(a dn.Answer) any {
	switch x := a.(type) {
	case *dn.TruthAnswer:
		return map[string]any{"kind": "truth", "truth": x.Truth}
	case *dn.ChoiceAnswer:
		return map[string]any{"kind": "choice", "choice": x.Choice, "confidence": x.Confidence, "probabilities": x.Probabilities}
	case *dn.NumberAnswer:
		return map[string]any{"kind": "number", "number": x.Number, "expected": x.Expected, "confidence": x.Confidence, "probabilities": x.Probabilities}
	case *dn.PointAnswer:
		return map[string]any{"kind": "point", "x": x.X, "y": x.Y, "present": x.Present, "confidence": x.Confidence,
			"image": x.Image, "width": x.Width, "height": x.Height}
	case *dn.BoxAnswer:
		return map[string]any{"kind": "box", "x1": x.X1, "y1": x.Y1, "x2": x.X2, "y2": x.Y2, "present": x.Present,
			"confidence": x.Confidence, "image": x.Image, "width": x.Width, "height": x.Height}
	case *dn.PointsAnswer:
		places := []any{}
		for _, pl := range x.Points {
			places = append(places, map[string]any{"x": pl.X, "y": pl.Y, "confidence": pl.Confidence})
		}
		return map[string]any{"kind": "points", "count": x.Count, "present": x.Present, "complete": x.Complete,
			"points": places, "image": x.Image, "width": x.Width, "height": x.Height}
	case *dn.BoxesAnswer:
		places := []any{}
		for _, pl := range x.Boxes {
			places = append(places, map[string]any{"x1": pl.X1, "y1": pl.Y1, "x2": pl.X2, "y2": pl.Y2, "confidence": pl.Confidence})
		}
		return map[string]any{"kind": "boxes", "count": x.Count, "present": x.Present, "complete": x.Complete,
			"boxes": places, "image": x.Image, "width": x.Width, "height": x.Height}
	case *dn.ScoreAnswer:
		probs := map[string]any{}
		for k, v := range x.Probabilities {
			probs[strconv.Itoa(k)] = v
		}
		legend := map[string]any{}
		for k, v := range x.Legend {
			legend[strconv.Itoa(k)] = v
		}
		return map[string]any{"kind": "score", "score": x.Score, "confidence": x.Confidence, "probabilities": probs, "legend": legend}
	default:
		return map[string]any{"kind": "unknown", "type": a.Type(), "raw": a.Raw()}
	}
}

// normReading is a reading in the suite's form: its kind and fields, or the
// raw fields of a generic reading.
func normReading(r dn.Reading) any {
	switch x := r.(type) {
	case *dn.AgeReading:
		return map[string]any{"kind": "age", "image": x.Image, "faces": x.Faces, "age_years": intOrNil(x.AgeYears),
			"low": intOrNil(x.Low), "high": intOrNil(x.High), "confidence": floatOrNil(x.Confidence)}
	case *dn.AIGeneratedReading:
		var top any
		if x.TopGenerator != nil {
			top = map[string]any{"name": x.TopGenerator.Name, "probability": x.TopGenerator.Probability}
		}
		var signer any
		if x.C2PASigner != nil {
			signer = *x.C2PASigner
		}
		var c2pa any
		if x.C2PA != nil {
			c2pa = *x.C2PA
		}
		return map[string]any{"kind": "ai_generated", "image": x.Image, "generated": x.Generated, "confidence": x.Confidence,
			"method": x.Method, "top_generator": top, "c2pa_signer": signer, "c2pa": c2pa}
	case *dn.FaceDetectionReading:
		found := []any{}
		for _, d := range x.Detections {
			found = append(found, map[string]any{"box": normBox(d.Box), "confidence": d.Confidence})
		}
		return map[string]any{"kind": "face_detection", "image": x.Image, "faces": x.Faces, "detections": found}
	case *dn.FaceRecognitionReading:
		matches := []any{}
		for _, m := range x.Matches {
			matches = append(matches, map[string]any{"box": normBox(m.Box), "score": m.Score, "collection": m.Collection,
				"reference": m.Reference, "name": nilIfEmpty(m.Name), "label": nilIfEmpty(m.Label),
				"collection_name": nilIfEmpty(m.CollectionName), "metadata": m.Metadata})
		}
		return map[string]any{"kind": "face_recognition", "image": x.Image, "collection_name": nilIfEmpty(x.CollectionName), "faces": x.Faces, "matches": matches}
	case *dn.OCRReading:
		return map[string]any{"kind": "ocr", "image": x.Image, "text": x.Text, "truncated": x.Truncated}
	}
	return map[string]any{"kind": "unknown", "raw": r.Raw()}
}

func normBox(b dn.FaceBox) any {
	return map[string]any{"x1": b.X1, "y1": b.Y1, "x2": b.X2, "y2": b.Y2}
}

// normEvidence is a decision's evidence as the ordered pairs the
// expectations are turned into (orderedPairs), so the order is compared too.
func normEvidence(l dn.EvidenceList) any {
	if l == nil {
		return nil
	}
	out := []any{}
	for _, e := range l {
		results := []any{}
		for _, r := range e.Results {
			results = append(results, normReading(r))
		}
		out = append(out, map[string]any{"name": e.Booster, "value": map[string]any{
			"version": nilIfEmpty(e.Version), "status": nilIfEmpty(e.Status),
			"latency_ms": floatOrNil(e.LatencyMs), "billed_calls": intOrNil(e.BilledCalls), "results": results,
		}})
	}
	return out
}

// normBoosters is an error's booster statuses as ordered pairs; [] when the
// header was absent.
func normBoosters(list []dn.BoosterStatus) any {
	out := []any{}
	for _, b := range list {
		out = append(out, map[string]any{"name": b.Name, "value": map[string]any{"status": b.Status, "latency_ms": floatOrNil(b.LatencyMs)}})
	}
	return out
}

// orderedPairs turns the objects under the keys whose order the suite
// compares (a decision's evidence, an error's boosters) into lists of
// {name, value} pairs, in order, before the expectation is made plain.
func orderedPairs(v any) any {
	switch x := v.(type) {
	case *object:
		o := &object{keys: x.keys, vals: map[string]any{}}
		for _, k := range x.keys {
			val := x.vals[k]
			if inner, ok := val.(*object); ok && (k == "evidence" || k == "boosters") {
				pairs := []any{}
				for _, name := range inner.keys {
					pairs = append(pairs, &object{keys: []string{"name", "value"}, vals: map[string]any{"name": name, "value": orderedPairs(inner.vals[name])}})
				}
				o.vals[k] = pairs
				continue
			}
			o.vals[k] = orderedPairs(val)
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = orderedPairs(e)
		}
		return out
	}
	return v
}

// expectation is an ordered expectation made plain, with orderedPairs applied.
func expectation(v any) any { return plain(orderedPairs(v)) }

func normCost(c *dn.Cost) any {
	if c == nil {
		return nil
	}
	return map[string]any{"usd": c.USD, "model_usd": c.ModelUSD, "boosters_usd": c.BoostersUSD, "free_tier": c.FreeTier}
}

func normFallback(f *dn.Fallback) any {
	if f == nil {
		return nil
	}
	return map[string]any{"served_model": f.ServedModel, "requested_model": f.RequestedModel}
}

func normDecision(d *dn.Decision) any {
	var latency any
	if d.LatencyMs != nil {
		latency = *d.LatencyMs
	}
	answers := map[string]any{}
	for k, v := range d.Answers {
		answers[k] = normAnswer(v)
	}
	return canon(map[string]any{
		"model":      nilIfEmpty(d.Model),
		"request_id": nilIfEmpty(d.RequestID),
		"latency_ms": latency,
		"usage": map[string]any{
			"input_tokens":   intOrNil(d.Usage.InputTokens),
			"output_tokens":  intOrNil(d.Usage.OutputTokens),
			"video_frames":   intOrNil(d.Usage.VideoFrames),
			"video_seconds":  floatOrNil(d.Usage.VideoSeconds),
			"booster_tokens": intOrNil(d.Usage.BoosterTokens),
			"boosters":       intOrNil(d.Usage.Boosters),
			"cost":           normCost(d.Usage.Cost),
		},
		"warnings": d.Warnings,
		"evidence": normEvidence(d.Evidence),
		"fallback": normFallback(d.Fallback),
		"safety":   normSafety(d.Safety),
		"answers":  answers,
		"raw":      d.Raw,
	})
}

func floatOrNil(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// errorClasses names the error's class and every class it derives from.
func errorClasses(err error) []string {
	switch err.(type) {
	case *dn.ContextLimitError:
		return []string{"ContextLimitError", "BadRequestError", "DecisionNodeError"}
	case *dn.BadRequestError:
		return []string{"BadRequestError", "DecisionNodeError"}
	case *dn.AuthenticationError:
		return []string{"AuthenticationError", "DecisionNodeError"}
	case *dn.InsufficientCreditError:
		return []string{"InsufficientCreditError", "DecisionNodeError"}
	case *dn.RefusalError:
		return []string{"RefusalError", "DecisionNodeError"}
	case *dn.PermissionDeniedError:
		return []string{"PermissionDeniedError", "DecisionNodeError"}
	case *dn.NotFoundError:
		return []string{"NotFoundError", "DecisionNodeError"}
	case *dn.RequestTooLargeError:
		return []string{"RequestTooLargeError", "DecisionNodeError"}
	case *dn.ValidationError:
		return []string{"ValidationError", "DecisionNodeError"}
	case *dn.SessionLimitError:
		return []string{"SessionLimitError", "RateLimitError", "DecisionNodeError"}
	case *dn.RateLimitError:
		return []string{"RateLimitError", "DecisionNodeError"}
	case *dn.OverloadedError:
		return []string{"OverloadedError", "DecisionNodeError"}
	case *dn.ServerError:
		return []string{"ServerError", "DecisionNodeError"}
	case *dn.ConnectionError:
		return []string{"ConnectionError", "DecisionNodeError"}
	case *dn.TimeoutError:
		return []string{"TimeoutError", "DecisionNodeError"}
	case *dn.Error:
		return []string{"DecisionNodeError"}
	}
	return []string{fmt.Sprintf("%T", err)}
}

func normError(err error) any {
	var base *dn.Error
	if !errors.As(err, &base) {
		return map[string]any{}
	}
	out := map[string]any{
		"status":      nil,
		"error_type":  nilIfEmpty(base.ErrorType),
		"message":     err.Error(),
		"request_id":  nilIfEmpty(base.RequestID),
		"reason":      nilIfEmpty(base.Reason),
		"category":    nil,
		"safety":      nil,
		"entries":     nil,
		"retry_after": nil,
		"boosters":    normBoosters(base.Boosters),
		"details":     base.Details,
	}
	if base.Status != 0 {
		out["status"] = base.Status
	}
	var refusal *dn.RefusalError
	if errors.As(err, &refusal) {
		out["category"] = nilIfEmpty(refusal.Category)
		out["safety"] = normSafety(refusal.Safety)
	}
	var validation *dn.ValidationError
	if errors.As(err, &validation) && validation.Entries != nil {
		out["entries"] = validation.Entries
	}
	var wait time.Duration
	var rate *dn.RateLimitError
	var over *dn.OverloadedError
	var limit *dn.SessionLimitError
	if errors.As(err, &limit) {
		wait = limit.RetryAfter
	} else if errors.As(err, &rate) {
		wait = rate.RetryAfter
	} else if errors.As(err, &over) {
		wait = over.RetryAfter
	}
	if wait != 0 {
		out["retry_after"] = wait.Seconds()
	}
	return canon(out)
}

func checkError(spec map[string]any, err error, path string) error {
	cls, _ := spec["error"].(string)
	if cls == "" {
		cls, _ = spec["class"].(string)
	}
	if as, _ := spec["catch_as"].(string); as != "" {
		if as != "DecisionNodeError" {
			return fmt.Errorf("%s: unknown catch_as %q", path, as)
		}
		// Caught through the exported name, written in source: this does
		// not compile when the name is missing.
		var caught *dn.DecisionNodeError
		if !errors.As(err, &caught) {
			return fmt.Errorf("%s: %T is not caught as DecisionNodeError", path, err)
		}
	}
	names := errorClasses(err)
	if ok, _ := spec["subclass_ok"].(bool); ok {
		found := false
		for _, n := range names {
			found = found || n == cls
		}
		if !found {
			return fmt.Errorf("%s: expected %s or a subclass, got %s", path, cls, names[0])
		}
	} else if names[0] != cls {
		return fmt.Errorf("%s: expected %s, got %s (%v)", path, cls, names[0], err)
	}
	fields, _ := spec["fields"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}
	return match(fields, normError(err), path+".fields")
}

// ---------------------------------------------------------------- calls

// refs holds the results earlier steps saved, for {"$ref": "name.path"}.
type refs map[string]any

func (r refs) resolve(path string) any {
	name, rest, _ := strings.Cut(path, ".")
	cur := r[name]
	if rest == "" {
		return cur
	}
	for _, part := range strings.Split(rest, ".") {
		switch x := cur.(type) {
		case *dn.Batch:
			if part == "id" {
				cur = x.ID
			} else {
				cur = x.Raw[part]
			}
		case *dn.Collection:
			cur = x.Raw[part]
		case *dn.CollectionItem:
			cur = x.Raw[part]
		case *dn.UsagePage:
			cur = x.Raw[part]
		case *dn.CreatedKey:
			cur = x.Raw[part]
		case *dn.Key:
			cur = x.Raw[part]
		case *dn.KeyPage:
			cur = x.Raw[part]
		case []dn.Key:
			i, _ := strconv.Atoi(part)
			cur = x[i].Raw
		case []string:
			i, _ := strconv.Atoi(part)
			cur = x[i]
		case map[string]any:
			cur = x[part]
		default:
			return nil
		}
	}
	return cur
}

// special decodes a one-key object that stands for something else: $ref and
// $text. ok is false for any other value.
func special(v any, r refs) (any, bool) {
	o, isObj := v.(*object)
	if !isObj || len(o.keys) != 1 {
		return nil, false
	}
	args, _ := o.vals[o.keys[0]].(*object)
	switch o.keys[0] {
	case "$ref":
		return r.resolve(str(o.vals["$ref"])), true
	case "$text":
		n, _ := get(args, "times")
		times, _ := n.(json.Number).Int64()
		return strings.Repeat(str(args.vals["repeat"]), int(times)), true
	case "$nest":
		// The value inside depth arrays: [[[...value...]]].
		n, _ := get(args, "depth")
		depth, _ := n.(json.Number).Int64()
		out := json.RawMessage(raw(plainOrRaw(args.vals["value"], r)))
		for i := 0; i < int(depth); i++ {
			out = append(append(json.RawMessage("["), out...), ']')
		}
		return out, true
	}
	return nil, false
}

func plainOrRaw(v any, r refs) any {
	if x, ok := special(v, r); ok {
		return x
	}
	return raw(v)
}

func decodeQuestion(v any) any {
	o, ok := v.(*object)
	if ok && len(o.keys) == 1 {
		args, _ := o.vals[o.keys[0]].(*object)
		switch o.keys[0] {
		case "$truth":
			return dn.TruthWith(argValue(args, "instructions"), argValue(args, "true"), argValue(args, "false"))
		case "$choice":
			return dn.Choice(argValue(args, "instructions"), decodeOptions(args.vals["options"]))
		case "$score":
			return dn.Score(argValue(args, "instructions"), argValue(args, "levels"))
		case "$number":
			return dn.NumberWith(argValue(args, "instructions"), argValue(args, "min"), argValue(args, "max"), argValue(args, "step"))
		case "$point", "$box":
			return decodePlace(o.keys[0], args)
		}
	}
	return raw(v)
}

// decodePlace builds a point or box question. A whole-number max goes
// through Points or Boxes; any other max (2.5, "8", true) is set as given,
// for the local check to refuse.
func decodePlace(key string, args *object) dn.Question {
	instructions, image, units := argValue(args, "instructions"), str(args.vals["image"]), str(args.vals["units"])
	most := argValue(args, "max")
	if n, ok := most.(json.Number); ok && !strings.ContainsAny(string(n), ".eE") {
		if i, err := n.Int64(); err == nil {
			if key == "$point" {
				return dn.PointsWith(instructions, image, units, int(i))
			}
			return dn.BoxesWith(instructions, image, units, int(i))
		}
	}
	q := dn.PointWith(instructions, image, units)
	if key == "$box" {
		q = dn.BoxWith(instructions, image, units)
	}
	if most != nil {
		q.Max = most
	}
	return q
}

func decodeOptions(v any) any {
	switch x := v.(type) {
	case *object:
		opts := dn.Options{}
		for _, k := range x.keys {
			var desc any
			if x.vals[k] != nil {
				desc = raw(x.vals[k])
			}
			opts = append(opts, dn.ChoiceOption{Name: k, Description: desc})
		}
		return opts
	case []any:
		names := make([]string, len(x))
		for i, n := range x {
			names[i] = str(n)
		}
		return names
	}
	return raw(v)
}

// argValue returns a plain-data argument: nil when absent or null, a string
// as is, a number as its exact literal, else its exact JSON.
func argValue(args *object, key string) any {
	v, ok := get(args, key)
	if !ok || v == nil {
		return nil
	}
	return plainArg(v, nil)
}

func plainArg(v any, r refs) any {
	if x, ok := special(v, r); ok {
		return x
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x
	}
	return raw(v)
}

// decodeBoosters reads a request's boosters: {"$booster": {name, images?,
// require?, options?}} is a dn.Booster with only the arguments present, a
// name or any other value is passed as is.
func decodeBoosters(v any, r refs) []any {
	arr := listArg(v)
	if arr == nil {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		o, ok := item.(*object)
		if !ok || len(o.keys) != 1 || o.keys[0] != "$booster" {
			out = append(out, plainArg(item, r))
			continue
		}
		args := o.vals["$booster"].(*object)
		b := dn.Booster{Name: str(args.vals["name"])}
		if v, ok := get(args, "images"); ok {
			b.Images = []string{}
			for _, id := range v.([]any) {
				b.Images = append(b.Images, str(id))
			}
		}
		if v, ok := get(args, "require"); ok {
			b.Require = dn.Ptr(v.(bool))
		}
		if v, ok := get(args, "options"); ok {
			// Each setting as plain data, a $nest marker inside it built.
			if o, ok := v.(*object); ok {
				b.Options = make(map[string]any, len(o.keys))
				for _, k := range o.keys {
					b.Options[k] = plainArg(o.vals[k], r)
				}
			}
		}
		out = append(out, b)
	}
	return out
}

// boolArg is a boolean argument, nil when absent.
func boolArg(args *object, key string) *bool {
	if v, ok := args.vals[key].(bool); ok {
		return dn.Ptr(v)
	}
	return nil
}

func decodeQuestions(v any) dn.Questions {
	o, _ := v.(*object)
	qs := dn.Questions{}
	if o == nil {
		return qs
	}
	for _, k := range o.keys {
		qs = append(qs, dn.Q(k, decodeQuestion(o.vals[k])))
	}
	return qs
}

func decodeImage(root string, v any, r refs) (dn.Image, error) {
	args := v.(*object).vals["$image"].(*object)
	val := func(k string) any {
		x, ok := get(args, k)
		if !ok {
			return nil
		}
		if s, ok := special(x, r); ok {
			return s
		}
		return x
	}
	id := str(val("id"))
	mediaType := str(val("media_type"))
	_, padded := get(args, "pad_to")
	switch {
	case val("url") != nil:
		return dn.ImageURL(id, str(val("url"))), nil
	case val("upload_id") != nil:
		return dn.ImageUpload(id, str(val("upload_id")), mediaType), nil
	case val("path") != nil && !padded:
		return dn.ImageFile(id, filepath.Join(root, str(val("path"))))
	}
	var data []byte
	if p := val("path"); p != nil {
		b, err := os.ReadFile(filepath.Join(root, str(p)))
		if err != nil {
			return dn.Image{}, err
		}
		data = b
	} else {
		b, err := base64.StdEncoding.DecodeString(str(val("data_base64")))
		if err != nil {
			panic(err)
		}
		data = b
	}
	if n, ok := val("pad_to").(json.Number); ok {
		size, _ := n.Int64()
		if int64(len(data)) < size {
			data = append(data, make([]byte, int(size)-len(data))...)
		}
	}
	return dn.ImageData(id, data, mediaType), nil
}

func decodeImages(root string, v any, r refs) ([]dn.Image, error) {
	arr := listArg(v)
	var out []dn.Image
	for _, item := range arr {
		img, err := decodeImage(root, item, r)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

func decodeRequest(root string, args *object, r refs) (dn.Request, error) {
	images, err := decodeImages(root, args.vals["images"], r)
	if err != nil {
		return dn.Request{}, err
	}
	videos, err := decodeVideos(root, args.vals["videos"], r)
	var state any
	if v, ok := get(args, "state"); ok && v != nil {
		state = plainArg(v, r)
	}
	return dn.Request{
		State:     state,
		Questions: decodeQuestions(args.vals["questions"]),
		Images:    images,
		Videos:    videos,
		Model:     str(args.vals["model"]),
		Boosters:  decodeBoosters(args.vals["boosters"], r),
		Fallback:  boolArg(args, "fallback"),
	}, err
}

// decodeVideos reads a request's videos: {"$video": {id, frames, fps, start,
// end}} is a dn.Video, any other value a video in its wire form, passed as is.
func decodeVideos(root string, v any, r refs) ([]any, error) {
	arr := listArg(v)
	var out []any
	for _, item := range arr {
		o, ok := item.(*object)
		if !ok || len(o.keys) != 1 || o.keys[0] != "$video" {
			out = append(out, plainArg(item, r))
			continue
		}
		args := o.vals["$video"].(*object)
		video := dn.Video{ID: str(args.vals["id"])}
		for _, name := range []string{"fps", "start", "end"} {
			n, ok := args.vals[name].(json.Number)
			if !ok {
				continue
			}
			f, _ := n.Float64()
			switch name {
			case "fps":
				video.FPS = &f
			case "start":
				video.Start = &f
			default:
				video.End = &f
			}
		}
		frames, _ := args.vals["frames"].([]any)
		for _, fr := range frames {
			frame, err := decodeVideoFrame(root, fr)
			if err != nil {
				return nil, err
			}
			video.Frames = append(video.Frames, frame)
		}
		out = append(out, video)
	}
	return out, nil
}

// decodeVideoFrame reads {"$frame": {t, path | data_base64 | data_text,
// media_type}}, or a frame in its wire form, which is base64 text with its
// media type.
func decodeVideoFrame(root string, v any) (dn.VideoFrame, error) {
	o := v.(*object)
	args := o
	if len(o.keys) == 1 && o.keys[0] == "$frame" {
		args = o.vals["$frame"].(*object)
	} else {
		t, _ := o.vals["t"].(json.Number).Float64()
		return dn.VideoFrameBase64(t, str(o.vals["data"]), str(o.vals["media_type"])), nil
	}
	t, _ := args.vals["t"].(json.Number).Float64()
	mediaType := str(args.vals["media_type"])
	switch {
	case args.vals["data_text"] != nil:
		return dn.VideoFrameBase64(t, str(args.vals["data_text"]), mediaType), nil
	case args.vals["path"] != nil:
		b, err := os.ReadFile(filepath.Join(root, str(args.vals["path"])))
		if err != nil {
			return dn.VideoFrame{}, err
		}
		return dn.VideoFrameData(t, b, mediaType), nil
	}
	b, err := base64.StdEncoding.DecodeString(str(args.vals["data_base64"]))
	if err != nil {
		return dn.VideoFrame{}, err
	}
	return dn.VideoFrameData(t, b, mediaType), nil
}

// listArg returns a list argument, expanding {"$list": {count, item}} into
// count copies of item with {i} in its strings replaced by the index.
func listArg(v any) []any {
	if o, ok := v.(*object); ok && len(o.keys) == 1 && o.keys[0] == "$list" {
		args := o.vals["$list"].(*object)
		n, _ := args.vals["count"].(json.Number).Int64()
		out := make([]any, n)
		for i := range out {
			out[i] = withIndex(args.vals["item"], strconv.Itoa(i))
		}
		return out
	}
	arr, _ := v.([]any)
	return arr
}

func withIndex(v any, i string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "{i}", i)
	case *object:
		o := &object{keys: x.keys, vals: map[string]any{}}
		for k, val := range x.vals {
			o.vals[k] = withIndex(val, i)
		}
		return o
	case []any:
		out := make([]any, len(x))
		for j, e := range x {
			out[j] = withIndex(e, i)
		}
		return out
	}
	return v
}

func decodeBatchRequests(root string, v any, r refs) ([]dn.BatchRequest, error) {
	var out []dn.BatchRequest
	arr := listArg(v)
	for _, item := range arr {
		o := item.(*object)
		req, err := decodeRequest(root, o, r)
		if err != nil {
			return nil, err
		}
		br := dn.BatchRequest{Request: req}
		if v, ok := get(o, "custom_id"); ok && v != nil {
			br.CustomID = dn.Ptr(str(v))
		}
		out = append(out, br)
	}
	return out, nil
}

func decodeSessionRequest(args *object, r refs) dn.SessionRequest {
	sr := dn.SessionRequest{Questions: decodeQuestions(args.vals["questions"]), Model: str(args.vals["model"])}
	if v, ok := get(args, "state"); ok && v != nil {
		sr.State = plainArg(v, r)
	}
	if v, ok := get(args, "instructions"); ok && v != nil {
		sr.Instructions = plainArg(v, r)
	}
	if n, ok := args.vals["window"].(json.Number); ok {
		i, _ := n.Int64()
		sr.Window = dn.Ptr(int(i))
	}
	if n, ok := args.vals["ttl_seconds"].(json.Number); ok {
		i, _ := n.Int64()
		sr.TTLSeconds = dn.Ptr(int(i))
	}
	return sr
}

func decodeFrame(root string, args *object, r refs) (dn.Frame, error) {
	var f dn.Frame
	if v, ok := get(args, "frame"); ok && v != nil {
		f.Frame = plainArg(v, r)
	}
	if v, ok := get(args, "image"); ok && v != nil {
		img, err := decodeImage(root, v, r)
		if err != nil {
			return f, err
		}
		f.Image = &img
	}
	if v, ok := get(args, "questions"); ok && v != nil {
		f.Questions = []string{}
		for _, q := range v.([]any) {
			f.Questions = append(f.Questions, str(q))
		}
	}
	if v, ok := get(args, "reset"); ok && v != nil {
		f.Reset, _ = v.(bool)
	}
	if n, ok := args.vals["seq"].(json.Number); ok {
		i, _ := n.Int64()
		f.Seq = &i
	}
	return f, nil
}

// supervised is what a supervise call returns to the harness.
type supervised struct {
	replies []dn.SessionEvent
	closed  *dn.SessionClosed
}

// batchRun is what run_batch returns.
type batchRun struct {
	batch   *dn.Batch
	results []dn.BatchResult
}

func durationArg(args *object, key string) time.Duration {
	n, ok := args.vals[key].(json.Number)
	if !ok {
		return 0
	}
	f, _ := n.Float64()
	return time.Duration(f * float64(time.Second))
}

func invoke(ctx context.Context, root string, c *dn.Client, call *object, r refs) (any, error) {
	method := str(call.vals["method"])
	args, _ := call.vals["args"].(*object)
	if args == nil {
		args = &object{vals: map[string]any{}}
	}
	// A file image is read when it is made, so an unreadable one fails here,
	// as an argument error, before the call.
	images, err := decodeImages(root, args.vals["images"], r)
	if err != nil {
		return nil, err
	}
	videos, err := decodeVideos(root, args.vals["videos"], r)
	if err != nil {
		return nil, err
	}
	if check, _ := call.vals["check"].(bool); check {
		// The same request handed to the SDK's check instead of being sent.
		return nil, runCheck(root, c, method, args, r)
	}
	switch method {
	case "decide":
		req, err := decodeRequest(root, args, r)
		if err != nil {
			return nil, err
		}
		return c.Decide(ctx, req)
	case "decide_many":
		var reqs []dn.Request
		for _, item := range listArg(args.vals["requests"]) {
			req, err := decodeRequest(root, item.(*object), r)
			if err != nil {
				return nil, err
			}
			reqs = append(reqs, req)
		}
		conc := dn.DefaultConcurrency
		if n, ok := args.vals["concurrency"].(json.Number); ok {
			i, _ := n.Int64()
			conc = int(i)
		}
		return c.DecideMany(ctx, reqs, conc)
	case "check":
		return c.Check(ctx, dn.CheckRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			IfTrue:       argValue(args, "true"),
			IfFalse:      argValue(args, "false"),
			Images:       images,
			Videos:       videos,
			Model:        str(args.vals["model"]),
			Boosters:     decodeBoosters(args.vals["boosters"], r),
			Fallback:     boolArg(args, "fallback"),
		})
	case "classify":
		return c.Classify(ctx, dn.ClassifyRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			Options:      decodeOptions(args.vals["options"]),
			Images:       images,
			Videos:       videos,
			Model:        str(args.vals["model"]),
			Boosters:     decodeBoosters(args.vals["boosters"], r),
			Fallback:     boolArg(args, "fallback"),
		})
	case "score":
		return c.Score(ctx, dn.ScoreRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			Levels:       argValue(args, "levels"),
			Images:       images,
			Videos:       videos,
			Model:        str(args.vals["model"]),
			Boosters:     decodeBoosters(args.vals["boosters"], r),
			Fallback:     boolArg(args, "fallback"),
		})
	case "number":
		return c.Number(ctx, dn.NumberRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			Min:          argValue(args, "min"),
			Max:          argValue(args, "max"),
			Step:         argValue(args, "step"),
			Images:       images,
			Videos:       videos,
			Model:        str(args.vals["model"]),
			Boosters:     decodeBoosters(args.vals["boosters"], r),
			Fallback:     boolArg(args, "fallback"),
		})
	case "gate":
		return c.Gate(ctx, dn.GateRequest{
			Action:   argValue(args, "action"),
			Context:  argValue(args, "context"),
			Images:   images,
			Videos:   videos,
			Model:    str(args.vals["model"]),
			Boosters: decodeBoosters(args.vals["boosters"], r),
			Fallback: boolArg(args, "fallback"),
		})
	case "models":
		return c.Models(ctx)
	case "decide_raw":
		return c.DecideRaw(ctx, raw(args.vals["body"]))
	case "upload":
		ups, err := c.Upload(ctx, images)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(ups))
		for i, u := range ups {
			ids[i] = u.ID
		}
		return ids, nil
	case "open_session":
		return c.OpenSession(ctx, decodeSessionRequest(args, r))
	case "supervise":
		body := decodeSessionRequest(args.vals["body"].(*object), r)
		var readings []dn.Frame
		for _, item := range args.vals["feed"].([]any) {
			f, err := decodeFrame(root, item.(*object), r)
			if err != nil {
				return nil, err
			}
			readings = append(readings, f)
		}
		interval := durationArg(args, "feed_interval_ms") / 1000
		feed := make(chan dn.Frame)
		go func() {
			defer close(feed)
			for i, f := range readings {
				if i > 0 {
					time.Sleep(interval)
				}
				feed <- f
			}
		}()
		out := &supervised{}
		closed, err := c.Supervise(ctx, body, feed, func(ev dn.SessionEvent) { out.replies = append(out.replies, ev) })
		if err != nil {
			return nil, err
		}
		out.closed = closed
		return out, nil
	case "run_batch":
		reqs, err := decodeBatchRequests(root, args.vals["requests"], r)
		if err != nil {
			return nil, err
		}
		batch, results, err := c.RunBatch(ctx, reqs, dn.RunBatchOptions{PollInterval: durationArg(args, "poll_interval"), Timeout: durationArg(args, "timeout")})
		if err != nil {
			return nil, err
		}
		return &batchRun{batch: batch, results: results}, nil
	case "batches.create", "batches.add":
		reqs, err := decodeBatchRequests(root, args.vals["requests"], r)
		if err != nil {
			return nil, err
		}
		if method == "batches.create" {
			return c.Batches().Create(ctx, reqs)
		}
		return c.Batches().Add(ctx, str(plainArg(args.vals["batch_id"], r)), reqs)
	case "batches.finalize", "batches.get", "batches.results", "batches.cancel":
		id := str(plainArg(args.vals["batch_id"], r))
		switch method {
		case "batches.finalize":
			return c.Batches().Finalize(ctx, id)
		case "batches.get":
			return c.Batches().Get(ctx, id)
		case "batches.results":
			return c.Batches().Results(ctx, id)
		}
		return c.Batches().Cancel(ctx, id)
	case "collections.create":
		return c.Collections().Create(ctx, str(plainArg(args.vals["name"], r)), str(plainArg(args.vals["type"], r)))
	case "collections.list":
		return c.Collections().List(ctx)
	case "collections.get":
		return c.Collections().Get(ctx, str(plainArg(args.vals["collection_id"], r)))
	case "collections.delete":
		return c.Collections().Delete(ctx, str(plainArg(args.vals["collection_id"], r)))
	case "collections.enroll":
		e := dn.Enrollment{Reference: str(plainArg(args.vals["reference"], r))}
		if v, ok := get(args, "image"); ok && v != nil {
			img, err := decodeImage(root, v, r)
			if err != nil {
				return nil, err
			}
			e.Image = img
		}
		if v, ok := get(args, "label"); ok && v != nil {
			e.Label = dn.Ptr(str(plainArg(v, r)))
		}
		if v, ok := get(args, "metadata"); ok && v != nil {
			e.Metadata = metadataArg(v)
		}
		return c.Collections().Enroll(ctx, str(plainArg(args.vals["collection_id"], r)), e)
	case "collections.update_item":
		var u dn.ItemUpdate
		if v, ok := get(args, "reference"); ok {
			u.Reference = dn.Ptr(str(plainArg(v, r)))
		}
		if v, ok := get(args, "label"); ok {
			if v == nil {
				u.ClearLabel = true
			} else {
				u.Label = dn.Ptr(str(plainArg(v, r)))
			}
		}
		if v, ok := get(args, "metadata"); ok {
			u.Metadata = metadataArg(v)
		}
		return c.Collections().UpdateItem(ctx, str(plainArg(args.vals["collection_id"], r)), str(plainArg(args.vals["item_id"], r)), u)
	case "collections.list_items":
		var opts dn.ItemListOptions
		if n, ok := args.vals["offset"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Offset = dn.Ptr(int(i))
		}
		if n, ok := args.vals["limit"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Limit = dn.Ptr(int(i))
		}
		if v, ok := get(args, "reference"); ok && v != nil {
			opts.Reference = dn.Ptr(str(plainArg(v, r)))
		}
		if v, ok := get(args, "reference_prefix"); ok && v != nil {
			opts.ReferencePrefix = dn.Ptr(str(plainArg(v, r)))
		}
		return c.Collections().ListItems(ctx, str(plainArg(args.vals["collection_id"], r)), opts)
	case "collections.delete_item":
		return c.Collections().DeleteItem(ctx, str(plainArg(args.vals["collection_id"], r)), str(plainArg(args.vals["item_id"], r)))
	case "usage.key":
		return c.Usage().Key(ctx, str(plainArg(args.vals["key_id"], r)), dn.UsageOptions{Start: str(args.vals["start"]), End: str(args.vals["end"]),
			Granularity: str(args.vals["granularity"]), GroupBy: str(args.vals["group_by"])})
	case "usage.list", "usage.all":
		opts := dn.UsageListOptions{Start: str(args.vals["start"]), End: str(args.vals["end"]), Granularity: str(args.vals["granularity"]),
			GroupBy: str(args.vals["group_by"]), DeviceID: str(args.vals["device_id"]), Scope: str(args.vals["scope"]), Status: str(args.vals["status"]),
			Cursor: str(plainArg(args.vals["cursor"], r))}
		if l, ok := args.vals["labels"].(*object); ok {
			opts.Labels = map[string]string{}
			for _, k := range l.keys {
				opts.Labels[k] = str(l.vals[k])
			}
		}
		if ids, ok := args.vals["key_ids"].([]any); ok {
			for _, id := range ids {
				opts.KeyIDs = append(opts.KeyIDs, str(id))
			}
		}
		if b, ok := args.vals["include_empty"].(bool); ok {
			opts.IncludeEmpty = &b
		}
		if n, ok := args.vals["limit"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Limit = dn.Ptr(int(i))
		}
		if method == "usage.all" {
			return c.Usage().All(ctx, opts)
		}
		return c.Usage().List(ctx, opts)
	case "keys.create":
		k := dn.KeyCreate{Name: str(plainArg(args.vals["name"], r)), DeviceID: str(args.vals["device_id"]), Labels: labelsArg(args.vals["labels"]),
			Limits: limitsArg(args.vals["limits"]), DailySpendCapUSD: capArg(args.vals["daily_spend_cap"]), ExpiresAt: str(args.vals["expires_at"])}
		if b, ok := args.vals["may_search_collections"].(bool); ok {
			k.MaySearchCollections = &b
		}
		if v, ok := args.vals["idempotency_key"].(string); ok {
			k.IdempotencyKey = &v
		}
		return c.Keys().Create(ctx, k)
	case "keys.list", "keys.all":
		opts := dn.KeyListOptions{Labels: labelsArg(args.vals["labels"]), DeviceID: str(args.vals["device_id"]), Status: str(args.vals["status"]),
			Cursor: str(plainArg(args.vals["cursor"], r))}
		if n, ok := args.vals["limit"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Limit = dn.Ptr(int(i))
		}
		if method == "keys.all" {
			return c.Keys().All(ctx, opts)
		}
		return c.Keys().List(ctx, opts)
	case "keys.update":
		var u dn.KeyUpdate
		if v, ok := get(args, "name"); ok {
			u.Name = dn.Ptr(str(plainArg(v, r)))
		}
		if v, ok := get(args, "labels"); ok {
			u.Labels, u.ClearLabels = labelsArg(v), v == nil
		}
		if v, ok := get(args, "limits"); ok {
			u.Limits, u.ClearLimits = limitsArg(v), v == nil
		}
		if v, ok := get(args, "daily_spend_cap"); ok {
			u.DailySpendCapUSD, u.ClearDailySpendCap = capArg(v), v == nil
		}
		if v, ok := get(args, "expires_at"); ok {
			if v == nil {
				u.ClearExpiresAt = true
			} else {
				u.ExpiresAt = dn.Ptr(str(v))
			}
		}
		if b, ok := args.vals["may_search_collections"].(bool); ok {
			u.MaySearchCollections = &b
		}
		return c.Keys().Update(ctx, str(plainArg(args.vals["key_id"], r)), u)
	case "keys.revoke":
		return c.Keys().Revoke(ctx, str(plainArg(args.vals["key_id"], r)))
	case "keys.revoke_by_labels":
		kr := dn.KeyRevoke{Labels: labelsArg(args.vals["labels"])}
		if kr.Labels == nil {
			kr.Labels = map[string]string{}
		}
		if ids, ok := args.vals["device_ids"].([]any); ok {
			kr.DeviceIDs = []string{}
			for _, id := range ids {
				kr.DeviceIDs = append(kr.DeviceIDs, str(id))
			}
		}
		kr.DryRun, _ = args.vals["dry_run"].(bool)
		return c.Keys().RevokeByLabels(ctx, kr)
	case "batches.list":
		var opts dn.BatchListOptions
		if n, ok := args.vals["offset"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Offset = dn.Ptr(int(i))
		}
		if n, ok := args.vals["limit"].(json.Number); ok {
			i, _ := n.Int64()
			opts.Limit = dn.Ptr(int(i))
		}
		return c.Batches().List(ctx, opts)
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

func runCheck(root string, c *dn.Client, method string, args *object, r refs) error {
	switch method {
	case "decide":
		req, err := decodeRequest(root, args, r)
		if err != nil {
			return err
		}
		return c.CheckDecide(req)
	case "open_session":
		return c.CheckSession(decodeSessionRequest(args, r))
	case "batches.create", "batches.add":
		reqs, err := decodeBatchRequests(root, args.vals["requests"], r)
		if err != nil {
			return err
		}
		if method == "batches.create" {
			return c.CheckBatchCreate(reqs)
		}
		return c.CheckBatchAdd(reqs)
	}
	return fmt.Errorf("no check for %q", method)
}

func makeClient(t *testing.T, c *object, mockURL string, extra map[string]string, more ...dn.Option) *dn.Client {
	mockPort := ""
	if u, err := url.Parse(mockURL); err == nil {
		mockPort = u.Port()
	}
	fill := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "{mock_url}", mockURL), "{mock_port}", mockPort)
	}
	for _, v := range envVars {
		t.Setenv(v, "")
		_ = os.Unsetenv(v)
	}
	if env, ok := get(c, "env"); ok && env != nil {
		e := env.(*object)
		for _, k := range e.keys {
			t.Setenv(k, fill(str(e.vals[k])))
		}
	}
	spec, _ := c.vals["client"].(*object)
	var opts []dn.Option
	if v, ok := get(spec, "api_key"); ok && v != nil {
		opts = append(opts, dn.WithAPIKey(str(v)))
	}
	if v, ok := get(spec, "model"); ok && v != nil {
		opts = append(opts, dn.WithModel(str(v)))
	}
	if v, ok := get(spec, "timeout"); ok && v != nil {
		f, _ := v.(json.Number).Float64()
		opts = append(opts, dn.WithTimeout(time.Duration(f*float64(time.Second))))
	}
	if v, ok := get(spec, "max_retries"); ok && v != nil {
		n, _ := v.(json.Number).Int64()
		opts = append(opts, dn.WithMaxRetries(int(n)))
	}
	if v, ok := get(spec, "upload_timeout"); ok && v != nil {
		f, _ := v.(json.Number).Float64()
		opts = append(opts, dn.WithUploadTimeout(time.Duration(f*float64(time.Second))))
	}
	if v, ok := get(spec, "inline_threshold"); ok && v != nil {
		n, _ := v.(json.Number).Int64()
		opts = append(opts, dn.WithInlineThreshold(int(n)))
	}
	if v, ok := get(spec, "base_url"); ok {
		if v != nil {
			opts = append(opts, dn.WithBaseURL(fill(str(v))))
		}
	} else {
		opts = append(opts, dn.WithBaseURL(mockURL))
	}
	if v, ok := get(spec, "http_client"); ok && str(v) == "caller_following_redirects" {
		// A zero http.Client follows redirects by default.
		opts = append(opts, dn.WithHTTPClient(&http.Client{}))
	}
	headers := map[string]string{}
	if v, ok := get(spec, "headers"); ok && v != nil {
		h := v.(*object)
		for _, k := range h.keys {
			headers[k] = str(h.vals[k])
		}
	}
	for k, v := range extra {
		headers[k] = v
	}
	opts = append(opts, dn.WithHeaders(headers))
	if os.Getenv("DECISIONNODE_TEST_DEBUG") != "" {
		opts = append(opts, dn.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	}
	return dn.NewClient(append(opts, more...)...)
}

func session() string {
	return fmt.Sprintf("go-%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------- requests

func runRequest(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	extra := map[string]string{"x-mock-session": sid}
	if v, ok := get(c, "mock"); ok && v != nil {
		extra["x-mock-scenario"] = "request/" + str(c.vals["id"])
	}
	client := makeClient(t, c, m.url, extra)
	if _, err := invoke(context.Background(), root, client, c.vals["call"].(*object), refs{}); err != nil {
		return fmt.Errorf("the call failed: %w", err)
	}
	time.Sleep(20 * time.Millisecond)
	log := m.requests(t, sid).Requests
	exp := c.vals["expect"].(*object)
	if one, ok := get(exp, "request"); ok {
		if len(log) != 1 {
			return fmt.Errorf("expected 1 request, the mock received %d: %s", len(log), describe(log))
		}
		return checkRequest(one.(*object), log[0], "request", false)
	}
	return checkRequestList(exp.vals["requests"].([]any), log)
}

func describe(log []mockRecord) string {
	var parts []string
	for _, r := range log {
		parts = append(parts, r.Method+" "+r.Path)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func checkRequestList(expected []any, log []mockRecord) error {
	if len(log) != len(expected) {
		return fmt.Errorf("expected %d requests, the mock received %d: %s", len(expected), len(log), describe(log))
	}
	parallel := func(i int) bool { p, _ := get(expected[i].(*object), "parallel"); b, _ := p.(bool); return b }
	for i := 0; i < len(expected); {
		if !parallel(i) {
			if err := checkRequest(expected[i].(*object), log[i], fmt.Sprintf("requests[%d]", i), false); err != nil {
				return err
			}
			i++
			continue
		}
		j := i
		for j < len(expected) && parallel(j) {
			j++
		}
		// Requests sent in parallel may arrive in any order: one assignment
		// of records to expectations must hold.
		used := make([]bool, j-i)
		var assign func(k int) error
		assign = func(k int) error {
			if k == j {
				return nil
			}
			var first error
			for g := i; g < j; g++ {
				if used[g-i] {
					continue
				}
				if err := checkRequest(expected[k].(*object), log[g], fmt.Sprintf("requests[%d]", k), false); err != nil {
					if first == nil {
						first = err
					}
					continue
				}
				used[g-i] = true
				if err := assign(k + 1); err == nil {
					return nil
				} else if first == nil {
					first = err
				}
				used[g-i] = false
			}
			return first
		}
		if err := assign(i); err != nil {
			return fmt.Errorf("no order of the parallel requests matches: %w", err)
		}
		i = j
	}
	return nil
}

func parseJSONL(text string) ([]any, error) {
	s := strings.TrimSpace(text)
	if s == "" {
		return []any{}, nil
	}
	if strings.HasPrefix(s, "[") {
		v, err := parseOrdered([]byte(s))
		if err != nil {
			return nil, err
		}
		return v.([]any), nil
	}
	var out []any
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := parseOrdered([]byte(line))
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// checkRequest compares one recorded request with an expectation. anyBody
// allows a body the expectation does not describe (a request sequence).
func checkRequest(exp *object, got mockRecord, where string, anyBody bool) error {
	if m := str(exp.vals["method"]); got.Method != m {
		return fmt.Errorf("%s method: expected %s, got %s %s", where, m, got.Method, got.Path)
	}
	if p, ok := get(exp, "path"); ok && got.Path != str(p) {
		return fmt.Errorf("%s path: expected %s, got %s", where, str(p), got.Path)
	}
	if p, ok := get(exp, "path_pattern"); ok && !regexp.MustCompile(str(p)).MatchString(got.Path) {
		return fmt.Errorf("%s path: %s does not match %s", where, got.Path, str(p))
	}
	if q, ok := get(exp, "query"); ok {
		parsed, _ := url.ParseQuery(got.Query)
		have := map[string]any{}
		for k, v := range parsed {
			have[k] = v[0]
		}
		if want := plain(q); !same(want, have) {
			return fmt.Errorf("%s query: expected %s, got %s", where, short(want), short(have))
		}
	}
	if q, ok := get(exp, "query_all"); ok {
		// Suite 1.18: every parameter with all its values, a repeated one in
		// its order.
		parsed, _ := url.ParseQuery(got.Query)
		have := map[string]any{}
		for k, v := range parsed {
			vals := make([]any, len(v))
			for i, x := range v {
				vals[i] = x
			}
			have[k] = vals
		}
		if want := plain(q); !same(want, have) {
			return fmt.Errorf("%s query_all: expected %s, got %s", where, short(want), short(have))
		}
	}
	if st, ok := get(exp, "status"); ok {
		want, _ := st.(json.Number).Int64()
		if got.Status == nil || int64(*got.Status) != want {
			return fmt.Errorf("%s status: the mock answered %v, expected %d", where, got.Status, want)
		}
	}
	if h, ok := get(exp, "headers"); ok {
		for _, k := range h.(*object).keys {
			want := str(h.(*object).vals[k])
			if have, ok := got.Headers[strings.ToLower(k)]; !ok || have != want {
				return fmt.Errorf("%s header %s: expected %q, got %q", where, k, want, have)
			}
		}
	}
	if h, ok := get(exp, "header_patterns"); ok {
		for _, k := range h.(*object).keys {
			re := regexp.MustCompile(str(h.(*object).vals[k]))
			have, ok := got.Headers[strings.ToLower(k)]
			if !ok || !re.MatchString(have) {
				return fmt.Errorf("%s header %s: %q does not match %s", where, k, have, re)
			}
		}
	}
	if a, ok := get(exp, "absent_headers"); ok {
		for _, n := range a.([]any) {
			if _, present := got.Headers[strings.ToLower(str(n))]; present {
				return fmt.Errorf("%s header %s must not be sent", where, str(n))
			}
		}
	}
	if b, ok := get(exp, "body_base64"); ok {
		if got.BodyBase64 != str(b) {
			return fmt.Errorf("%s body: the bytes differ from the expected ones", where)
		}
		return nil
	}
	if b, ok := get(exp, "body_sha256"); ok {
		data, _ := base64.StdEncoding.DecodeString(got.BodyBase64)
		if sum := fmt.Sprintf("%x", sha256.Sum256(data)); sum != str(b) {
			return fmt.Errorf("%s body: sha256 %s, expected %s", where, sum, str(b))
		}
		return nil
	}
	if want, ok := get(exp, "jsonl"); ok {
		lines, err := parseJSONL(got.BodyText)
		if err != nil {
			return fmt.Errorf("%s body: %w", where, err)
		}
		if !same(plain(want), plain(lines)) {
			return fmt.Errorf("%s body lines: expected %s, got %s", where, short(plain(want)), short(plain(lines)))
		}
		return nil
	}
	count, hasCount := get(exp, "jsonl_count")
	lineChecks, hasLineChecks := get(exp, "jsonl_checks")
	if hasCount || hasLineChecks {
		lines, err := parseJSONL(got.BodyText)
		if err != nil {
			return fmt.Errorf("%s body: %w", where, err)
		}
		if hasCount {
			if n, _ := count.(json.Number).Int64(); int64(len(lines)) != n {
				return fmt.Errorf("%s body: expected %d lines, got %d", where, n, len(lines))
			}
		}
		if hasLineChecks {
			for _, item := range lineChecks.([]any) {
				chk := item.(*object)
				n, _ := chk.vals["line"].(json.Number).Int64()
				if int(n) >= len(lines) {
					return fmt.Errorf("%s body: no line %d", where, n)
				}
				if err := checkValue(chk, lines[n], fmt.Sprintf("%s line %d", where, n)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	expBody, hasBody := get(exp, "body")
	checks, hasChecks := get(exp, "body_checks")
	_, hasOrder := get(exp, "ordered_keys")
	if anyBody && !hasBody && !hasChecks && !hasOrder {
		return nil
	}
	var body any
	if got.BodyText != "" {
		var err error
		if body, err = parseOrdered([]byte(got.BodyText)); err != nil {
			return fmt.Errorf("%s: the body is not JSON: %w", where, err)
		}
	}
	if hasBody {
		if !same(plain(expBody), plain(body)) {
			return fmt.Errorf("%s body: expected %s, got %s", where, short(plain(expBody)), short(plain(body)))
		}
	} else if !hasChecks && got.BodyText != "" && !anyBody {
		return fmt.Errorf("%s: expected no body, got %s", where, short(got.BodyText))
	}
	if hasChecks {
		for _, item := range checks.([]any) {
			if err := checkValue(item.(*object), body, where); err != nil {
				return err
			}
		}
	}
	if ok, present := get(exp, "ordered_keys"); present {
		for _, p := range ok.([]any) {
			want, _ := pointer(expBody, str(p))
			have, _ := pointer(body, str(p))
			wo, _ := want.(*object)
			ho, _ := have.(*object)
			if wo == nil || ho == nil || strings.Join(wo.keys, "\x00") != strings.Join(ho.keys, "\x00") {
				var hk []string
				if ho != nil {
					hk = ho.keys
				}
				return fmt.Errorf("%s body%s: key order expected %q, got %q", where, str(p), wo.keys, hk)
			}
		}
	}
	return nil
}

func checkValue(chk *object, body any, where string) error {
	ptr := str(chk.vals["pointer"])
	val, found := pointer(body, ptr)
	if absent, _ := chk.vals["absent"].(bool); absent {
		if found {
			return fmt.Errorf("%s body%s: must be absent, got %s", where, ptr, short(plain(val)))
		}
		return nil
	}
	if !found {
		return fmt.Errorf("%s body%s: missing", where, ptr)
	}
	if eq, ok := get(chk, "equals"); ok && !same(plain(eq), plain(val)) {
		return fmt.Errorf("%s body%s: expected %s, got %s", where, ptr, short(plain(eq)), short(plain(val)))
	}
	if typ, ok := get(chk, "type"); ok && !jsonType(str(typ), plain(val)) {
		return fmt.Errorf("%s body%s: expected a %s", where, ptr, str(typ))
	}
	if pat, ok := get(chk, "pattern"); ok {
		s, isStr := val.(string)
		if !isStr || !regexp.MustCompile(str(pat)).MatchString(s) {
			return fmt.Errorf("%s body%s: %s does not match %s", where, ptr, short(plain(val)), str(pat))
		}
	}
	if keys, ok := get(chk, "keys"); ok {
		o, isObj := val.(*object)
		if !isObj || !same(plain(keys), plain(stringsToAny(o.keys))) {
			return fmt.Errorf("%s body%s: keys expected %s", where, ptr, short(plain(keys)))
		}
	}
	return nil
}

func stringsToAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func jsonType(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// ---------------------------------------------------------------- normalised values

func nilIfZero(s string) any { return nilIfEmpty(s) }

func normBatch(b *dn.Batch) any {
	var errVal any
	if b.Error != nil {
		errVal = map[string]any{"error_type": b.Error.ErrorType, "message": b.Error.Message}
	}
	return canon(map[string]any{
		"id":    b.ID,
		"state": b.State,
		"counters": map[string]any{
			"requests": b.Counters.Requests, "queued": b.Counters.Queued, "in_progress": b.Counters.InProgress,
			"completed": b.Counters.Completed, "failed": b.Counters.Failed, "expired": b.Counters.Expired, "cancelled": b.Counters.Cancelled,
		},
		"created_at":        nilIfZero(b.CreatedAt),
		"finalized_at":      nilIfZero(b.FinalizedAt),
		"expires_at":        nilIfZero(b.ExpiresAt),
		"ended_at":          nilIfZero(b.EndedAt),
		"results_url":       nilIfZero(b.ResultsURL),
		"results_expire_at": nilIfZero(b.ResultsExpireAt),
		"usage":             map[string]any{"input_tokens": intOrNil(b.Usage.InputTokens), "output_tokens": intOrNil(b.Usage.OutputTokens)},
		"error":             errVal,
	})
}

// checkUsageResult compares a usage read's result, the object as sent;
// usage_items is the list, compared entry by entry.
func checkUsageResult(kind string, want, result any, path string) error {
	if want == nil {
		// Only the kind is expected.
		want = map[string]any{}
		if kind == "usage_items" {
			if _, ok := result.([]dn.UsageItem); !ok {
				return fmt.Errorf("%s: expected usage items, got %T", path, result)
			}
			return nil
		}
	}
	switch kind {
	case "key_usage":
		u, ok := result.(*dn.KeyUsage)
		if !ok {
			return fmt.Errorf("%s: expected a key's usage, got %T", path, result)
		}
		return match(want, canon(u.Raw), path)
	case "usage_page":
		p, ok := result.(*dn.UsagePage)
		if !ok {
			return fmt.Errorf("%s: expected a usage page, got %T", path, result)
		}
		return match(want, canon(p.Raw), path)
	}
	items, ok := result.([]dn.UsageItem)
	if !ok {
		return fmt.Errorf("%s: expected usage items, got %T", path, result)
	}
	wants, _ := want.([]any)
	if len(wants) != len(items) {
		return fmt.Errorf("%s: expected %d items, got %d", path, len(wants), len(items))
	}
	for i := range items {
		if err := match(wants[i], canon(items[i].Raw), fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

// checkKeysResult compares a /v1/keys call's result, the object as sent;
// api_keys is the list, compared entry by entry.
func checkKeysResult(kind string, want, result any, path string) error {
	var got any
	switch x := result.(type) {
	case *dn.CreatedKey:
		got = x.Raw
	case *dn.Key:
		got = x.Raw
	case *dn.KeyPage:
		got = x.Raw
	case *dn.RevokedKey:
		got = x.Raw
	case *dn.KeysRevoked:
		got = x.Raw
	case []dn.Key:
		list := []any{}
		for _, k := range x {
			list = append(list, k.Raw)
		}
		wants, _ := want.([]any)
		if want != nil && len(wants) != len(list) {
			return fmt.Errorf("%s: expected %d keys, got %d", path, len(wants), len(list))
		}
		for i := range wants {
			if err := match(wants[i], canon(list[i]), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s: expected a %s, got %T", path, kind, result)
	}
	if want == nil {
		return nil
	}
	return match(want, canon(got), path)
}

func labelsArg(v any) map[string]string {
	o, ok := v.(*object)
	if !ok {
		return nil
	}
	m := map[string]string{}
	for _, k := range o.keys {
		m[k] = str(o.vals[k])
	}
	return m
}

func limitsArg(v any) *dn.KeyLimits {
	o, ok := v.(*object)
	if !ok {
		return nil
	}
	l := &dn.KeyLimits{}
	if n, ok := o.vals["requests_per_second"].(json.Number); ok {
		i, _ := n.Int64()
		l.RequestsPerSecond = dn.Ptr(int(i))
	}
	if n, ok := o.vals["tokens_per_second"].(json.Number); ok {
		i, _ := n.Int64()
		l.TokensPerSecond = dn.Ptr(int(i))
	}
	return l
}

func capArg(v any) *float64 {
	o, ok := v.(*object)
	if !ok {
		return nil
	}
	n, ok := o.vals["usd"].(json.Number)
	if !ok {
		return nil
	}
	f, _ := n.Float64()
	return &f
}

// collectionKinds are the expectations of the collections calls' results.
var collectionKinds = []string{"collection", "collections", "item", "item_page", "deleted"}

// checkCollectionResult compares a collections call's result, by its
// fields' wire names; collections is the list, compared entry by entry.
func checkCollectionResult(kind string, want, result any, path string) error {
	var got any
	switch kind {
	case "collection":
		c, ok := result.(*dn.Collection)
		if !ok {
			return fmt.Errorf("%s: expected a collection, got %T", path, result)
		}
		got = normCollection(c)
	case "collections":
		list, ok := result.([]dn.Collection)
		if !ok {
			return fmt.Errorf("%s: expected a list of collections, got %T", path, result)
		}
		wants, _ := want.([]any)
		if len(wants) != len(list) {
			return fmt.Errorf("%s: expected %d collections, got %d", path, len(wants), len(list))
		}
		for i := range list {
			if err := match(wants[i], normCollection(&list[i]), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case "item":
		it, ok := result.(*dn.CollectionItem)
		if !ok {
			return fmt.Errorf("%s: expected an item, got %T", path, result)
		}
		got = normItem(it)
	case "item_page":
		p, ok := result.(*dn.CollectionItemPage)
		if !ok {
			return fmt.Errorf("%s: expected a page of items, got %T", path, result)
		}
		items := []any{}
		for i := range p.Items {
			items = append(items, normItem(&p.Items[i]))
		}
		var next any
		if p.NextOffset != nil {
			next = *p.NextOffset
		}
		got = canon(map[string]any{"items": items, "offset": p.Offset, "limit": p.Limit, "total": p.Total, "next_offset": next,
			"total_capped": p.TotalCapped, "reference": nilIfEmpty(p.Reference), "reference_prefix": nilIfEmpty(p.ReferencePrefix)})
	case "deleted":
		d, ok := result.(*dn.Deleted)
		if !ok {
			return fmt.Errorf("%s: expected a delete, got %T", path, result)
		}
		got = canon(map[string]any{"id": d.ID, "deleted": d.Deleted})
	}
	w, _ := want.(map[string]any)
	return match(orEmpty(w), got, path)
}

func normCollection(c *dn.Collection) any {
	var deletion any
	if c.Deletion != nil {
		deletion = map[string]any{"at": c.Deletion.At, "reason": c.Deletion.Reason}
	}
	return canon(map[string]any{"id": c.ID, "type": c.Type, "name": c.Name, "public": c.Public, "items": c.Items,
		"created_at": nilIfEmpty(c.CreatedAt), "deletion": deletion})
}

func normItem(it *dn.CollectionItem) any {
	m := map[string]any{"id": it.ID, "reference": it.Reference, "label": nilIfEmpty(it.Label), "created_at": nilIfEmpty(it.CreatedAt)}
	if it.Collection != "" {
		m["collection"] = it.Collection
	}
	if _, ok := it.Raw["faces_found"]; ok {
		m["faces_found"] = it.FacesFound
	}
	if it.Box != nil {
		m["box"] = normBox(*it.Box)
	}
	if _, ok := it.Raw["metadata"]; ok {
		m["metadata"] = it.Metadata
	}
	if it.Usage != nil {
		u := map[string]any{"enrollments": intOrNil(it.Usage.Enrollments)}
		if it.Usage.Cost != nil {
			u["cost"] = map[string]any{"usd": it.Usage.Cost.USD}
		}
		m["usage"] = u
	}
	return canon(m)
}

// metadataArg is a metadata argument as the SDK takes it: an object of
// keys and values, numbers kept as written.
func metadataArg(v any) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw(v)))
	dec.UseNumber()
	m := map[string]any{}
	_ = dec.Decode(&m)
	return m
}

func normBatchPage(p *dn.BatchPage) any {
	batches := []any{}
	for i := range p.Batches {
		batches = append(batches, normBatch(&p.Batches[i]))
	}
	var next any
	if p.NextOffset != nil {
		next = *p.NextOffset
	}
	return canon(map[string]any{"batches": batches, "offset": p.Offset, "limit": p.Limit, "total": p.Total, "next_offset": next})
}

func checkBatchResults(expected []any, results []dn.BatchResult, path string) error {
	if len(results) != len(expected) {
		return fmt.Errorf("%s: expected %d results, got %d", path, len(expected), len(results))
	}
	for i, e := range expected {
		spec := e.(map[string]any)
		r := results[i]
		where := fmt.Sprintf("%s[%d]", path, i)
		var status any = r.Status
		if r.StatusText != "" {
			status = r.StatusText
		}
		got := canon(map[string]any{"custom_id": r.CustomID, "status": status, "headers": r.Headers}).(map[string]any)
		for _, key := range []string{"custom_id", "status"} {
			if want, ok := spec[key]; ok && !same(want, got[key]) {
				return fmt.Errorf("%s.%s: expected %s, got %s", where, key, short(want), short(got[key]))
			}
		}
		if want, ok := spec["headers"]; ok {
			if err := match(want, got["headers"], where+".headers"); err != nil {
				return err
			}
		}
		if want, ok := spec["decision"]; ok {
			if r.Decision == nil {
				return fmt.Errorf("%s: expected a decision, got none", where)
			}
			if err := match(want, normDecision(r.Decision), where+".decision"); err != nil {
				return err
			}
		}
		if want, ok := spec["error"]; ok {
			if r.Err == nil {
				return fmt.Errorf("%s: expected an error value, got none", where)
			}
			if err := checkError(want.(map[string]any), r.Err, where+".error"); err != nil {
				return err
			}
		}
		if no, _ := spec["no_decision"].(bool); no && r.Decision != nil {
			return fmt.Errorf("%s: expected no decision", where)
		}
	}
	return nil
}

func normSession(s *dn.Session) any {
	out := canon(s.Raw).(map[string]any)
	answers := map[string]any{}
	for k, v := range s.Answers {
		answers[k] = canon(normAnswer(v))
	}
	out["answers"] = answers
	return out
}

func normEvent(ev dn.SessionEvent) any {
	switch e := ev.(type) {
	case *dn.Reply:
		answers := map[string]any{}
		for k, v := range e.Answers {
			answers[k] = normAnswer(v)
		}
		var latency any
		if e.LatencyMs != nil {
			latency = *e.LatencyMs
		}
		out := map[string]any{"seq": e.Seq, "answers": answers, "usage": e.Raw["usage"], "latency_ms": latency, "safety": e.Raw["safety"]}
		if e.Dropped != nil {
			out["dropped"] = e.Dropped
		}
		return canon(map[string]any{"reply": out})
	case *dn.FrameError:
		var seq any
		if e.Seq != nil {
			seq = *e.Seq
		}
		out := map[string]any{"seq": seq, "error_type": e.ErrorType, "message": e.Message}
		if e.Category != "" {
			out["category"] = e.Category
		}
		if e.RetryAfterMs != nil {
			out["retry_after_ms"] = *e.RetryAfterMs
		}
		if e.Usage != nil {
			out["usage"] = e.Raw["usage"]
		}
		if e.Safety != nil {
			out["safety"] = e.Raw["safety"]
		}
		return canon(map[string]any{"frame_error": out})
	case *dn.SessionClosed:
		return canon(map[string]any{"closed": map[string]any{"code": e.Code, "reopen": e.Reopen}})
	}
	return map[string]any{"unknown": fmt.Sprintf("%T", ev)}
}

// ---------------------------------------------------------------- responses and errors

func runResponse(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	client := makeClient(t, &object{keys: []string{"client"}, vals: map[string]any{"client": &object{keys: []string{"api_key"}, vals: map[string]any{"api_key": "dn_test_conformance"}}}},
		m.url, map[string]string{"x-mock-session": sid, "x-mock-scenario": "response/" + str(c.vals["id"])})
	result, err := invoke(context.Background(), root, client, c.vals["call"].(*object), refs{})
	if err != nil {
		return fmt.Errorf("the call failed: %w", err)
	}
	exp := expectation(c.vals["expect"]).(map[string]any)
	switch {
	case exp["decision"] != nil:
		return match(exp["decision"], normDecision(result.(*dn.Decision)), "decision")
	case exp["answer"] != nil:
		return match(exp["answer"], canon(normAnswer(result.(dn.Answer))), "answer")
	case exp["models"] != nil:
		var got []any
		want, _ := exp["models"].([]any)
		for i, mo := range result.([]dn.ModelInfo) {
			entry := map[string]any{"name": mo.Name, "description": mo.Description, "release_date": mo.ReleaseDate, "limits": mo.Limits}
			var listed map[string]any
			if i < len(want) {
				listed, _ = want[i].(map[string]any)
			}
			var fineTuned any
			if mo.FineTuned != nil {
				fineTuned = *mo.FineTuned
			}
			for k, v := range map[string]any{"fine_tuned": fineTuned, "base": nilIfEmpty(mo.Base), "adapter_version": intOrNil(mo.AdapterVersion), "status": nilIfEmpty(mo.Status)} {
				if _, ok := listed[k]; ok {
					entry[k] = v
				}
			}
			got = append(got, entry)
		}
		if g := canon(got); !same(exp["models"], g) {
			return fmt.Errorf("models: expected %s, got %s", short(exp["models"]), short(g))
		}
	case exp["raw"] != nil:
		if g := canon(result); !same(exp["raw"], g) {
			return fmt.Errorf("raw: expected %s, got %s", short(exp["raw"]), short(g))
		}
	case exp["batch"] != nil:
		return match(exp["batch"], normBatch(result.(*dn.Batch)), "batch")
	case exp["batch_page"] != nil:
		return match(exp["batch_page"], normBatchPage(result.(*dn.BatchPage)), "batch_page")
	case exp["collection"] != nil, exp["collections"] != nil, exp["item"] != nil, exp["item_page"] != nil, exp["deleted"] != nil:
		for _, kind := range collectionKinds {
			if want, ok := exp[kind]; ok {
				return checkCollectionResult(kind, want, result, kind)
			}
		}
	case exp["created_key"] != nil, exp["api_key"] != nil, exp["key_page"] != nil, exp["api_keys"] != nil, exp["key_revoked"] != nil, exp["keys_revoked"] != nil:
		for _, kind := range []string{"created_key", "api_key", "key_page", "api_keys", "key_revoked", "keys_revoked"} {
			if want, ok := exp[kind]; ok {
				return checkKeysResult(kind, want, result, kind)
			}
		}
	case exp["key_usage"] != nil, exp["usage_page"] != nil, exp["usage_items"] != nil:
		for _, kind := range []string{"key_usage", "usage_page", "usage_items"} {
			if want, ok := exp[kind]; ok {
				return checkUsageResult(kind, want, result, kind)
			}
		}
	case exp["batch_results"] != nil:
		return checkBatchResults(exp["batch_results"].([]any), result.([]dn.BatchResult), "batch_results")
	case exp["session"] != nil:
		return match(exp["session"], normSession(result.(*dn.Session)), "session")
	case exp["upload_ids"] != nil:
		if g := canon(result); !same(exp["upload_ids"], g) {
			return fmt.Errorf("upload_ids: expected %s, got %s", short(exp["upload_ids"]), short(g))
		}
	default:
		return fmt.Errorf("no expectation this harness knows: %v", exp)
	}
	return nil
}

func runError(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	client := makeClient(t, c, m.url, map[string]string{"x-mock-session": sid, "x-mock-scenario": "error/" + str(c.vals["id"])})
	_, err := invoke(context.Background(), root, client, c.vals["call"].(*object), refs{})
	if err == nil {
		return errors.New("no error was returned")
	}
	exp := expectation(c.vals["expect"]).(map[string]any)
	if e := checkError(exp["error"].(map[string]any), err, "error"); e != nil {
		return e
	}
	if n := len(m.requests(t, sid).Requests); n != 1 {
		return fmt.Errorf("expected 1 attempt with max_retries 0, got %d", n)
	}
	return nil
}

// ---------------------------------------------------------------- behaviour

func checkOutcome(spec map[string]any, result any, err error, path string) error {
	if ok, _ := spec["argument_error"].(bool); ok {
		var apiErr *dn.Error
		switch {
		case err == nil:
			return fmt.Errorf("%s: expected an argument error, got a result", path)
		case errors.As(err, &apiErr):
			return fmt.Errorf("%s: expected an argument error, got %T: %v", path, err, err)
		}
		return nil
	}
	kind, isResult := spec["result"].(string)
	if !isResult {
		if err == nil {
			return fmt.Errorf("%s: expected %v, got a result", path, spec["error"])
		}
		return checkError(spec, err, path)
	}
	if err != nil {
		return fmt.Errorf("%s: expected a %s, got %T: %v", path, kind, err, err)
	}
	switch kind {
	case "decision":
		d, ok := result.(*dn.Decision)
		if !ok {
			return fmt.Errorf("%s: expected a decision, got %T", path, result)
		}
		if want, ok := spec["decision"]; ok {
			return match(want, normDecision(d), path+".decision")
		}
	case "answer":
		a, ok := result.(dn.Answer)
		if !ok {
			return fmt.Errorf("%s: expected an answer, got %T", path, result)
		}
		want, _ := spec["answer"].(map[string]any)
		return match(orEmpty(want), canon(normAnswer(a)), path+".answer")
	case "batch":
		b, ok := result.(*dn.Batch)
		if !ok {
			return fmt.Errorf("%s: expected a batch, got %T", path, result)
		}
		want, _ := spec["batch"].(map[string]any)
		return match(orEmpty(want), normBatch(b), path+".batch")
	case "batch_page":
		p, ok := result.(*dn.BatchPage)
		if !ok {
			return fmt.Errorf("%s: expected a batch page, got %T", path, result)
		}
		want, _ := spec["batch_page"].(map[string]any)
		return match(orEmpty(want), normBatchPage(p), path+".batch_page")
	case "batch_results":
		rs, ok := result.([]dn.BatchResult)
		if !ok {
			return fmt.Errorf("%s: expected batch results, got %T", path, result)
		}
		return checkResultsSpec(spec, rs, path)
	case "collection", "collections", "item", "item_page", "deleted":
		return checkCollectionResult(kind, spec[kind], result, path+"."+kind)
	case "key_usage", "usage_page", "usage_items":
		return checkUsageResult(kind, spec[kind], result, path+"."+kind)
	case "created_key", "api_key", "key_page", "api_keys", "key_revoked", "keys_revoked":
		return checkKeysResult(kind, spec[kind], result, path+"."+kind)
	case "checked":
		if result != nil {
			return fmt.Errorf("%s: a check returned %v", path, result)
		}
	case "batch_run":
		run, ok := result.(*batchRun)
		if !ok {
			return fmt.Errorf("%s: expected a batch run, got %T", path, result)
		}
		want, _ := spec["batch"].(map[string]any)
		if err := match(orEmpty(want), normBatch(run.batch), path+".batch"); err != nil {
			return err
		}
		return checkResultsSpec(spec, run.results, path)
	case "session":
		s, ok := result.(*dn.Session)
		if !ok {
			return fmt.Errorf("%s: expected a session, got %T", path, result)
		}
		want, _ := spec["session"].(map[string]any)
		return match(orEmpty(want), normSession(s), path+".session")
	case "upload_ids":
		ids, ok := result.([]string)
		if !ok {
			return fmt.Errorf("%s: expected upload ids, got %T", path, result)
		}
		if n, ok := spec["count"].(float64); ok && len(ids) != int(n) {
			return fmt.Errorf("%s: expected %v upload ids, got %v", path, n, ids)
		}
		re := regexp.MustCompile(`^upl_[0-9a-f]{32}$`)
		seen := map[string]bool{}
		for _, id := range ids {
			if !re.MatchString(id) {
				return fmt.Errorf("%s: %q is not an upload id", path, id)
			}
			seen[id] = true
		}
		if d, ok := spec["distinct"].(bool); ok && (len(seen) == len(ids)) != d {
			return fmt.Errorf("%s: upload ids %v", path, ids)
		}
	case "supervised":
		sv, ok := result.(*supervised)
		if !ok {
			return fmt.Errorf("%s: expected supervise to return, got %T", path, result)
		}
		n := 0
		for _, ev := range sv.replies {
			if _, isReply := ev.(*dn.Reply); isReply {
				n++
			}
		}
		if r, ok := spec["replies"].(map[string]any); ok {
			if err := inRange(r, float64(n), "replies"); err != nil {
				return err
			}
		}
		if want, ok := spec["closed"]; ok {
			var got any
			if sv.closed != nil {
				got = canon(map[string]any{"code": sv.closed.Code, "reopen": sv.closed.Reopen})
			}
			if !same(want, got) {
				return fmt.Errorf("%s.closed: expected %s, got %s", path, short(want), short(got))
			}
		}
	default:
		return fmt.Errorf("%s: unknown result kind %q", path, kind)
	}
	return nil
}

// checkResultsSpec checks batch_results whole, or count with
// batch_results_sample by index.
func checkResultsSpec(spec map[string]any, results []dn.BatchResult, path string) error {
	if want, ok := spec["batch_results"].([]any); ok {
		if err := checkBatchResults(want, results, path+".batch_results"); err != nil {
			return err
		}
	}
	if n, ok := spec["count"].(float64); ok && len(results) != int(n) {
		return fmt.Errorf("%s: expected %v results, got %d", path, n, len(results))
	}
	samples, _ := spec["batch_results_sample"].([]any)
	for _, item := range samples {
		sample := map[string]any{}
		for k, v := range item.(map[string]any) {
			sample[k] = v
		}
		i := int(sample["index"].(float64))
		delete(sample, "index")
		if i >= len(results) {
			return fmt.Errorf("%s: no result %d of %d", path, i, len(results))
		}
		if err := checkBatchResults([]any{sample}, results[i:i+1], fmt.Sprintf("%s.batch_results[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func inRange(r map[string]any, v float64, what string) error {
	if lo, ok := r["min"].(float64); ok && v < lo {
		return fmt.Errorf("%s %.0f is below %v", what, v, lo)
	}
	if hi, ok := r["max"].(float64); ok && v > hi {
		return fmt.Errorf("%s %.0f is above %v", what, v, hi)
	}
	return nil
}

// runSteps runs a case's steps on one client and records the session events.
func runSteps(t *testing.T, root string, m *mock, sid string, client *dn.Client, steps []any) ([]any, error) {
	ctx := context.Background()
	saved := refs{}
	var events []any
	for i, item := range steps {
		step := item.(*object)
		where := fmt.Sprintf("steps[%d]", i)
		switch {
		case step.vals["call"] != nil:
			result, err := invoke(ctx, root, client, step.vals["call"].(*object), saved)
			if o, ok := get(step, "outcome"); ok {
				if e := checkOutcome(expectation(o).(map[string]any), result, err, where+".outcome"); e != nil {
					return events, e
				}
			} else if err != nil {
				return events, fmt.Errorf("%s: the call failed: %w", where, err)
			}
			if name := str(step.vals["save_as"]); name != "" {
				saved[name] = result
			}
		case step.vals["connect"] != nil:
			s := saved[str(step.vals["connect"])].(*dn.Session)
			if err := s.Connect(ctx); err != nil {
				var closed *dn.SessionClosed
				if errors.As(err, &closed) {
					events = append(events, normEvent(closed))
					continue
				}
				return events, fmt.Errorf("%s: connect: %w", where, err)
			}
		case step.vals["send"] != nil:
			args := step.vals["send"].(*object)
			s := saved[str(args.vals["session"])].(*dn.Session)
			f, err := decodeFrame(root, args, saved)
			if err != nil {
				return events, err
			}
			if check, _ := step.vals["check"].(bool); check {
				err = s.CheckFrame(f)
				o, _ := get(step, "outcome")
				if e := checkOutcome(expectation(o).(map[string]any), nil, err, where+".outcome"); e != nil {
					return events, e
				}
				continue
			}
			_, err = s.Send(ctx, f)
			if o, ok := get(step, "outcome"); ok {
				// A frame that breaks a fixed rule is refused locally and
				// nothing is sent.
				if err == nil {
					return events, fmt.Errorf("%s: expected %v, the frame was sent", where, plain(o))
				}
				if e := checkOutcome(expectation(o).(map[string]any), nil, err, where+".outcome"); e != nil {
					return events, e
				}
			} else if err != nil {
				return events, fmt.Errorf("%s: send: %w", where, err)
			}
		case step.vals["read"] != nil:
			args := step.vals["read"].(*object)
			s := saved[str(args.vals["session"])].(*dn.Session)
			timeout := 10 * time.Second
			if n, ok := args.vals["timeout_ms"].(json.Number); ok {
				ms, _ := n.Int64()
				timeout = time.Duration(ms) * time.Millisecond
			}
			count := -1
			if n, ok := args.vals["count"].(json.Number); ok {
				c, _ := n.Int64()
				count = int(c)
			}
			for count != 0 {
				rctx, cancel := context.WithTimeout(ctx, timeout)
				ev, err := s.Next(rctx)
				cancel()
				if err != nil {
					return events, fmt.Errorf("%s: no session event arrived in time (%d read so far)", where, len(events))
				}
				events = append(events, normEvent(ev))
				if _, closed := ev.(*dn.SessionClosed); closed {
					break
				}
				count--
			}
		case step.vals["end"] != nil:
			s := saved[str(step.vals["end"])].(*dn.Session)
			if _, err := s.End(ctx); err != nil {
				var base *dn.Error
				if !errors.As(err, &base) {
					return events, fmt.Errorf("%s: end: %w", where, err)
				}
				events = append(events, canon(map[string]any{"end_error": map[string]any{"class": errorClasses(err)[0], "status": base.Status}}))
				continue
			}
			events = append(events, map[string]any{"ended": true})
		case step.vals["close"] != nil:
			_ = saved[str(step.vals["close"])].(*dn.Session).Close()
		case step.vals["mock_clock"] != nil:
			m.post(t, "/_mock/clock?session="+url.QueryEscape(sid), plain(step.vals["mock_clock"]))
		case step.vals["sleep_ms"] != nil:
			ms, _ := step.vals["sleep_ms"].(json.Number).Int64()
			time.Sleep(time.Duration(ms) * time.Millisecond)
		default:
			return events, fmt.Errorf("%s: unknown step %v", where, step.keys)
		}
	}
	return events, nil
}

func checkSequence(expected []any, log []mockRecord) error {
	if len(expected) != len(log) {
		return fmt.Errorf("request sequence: expected %d requests, the mock received %d: %s", len(expected), len(log), describe(log))
	}
	for i, item := range expected {
		if err := checkRequest(item.(*object), log[i], fmt.Sprintf("request sequence[%d]", i), true); err != nil {
			return err
		}
	}
	return nil
}

// checkSameHeader checks that every logged request matching the spec's
// path_suffix and method (at least min, default 2) carries the header with
// one and the same value, matching pattern when given.
func checkSameHeader(spec map[string]any, log []mockRecord) error {
	name := strings.ToLower(spec["name"].(string))
	var values []string
	for _, r := range log {
		if suffix, ok := spec["path_suffix"].(string); ok && !strings.HasSuffix(r.Path, suffix) {
			continue
		}
		if method, ok := spec["method"].(string); ok && r.Method != method {
			continue
		}
		v, present := r.Headers[name]
		if !present {
			return fmt.Errorf("same_header %s: an attempt of %s %s carries none", name, r.Method, r.Path)
		}
		values = append(values, v)
	}
	least := 2
	if m, ok := spec["min"].(float64); ok {
		least = int(m)
	}
	if len(values) < least {
		return fmt.Errorf("same_header %s: expected at least %d attempts, got %d", name, least, len(values))
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return fmt.Errorf("same_header %s: expected one value on every attempt, got %v", name, values)
		}
	}
	if p, ok := spec["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(values[0]) {
		return fmt.Errorf("same_header %s: %q does not match %s", name, values[0], p)
	}
	return nil
}

func checkCounts(expected []any, log []mockRecord) error {
	for _, item := range expected {
		spec := item.(map[string]any)
		n := 0
		for _, r := range log {
			if r.Method != spec["method"] {
				continue
			}
			if p, ok := spec["path"].(string); ok && r.Path != p {
				continue
			}
			if p, ok := spec["path_pattern"].(string); ok && !regexp.MustCompile(p).MatchString(r.Path) {
				continue
			}
			n++
		}
		what := fmt.Sprintf("%v %v%v", spec["method"], spec["path"], spec["path_pattern"])
		if c, ok := spec["count"].(float64); ok && float64(n) != c {
			return fmt.Errorf("%s: expected %v requests, got %d", what, c, n)
		}
		if err := inRange(spec, float64(n), what); err != nil {
			return err
		}
	}
	return nil
}

func checkEvents(expected []any, events []any) error {
	if len(expected) != len(events) {
		return fmt.Errorf("session events: expected %d, got %d: %s", len(expected), len(events), short(events))
	}
	for i, e := range expected {
		spec := e.(map[string]any)
		got, _ := events[i].(map[string]any)
		for key, want := range spec {
			have, ok := got[key]
			if !ok || len(got) != 1 {
				return fmt.Errorf("session events[%d]: expected %s, got %s", i, key, short(got))
			}
			if wm, isMap := want.(map[string]any); isMap {
				if err := match(wm, have, fmt.Sprintf("session events[%d].%s", i, key)); err != nil {
					return err
				}
			} else if !same(want, have) {
				return fmt.Errorf("session events[%d]: expected %s, got %s", i, short(spec), short(got))
			}
		}
	}
	return nil
}

func runBehaviour(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	extra := map[string]string{"x-mock-session": sid}
	if v, ok := get(c, "mock"); ok && v != nil {
		extra["x-mock-scenario"] = "behaviour/" + str(c.vals["id"])
	}
	exp := expectation(c.vals["expect"]).(map[string]any)
	var mu sync.Mutex
	var waits []time.Duration
	var more []dn.Option
	if _, ok := exp["scheduled_waits_ms"]; ok {
		more = append(more, dn.WithSleep(func(ctx context.Context, d time.Duration) error {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			return ctx.Err()
		}))
	}
	if spec, _ := c.vals["client"].(*object); spec != nil && str(spec.vals["clock"]) == "mock" {
		// The SDK's clock reads the mock's clock for this session, so a case
		// that moves the mock's time moves the SDK's with it.
		more = append(more, dn.WithClock(func() time.Time { return m.clockNow(t, sid) }))
	}
	client := makeClient(t, c, m.url, extra, more...)
	start := time.Now()
	var result any
	var err error
	var events []any
	if steps, ok := c.vals["steps"].([]any); ok {
		if events, err = runSteps(t, root, m, sid, client, steps); err != nil {
			return err
		}
	} else {
		result, err = invoke(context.Background(), root, client, c.vals["call"].(*object), refs{})
	}
	elapsed := float64(time.Since(start)) / float64(time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	info := m.requests(t, sid)
	if alts, ok := exp["one_of"].([]any); ok {
		var reasons []string
		matched := false
		for i, a := range alts {
			alt := a.(map[string]any)
			if want := int(alt["attempts"].(float64)); len(info.Requests) != want {
				reasons = append(reasons, fmt.Sprintf("one_of[%d]: attempts: expected %d, the mock received %d", i, want, len(info.Requests)))
				continue
			}
			if e := checkOutcome(alt["outcome"].(map[string]any), result, err, fmt.Sprintf("one_of[%d].outcome", i)); e != nil {
				reasons = append(reasons, e.Error())
				continue
			}
			matched = true
			break
		}
		if !matched {
			return fmt.Errorf("no alternative matched: %s", strings.Join(reasons, "; "))
		}
	} else if want, ok := exp["attempts"].(float64); ok && len(info.Requests) != int(want) {
		return fmt.Errorf("attempts: expected %d, the mock received %d: %s", int(want), len(info.Requests), describe(info.Requests))
	}
	if seq, ok := c.vals["expect"].(*object).vals["request_sequence"].([]any); ok {
		if e := checkSequence(seq, info.Requests); e != nil {
			return e
		}
	}
	if counts, ok := exp["request_counts"].([]any); ok {
		if e := checkCounts(counts, info.Requests); e != nil {
			return e
		}
	}
	if same, ok := exp["same_header"].(map[string]any); ok {
		if e := checkSameHeader(same, info.Requests); e != nil {
			return e
		}
	}
	if want, ok := exp["session_events"].([]any); ok {
		if e := checkEvents(want, events); e != nil {
			return e
		}
	}
	if r, ok := exp["pongs"].(map[string]any); ok {
		if e := inRange(r, float64(info.Pongs), "pongs"); e != nil {
			return e
		}
	}
	if o, ok := exp["outcome"].(map[string]any); ok {
		if e := checkOutcome(o, result, err, "outcome"); e != nil {
			return e
		}
	}
	if rs, ok := exp["results"].([]any); ok {
		got, isList := result.([]dn.DecisionResult)
		if !isList || len(got) != len(rs) {
			return fmt.Errorf("results: expected %d results, got %v (%v)", len(rs), result, err)
		}
		for i, spec := range rs {
			var res any
			if got[i].Decision != nil {
				res = got[i].Decision
			}
			if e := checkOutcome(spec.(map[string]any), res, got[i].Err, fmt.Sprintf("results[%d]", i)); e != nil {
				return e
			}
		}
	}
	if gs, ok := exp["gaps_ms"].([]any); ok {
		for i, g := range gs {
			if i+1 >= len(info.Requests) {
				return fmt.Errorf("gap %d: not enough requests", i+1)
			}
			gap := (info.Requests[i+1].Time - info.Requests[i].Time) * 1000
			if e := inRange(g.(map[string]any), gap, fmt.Sprintf("gap %d (ms)", i+1)); e != nil {
				return e
			}
		}
	}
	if r, ok := exp["elapsed_ms"].(map[string]any); ok {
		if e := inRange(r, elapsed, "elapsed (ms)"); e != nil {
			return e
		}
	}
	if r, ok := exp["max_in_flight"].(map[string]any); ok {
		if e := inRange(r, info.MaxInFlight, "max in flight"); e != nil {
			return e
		}
	}
	if ws, ok := exp["scheduled_waits_ms"].([]any); ok {
		mu.Lock()
		got := append([]time.Duration(nil), waits...)
		mu.Unlock()
		if len(got) != len(ws) {
			return fmt.Errorf("scheduled waits: expected %d, got %v", len(ws), got)
		}
		for i, w := range ws {
			ms := float64(got[i]) / float64(time.Millisecond)
			if e := inRange(w.(map[string]any), ms, fmt.Sprintf("scheduled wait %d (ms)", i+1)); e != nil {
				return e
			}
		}
	}
	if ps, ok := exp["unrequested_paths"].([]any); ok {
		for _, p := range ps {
			if n := len(m.requestsAt(t, str(p)).Requests); n != 0 {
				return fmt.Errorf("path %s was requested %d times", str(p), n)
			}
		}
	}
	return nil
}

func runConstant(t *testing.T, root string, m *mock, c *object) error {
	name := str(c.vals["constant"])
	if names, ok := get(c, "names"); ok && names != nil {
		if n, ok := get(names.(*object), "go"); ok {
			name = str(n)
		}
	}
	values := map[string]any{"SpecVersion": dn.SpecVersion, "DefaultInlineThreshold": float64(dn.DefaultInlineThreshold)}
	got, ok := values[name]
	if !ok {
		return fmt.Errorf("no exported constant %s", name)
	}
	if want := plain(c.vals["equals"]); !same(want, got) {
		return fmt.Errorf("%s: expected %s, got %s", name, short(want), short(got))
	}
	return nil
}

// ---------------------------------------------------------------- the test

func TestConformance(t *testing.T) {
	root := suiteDir(t)
	m := startMock(t, root)
	runners := []struct {
		kind string
		run  func(*testing.T, string, *mock, *object) error
	}{
		{"requests", runRequest},
		{"responses", runResponse},
		{"errors", runError},
		{"behaviour", runBehaviour},
		{"constants", runConstant},
	}
	only := os.Getenv("DECISIONNODE_CONFORMANCE_CASE")
	totals := map[string][2]int{}
	for _, r := range runners {
		files, err := filepath.Glob(filepath.Join(root, "cases", r.kind, "*.json"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no %s cases in %s", r.kind, root)
		}
		sort.Strings(files)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := parseOrdered(data)
			if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			c := doc.(*object)
			id := str(c.vals["id"])
			if id != strings.TrimSuffix(filepath.Base(f), ".json") {
				t.Errorf("%s: id %q does not match the file name", f, id)
			}
			if only != "" && only != id {
				continue
			}
			tot := totals[r.kind]
			tot[1]++
			ok := t.Run(r.kind+"/"+id, func(t *testing.T) {
				if err := r.run(t, root, m, c); err != nil {
					t.Fatal(err)
				}
			})
			if ok {
				tot[0]++
			}
			totals[r.kind] = tot
		}
	}
	all := [2]int{}
	for _, r := range runners {
		tot := totals[r.kind]
		all[0] += tot[0]
		all[1] += tot[1]
		t.Logf("conformance %-10s %d/%d passed", r.kind, tot[0], tot[1])
	}
	t.Logf("conformance: %d of %d cases passed", all[0], all[1])
}
