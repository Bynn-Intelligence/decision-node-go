package decisionnode_test

// The conformance suite: language-neutral cases run against the suite's mock
// server. Point DECISIONNODE_CONFORMANCE_DIR at a checkout of the suite. When
// it is unset the test fails: it is never skipped.

import (
	"bufio"
	"bytes"
	"context"
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

type mockLog struct {
	Requests []struct {
		Time     float64           `json:"time"`
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		Headers  map[string]string `json:"headers"`
		BodyText string            `json:"body_text"`
	} `json:"requests"`
	MaxInFlight float64 `json:"max_in_flight"`
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
	return out
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

func same(a, b any) bool {
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
	if eok && aok {
		keys := make([]string, 0, len(em))
		for k := range em {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			av, ok := am[k]
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
		"usage":      map[string]any{"input_tokens": intOrNil(d.Usage.InputTokens), "output_tokens": intOrNil(d.Usage.OutputTokens)},
		"safety":     normSafety(d.Safety),
		"answers":    answers,
		"raw":        d.Raw,
	})
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
	case *dn.ValidationError:
		return []string{"ValidationError", "DecisionNodeError"}
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
	if errors.As(err, &rate) {
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
		}
	}
	return raw(v)
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

// argValue returns a plain-data argument: nil when absent or null, else its
// exact JSON.
func argValue(args *object, key string) any {
	v, ok := get(args, key)
	if !ok || v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return raw(v)
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

func decodeImages(root string, v any) ([]dn.Image, error) {
	arr, _ := v.([]any)
	var out []dn.Image
	for _, item := range arr {
		o := item.(*object)
		args := o.vals["$image"].(*object)
		id := str(args.vals["id"])
		switch {
		case args.vals["path"] != nil:
			img, err := dn.ImageFile(id, filepath.Join(root, str(args.vals["path"])))
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		case args.vals["url"] != nil:
			out = append(out, dn.ImageURL(id, str(args.vals["url"])))
		default:
			data, err := base64.StdEncoding.DecodeString(str(args.vals["data_base64"]))
			if err != nil {
				panic(err)
			}
			out = append(out, dn.ImageData(id, data, str(args.vals["media_type"])))
		}
	}
	return out, nil
}

func decodeRequest(root string, args *object) (dn.Request, error) {
	images, err := decodeImages(root, args.vals["images"])
	return dn.Request{
		State:     argValue(args, "state"),
		Questions: decodeQuestions(args.vals["questions"]),
		Images:    images,
		Model:     str(args.vals["model"]),
	}, err
}

func invoke(ctx context.Context, root string, c *dn.Client, call *object) (any, error) {
	method := str(call.vals["method"])
	args, _ := call.vals["args"].(*object)
	if args == nil {
		args = &object{vals: map[string]any{}}
	}
	// A file image is read when it is made, so an unreadable one fails here,
	// as an argument error, before the call.
	images, err := decodeImages(root, args.vals["images"])
	if err != nil {
		return nil, err
	}
	switch method {
	case "decide":
		req, err := decodeRequest(root, args)
		if err != nil {
			return nil, err
		}
		return c.Decide(ctx, req)
	case "decide_many":
		var reqs []dn.Request
		for _, r := range args.vals["requests"].([]any) {
			req, err := decodeRequest(root, r.(*object))
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
			Model:        str(args.vals["model"]),
		})
	case "classify":
		return c.Classify(ctx, dn.ClassifyRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			Options:      decodeOptions(args.vals["options"]),
			Images:       images,
			Model:        str(args.vals["model"]),
		})
	case "score":
		return c.Score(ctx, dn.ScoreRequest{
			State:        argValue(args, "state"),
			Instructions: argValue(args, "instructions"),
			Levels:       argValue(args, "levels"),
			Images:       images,
			Model:        str(args.vals["model"]),
		})
	case "gate":
		return c.Gate(ctx, dn.GateRequest{
			Action:  argValue(args, "action"),
			Context: argValue(args, "context"),
			Images:  images,
			Model:   str(args.vals["model"]),
		})
	case "models":
		return c.Models(ctx)
	case "decide_raw":
		return c.DecideRaw(ctx, raw(args.vals["body"]))
	}
	return nil, fmt.Errorf("unknown method %q", method)
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

// ---------------------------------------------------------------- the five kinds

func runRequest(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	client := makeClient(t, c, m.url, map[string]string{"x-mock-session": sid})
	if _, err := invoke(context.Background(), root, client, c.vals["call"].(*object)); err != nil {
		return fmt.Errorf("the call failed: %w", err)
	}
	log := m.requests(t, sid).Requests
	if len(log) != 1 {
		return fmt.Errorf("expected 1 request, the mock received %d", len(log))
	}
	got := log[0]
	exp := c.vals["expect"].(*object).vals["request"].(*object)
	if got.Method != str(exp.vals["method"]) {
		return fmt.Errorf("method: expected %s, got %s", str(exp.vals["method"]), got.Method)
	}
	if got.Path != str(exp.vals["path"]) {
		return fmt.Errorf("path: expected %s, got %s", str(exp.vals["path"]), got.Path)
	}
	if h, ok := get(exp, "headers"); ok {
		for _, k := range h.(*object).keys {
			want := str(h.(*object).vals[k])
			if have, ok := got.Headers[strings.ToLower(k)]; !ok || have != want {
				return fmt.Errorf("header %s: expected %q, got %q", k, want, have)
			}
		}
	}
	if h, ok := get(exp, "header_patterns"); ok {
		for _, k := range h.(*object).keys {
			re := regexp.MustCompile(str(h.(*object).vals[k]))
			have, ok := got.Headers[strings.ToLower(k)]
			if !ok || !re.MatchString(have) {
				return fmt.Errorf("header %s: %q does not match %s", k, have, re)
			}
		}
	}
	if a, ok := get(exp, "absent_headers"); ok {
		for _, n := range a.([]any) {
			if _, present := got.Headers[strings.ToLower(str(n))]; present {
				return fmt.Errorf("header %s must not be sent", str(n))
			}
		}
	}
	var body any
	if got.BodyText != "" {
		var err error
		if body, err = parseOrdered([]byte(got.BodyText)); err != nil {
			return fmt.Errorf("the body is not JSON: %w", err)
		}
	}
	expBody, hasBody := get(exp, "body")
	checks, hasChecks := get(exp, "body_checks")
	if hasBody {
		if !same(plain(expBody), plain(body)) {
			return fmt.Errorf("body: expected %s, got %s", short(plain(expBody)), short(plain(body)))
		}
	} else if !hasChecks && got.BodyText != "" {
		return fmt.Errorf("expected no body, got %s", got.BodyText)
	}
	if hasChecks {
		for _, item := range checks.([]any) {
			chk := item.(*object)
			ptr := str(chk.vals["pointer"])
			val, found := pointer(body, ptr)
			if !found {
				return fmt.Errorf("body%s: missing", ptr)
			}
			if eq, ok := get(chk, "equals"); ok && !same(plain(eq), plain(val)) {
				return fmt.Errorf("body%s: expected %s, got %s", ptr, short(plain(eq)), short(plain(val)))
			}
			if typ, ok := get(chk, "type"); ok && !jsonType(str(typ), plain(val)) {
				return fmt.Errorf("body%s: expected a %s", ptr, str(typ))
			}
			if keys, ok := get(chk, "keys"); ok {
				o, isObj := val.(*object)
				if !isObj || !same(plain(keys), plain(stringsToAny(o.keys))) {
					return fmt.Errorf("body%s: keys expected %s", ptr, short(plain(keys)))
				}
			}
		}
	}
	if ok, present := get(exp, "ordered_keys"); present {
		for _, p := range ok.([]any) {
			want, _ := pointer(expBody, str(p))
			have, _ := pointer(body, str(p))
			wo, _ := want.(*object)
			ho, _ := have.(*object)
			if wo == nil || ho == nil || strings.Join(wo.keys, "\x00") != strings.Join(ho.keys, "\x00") || len(wo.keys) != len(ho.keys) {
				var hk []string
				if ho != nil {
					hk = ho.keys
				}
				return fmt.Errorf("body%s: key order expected %q, got %q", str(p), wo.keys, hk)
			}
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

func runResponse(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	client := makeClient(t, &object{keys: []string{"client"}, vals: map[string]any{"client": &object{keys: []string{"api_key"}, vals: map[string]any{"api_key": "dn_test_conformance"}}}},
		m.url, map[string]string{"x-mock-session": sid, "x-mock-scenario": "response/" + str(c.vals["id"])})
	result, err := invoke(context.Background(), root, client, c.vals["call"].(*object))
	if err != nil {
		return fmt.Errorf("the call failed: %w", err)
	}
	exp := plain(c.vals["expect"]).(map[string]any)
	switch {
	case exp["decision"] != nil:
		return match(exp["decision"], normDecision(result.(*dn.Decision)), "decision")
	case exp["answer"] != nil:
		return match(exp["answer"], canon(normAnswer(result.(dn.Answer))), "answer")
	case exp["models"] != nil:
		var got []any
		for _, mo := range result.([]dn.ModelInfo) {
			got = append(got, map[string]any{"name": mo.Name, "description": mo.Description, "release_date": mo.ReleaseDate, "limits": mo.Limits})
		}
		if g := canon(got); !same(exp["models"], g) {
			return fmt.Errorf("models: expected %s, got %s", short(exp["models"]), short(g))
		}
	case exp["raw"] != nil:
		if g := canon(result); !same(exp["raw"], g) {
			return fmt.Errorf("raw: expected %s, got %s", short(exp["raw"]), short(g))
		}
	}
	return nil
}

func runError(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	client := makeClient(t, c, m.url, map[string]string{"x-mock-session": sid, "x-mock-scenario": "error/" + str(c.vals["id"])})
	_, err := invoke(context.Background(), root, client, c.vals["call"].(*object))
	if err == nil {
		return errors.New("no error was returned")
	}
	exp := plain(c.vals["expect"]).(map[string]any)
	if e := checkError(exp["error"].(map[string]any), err, "error"); e != nil {
		return e
	}
	if n := len(m.requests(t, sid).Requests); n != 1 {
		return fmt.Errorf("expected 1 attempt with max_retries 0, got %d", n)
	}
	return nil
}

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
	if _, ok := spec["result"]; ok {
		if err != nil {
			return fmt.Errorf("%s: expected a decision, got %v", path, err)
		}
		if d, ok := spec["decision"]; ok {
			return match(d, normDecision(result.(*dn.Decision)), path+".decision")
		}
		return nil
	}
	if err == nil {
		return fmt.Errorf("%s: expected %v, got a result", path, spec["error"])
	}
	return checkError(spec, err, path)
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

func runBehaviour(t *testing.T, root string, m *mock, c *object) error {
	sid := session()
	extra := map[string]string{"x-mock-session": sid}
	if v, ok := get(c, "mock"); ok && v != nil {
		extra["x-mock-scenario"] = "behaviour/" + str(c.vals["id"])
	}
	exp := plain(c.vals["expect"]).(map[string]any)
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
	client := makeClient(t, c, m.url, extra, more...)
	start := time.Now()
	result, err := invoke(context.Background(), root, client, c.vals["call"].(*object))
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
	} else if want := int(exp["attempts"].(float64)); len(info.Requests) != want {
		return fmt.Errorf("attempts: expected %d, the mock received %d", want, len(info.Requests))
	}
	if o, ok := exp["outcome"].(map[string]any); ok {
		if e := checkOutcome(o, result, err, "outcome"); e != nil {
			return e
		}
	}
	if rs, ok := exp["results"].([]any); ok {
		got, isList := result.([]dn.DecisionResult)
		if !isList || len(got) != len(rs) {
			return fmt.Errorf("results: expected %d results, got %v", len(rs), result)
		}
		for i, spec := range rs {
			if e := checkOutcome(spec.(map[string]any), got[i].Decision, got[i].Err, fmt.Sprintf("results[%d]", i)); e != nil {
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
	values := map[string]any{"SpecVersion": dn.SpecVersion}
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
	total, passed := 0, 0
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
			total++
			ok := t.Run(r.kind+"/"+id, func(t *testing.T) {
				if err := r.run(t, root, m, c); err != nil {
					t.Fatal(err)
				}
			})
			if ok {
				passed++
			}
		}
	}
	t.Logf("conformance: %d of %d cases passed", passed, total)
}
