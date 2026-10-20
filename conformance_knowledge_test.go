package decisionnode_test

// The conformance suite's knowledge cases (suite 1.23, spec 1.4): documents,
// the knowledge booster and its reading, and their results.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dn "github.com/Bynn-Intelligence/decision-node-go"
)

// decodeDocument builds a Document from {"$document": {...}}.
func decodeDocument(root string, v any, r refs) (dn.Document, error) {
	o, ok := v.(*object)
	if !ok || len(o.keys) != 1 || o.keys[0] != "$document" {
		return dn.Document{}, fmt.Errorf("not a $document: %s", short(plain(v)))
	}
	args := o.vals["$document"].(*object)
	s := func(key string) string { return str(plainArg(args.vals[key], r)) }
	if _, ok := get(args, "text"); ok {
		return dn.DocumentText(s("text"), s("format")), nil
	}
	if _, ok := get(args, "upload_id"); ok {
		return dn.DocumentUpload(s("upload_id")), nil
	}
	contentType, filename := s("content_type"), s("filename")
	_, padded := get(args, "pad_to")
	if _, ok := get(args, "path"); ok && !padded {
		return dn.DocumentFile(filepath.Join(root, s("path")), contentType, filename), nil
	}
	var data []byte
	switch {
	case args.vals["data_text"] != nil:
		data = []byte(s("data_text"))
	case args.vals["repeat_text"] != nil:
		rt := args.vals["repeat_text"].(*object)
		n, _ := rt.vals["times"].(json.Number).Int64()
		data = []byte(strings.Repeat(str(rt.vals["repeat"]), int(n)))
	case args.vals["path"] != nil:
		b, err := os.ReadFile(filepath.Join(root, s("path")))
		if err != nil {
			return dn.Document{}, err
		}
		data = b
		if filename == "" {
			filename = filepath.Base(s("path"))
		}
	default:
		text := s("data_base64")
		if _, ok := get(args, "stream_base64"); ok {
			text = s("stream_base64")
		}
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return dn.Document{}, err
		}
		data = b
	}
	if padded {
		n, _ := args.vals["pad_to"].(json.Number).Int64()
		if pad := int(n) - len(data); pad > 0 {
			data = append(data, make([]byte, pad)...)
		}
	}
	if _, ok := get(args, "stream_base64"); ok {
		return dn.DocumentReader(bytes.NewReader(data), contentType, filename), nil
	}
	return dn.DocumentData(data, contentType, filename), nil
}

// decodeKnowledge builds the knowledge booster from {"$knowledge": {...}}.
func decodeKnowledge(args *object, r refs) dn.Booster {
	var o dn.KnowledgeOptions
	sp := func(key string) *string {
		if v, ok := get(args, key); ok && v != nil {
			return dn.Ptr(str(plainArg(v, r)))
		}
		return nil
	}
	anyArg := func(key string) any {
		if v, ok := get(args, key); ok && v != nil {
			return plainArg(v, r)
		}
		return nil
	}
	o.Query, o.Scope = sp("query"), sp("scope")
	o.Match, o.Filter, o.MaxTokens = anyArg("match"), anyArg("filter"), anyArg("max_tokens")
	o.TopK = intArg(args, "top_k")
	o.Pinned, o.IncludeText = boolArg(args, "pinned"), boolArg(args, "include_text")
	if n, ok := args.vals["min_score"].(json.Number); ok {
		f, _ := n.Float64()
		o.MinScore = &f
	}
	if b := boolArg(args, "require"); b != nil {
		o.Require = *b
	}
	return dn.KnowledgeBooster(str(plainArg(args.vals["collection"], r)), o)
}

// intArg is a whole-number argument, nil when absent.
func intArg(args *object, key string) *int {
	if n, ok := args.vals[key].(json.Number); ok {
		i, _ := n.Int64()
		return dn.Ptr(int(i))
	}
	return nil
}

func documentOptions(args *object, r refs) dn.DocumentOptions {
	var o dn.DocumentOptions
	sp := func(key string) *string {
		if v, ok := get(args, key); ok && v != nil {
			return dn.Ptr(str(plainArg(v, r)))
		}
		return nil
	}
	o.Title, o.Reference, o.Label = sp("title"), sp("reference"), sp("label")
	o.Pinned, o.PinRank = boolArg(args, "pinned"), intArg(args, "pin_rank")
	if v, ok := get(args, "metadata"); ok && v != nil {
		o.Metadata = metadataArg(v)
	}
	if b := boolArg(args, "wait"); b != nil {
		o.Wait = *b
	}
	return o
}

// invokeKnowledge runs the knowledge calls; handled is false for any other.
func invokeKnowledge(ctx context.Context, root string, c *dn.Client, method string, args *object, r refs) (result any, handled bool, err error) {
	cols := c.Collections()
	id := func(key string) string { return str(plainArg(args.vals[key], r)) }
	switch method {
	case "collections.add_document", "collections.replace_document":
		doc, err := decodeDocument(root, args.vals["document"], r)
		if err != nil {
			return nil, true, err
		}
		if method == "collections.add_document" {
			res, err := cols.AddDocument(ctx, id("collection_id"), doc, documentOptions(args, r))
			return res, true, err
		}
		res, err := cols.ReplaceDocument(ctx, id("collection_id"), id("item_id"), doc, documentOptions(args, r))
		return res, true, err
	case "collections.get_item":
		include := boolArg(args, "include_text")
		res, err := cols.GetItem(ctx, id("collection_id"), id("item_id"), include != nil && *include)
		return res, true, err
	case "collections.list_chunks":
		res, err := cols.ListChunks(ctx, id("collection_id"), id("item_id"), dn.ChunkListOptions{Offset: intArg(args, "offset"), Limit: intArg(args, "limit")})
		return res, true, err
	case "collections.search":
		var o dn.SearchOptions
		if v, ok := get(args, "scope"); ok && v != nil {
			o.Scope = dn.Ptr(str(plainArg(v, r)))
		}
		for _, f := range []struct {
			key string
			to  *any
		}{{"match", &o.Match}, {"filter", &o.Filter}, {"max_tokens", &o.MaxTokens}} {
			if v, ok := get(args, f.key); ok && v != nil {
				*f.to = plainArg(v, r)
			}
		}
		o.TopK, o.Pinned = intArg(args, "top_k"), boolArg(args, "pinned")
		res, err := cols.Search(ctx, id("collection_id"), id("query"), o)
		return res, true, err
	case "collections.wait_until_ready":
		var o dn.WaitOptions
		for _, f := range []struct {
			key string
			to  **time.Duration
		}{{"timeout", &o.Timeout}, {"poll_interval", &o.PollInterval}} {
			if n, ok := args.vals[f.key].(json.Number); ok {
				s, _ := n.Float64()
				*f.to = dn.Ptr(time.Duration(s * float64(time.Second)))
			}
		}
		if v, ok := get(args, "item_id"); ok && v != nil {
			res, err := cols.WaitUntilReady(ctx, id("collection_id"), id("item_id"), o)
			return res, true, err
		}
		res, err := cols.WaitUntilCollectionReady(ctx, id("collection_id"), o)
		return res, true, err
	case "collections.delete_failed":
		res, err := cols.DeleteFailed(ctx, id("collection_id"))
		return res, true, err
	case "upload_documents":
		var docs []dn.Document
		for _, v := range listArg(args.vals["documents"]) {
			d, err := decodeDocument(root, v, r)
			if err != nil {
				return nil, true, err
			}
			docs = append(docs, d)
		}
		ups, err := c.UploadDocuments(ctx, docs)
		if err != nil {
			return nil, true, err
		}
		ids := make([]string, len(ups))
		for i, u := range ups {
			ids[i] = u.ID
		}
		return ids, true, nil
	}
	return nil, false, nil
}

func normKnowledge(x *dn.KnowledgeReading) any {
	passages := []any{}
	for _, p := range x.Passages {
		var rendered any
		if p.Rendered != nil {
			rendered = *p.Rendered
		}
		var tokens any
		if _, ok := p.Raw["tokens"]; ok && p.Raw["tokens"] != nil {
			tokens = p.Tokens
		}
		var lexical any
		if _, ok := p.Raw["lexical"].(bool); ok {
			lexical = p.Lexical
		}
		passages = append(passages, map[string]any{"n": p.N, "pinned": p.Pinned, "item": p.Item, "reference": nilIfEmpty(p.Reference),
			"title": nilIfEmpty(p.Title), "heading": nilIfEmpty(p.Heading), "chunk": p.Chunk, "byte_start": p.ByteStart, "byte_end": p.ByteEnd,
			"tokens": tokens, "score": floatOrNil(p.Score), "lexical": lexical, "rendered": rendered, "text": nilIfEmpty(p.Text)})
	}
	raw := x.Raw()
	// An object the reading did not send is null; one it sent holds its
	// typed fields, each null when not sent, beside the keys this version
	// does not type, as sent.
	sub := func(key string, fields map[string]any) any {
		m, ok := raw[key].(map[string]any)
		if !ok {
			return nil
		}
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		for k, v := range fields {
			if _, sent := m[k]; sent {
				out[k] = v
			}
		}
		return out
	}
	var scope any
	if x.Scope != nil {
		scope = sub("scope", map[string]any{"key": x.Scope.Key, "value": nilIfEmpty(x.Scope.Value)})
	}
	var omitted any
	if x.Omitted != nil {
		omitted = x.Omitted
	}
	var truncated any
	if x.Truncated != nil {
		truncated = *x.Truncated
	}
	var pending any
	if _, ok := raw["pending_items"]; ok {
		pending = x.PendingItems
	}
	return map[string]any{"kind": "knowledge", "collection": x.Collection, "collection_name": nilIfEmpty(x.CollectionName),
		"corpus": sub("corpus", map[string]any{"version": x.Corpus.Version}),
		"query":  sub("query", map[string]any{"source": nilIfEmpty(x.Query.Source), "tokens": x.Query.Tokens, "text": nilIfEmpty(x.Query.Text)}),
		"budget": sub("budget", map[string]any{"mode": nilIfEmpty(x.Budget.Mode), "tokens": x.Budget.Tokens, "entry": nilIfEmpty(x.Budget.Entry),
			"cause": nilIfEmpty(x.Budget.Cause)}),
		"scope": scope, "warnings": x.Warnings, "passages": passages, "omitted": omitted, "pending_items": pending,
		"search":    sub("search", map[string]any{"plan": nilIfEmpty(x.Search.Plan), "candidates": x.Search.Candidates, "fallback": x.Search.Fallback}),
		"truncated": truncated}
}

func normChunkPage(p *dn.ChunkPage) any {
	chunks := []any{}
	for _, ch := range p.Chunks {
		chunks = append(chunks, map[string]any{"chunk": ch.Chunk, "heading": nilIfEmpty(ch.Heading), "byte_start": ch.ByteStart, "byte_end": ch.ByteEnd,
			"page_start": intOrNil(ch.PageStart), "page_end": intOrNil(ch.PageEnd), "tokens": ch.Tokens, "text": ch.Text})
	}
	var next any
	if p.NextOffset != nil {
		next = *p.NextOffset
	}
	return canon(map[string]any{"chunks": chunks, "offset": p.Offset, "limit": p.Limit, "total": p.Total, "next_offset": next})
}

// normDocumentFields adds a text item's fields to an item's normal form.
func normDocumentFields(it *dn.CollectionItem, m map[string]any) {
	if it.Collection == "" && it.Raw["collection"] == nil {
		delete(m, "collection")
	}
	var file, progress, text any
	if it.File != nil {
		file = map[string]any{"filename": it.File.Filename, "content_type": it.File.ContentType, "byte_size": it.File.ByteSize,
			"pages": intOrNil(it.File.Pages), "original_held": it.File.OriginalHeld}
	}
	if it.Progress != nil {
		progress = map[string]any{"chunks_embedded": it.Progress.ChunksEmbedded, "chunks_total": it.Progress.ChunksTotal, "pages": intOrNil(it.Progress.Pages)}
	}
	if it.Text != nil {
		text = *it.Text
	}
	for k, v := range map[string]any{"title": nilIfEmpty(it.Title), "format": nilIfEmpty(it.Format), "pinned": it.Pinned, "pin_rank": it.PinRank,
		"source": nilIfEmpty(it.Source), "file": file, "status": nilIfEmpty(it.Status), "stage": nilIfEmpty(it.Stage), "progress": progress,
		"serving": nilIfEmpty(it.Serving), "error": nilIfEmpty(it.Error), "delayed": it.Delayed, "warnings": it.Warnings, "chunks": it.Chunks,
		"tokens": it.Tokens, "content_sha256": nilIfEmpty(it.ContentSHA256), "version": it.Version, "queued_at": nilIfEmpty(it.QueuedAt),
		"ready_at": nilIfEmpty(it.ReadyAt), "text": text} {
		if _, sent := it.Raw[k]; sent {
			m[k] = v
		}
	}
}

// normTextCollection adds a text collection's fields to its normal form.
func normTextCollection(c *dn.Collection, m map[string]any) {
	var counts, pinned any
	if c.StatusCounts != nil {
		counts = map[string]any{"queued": c.StatusCounts.Queued, "processing": c.StatusCounts.Processing, "ready": c.StatusCounts.Ready,
			"failed": c.StatusCounts.Failed}
	}
	if c.Pinned != nil {
		var largest any
		if c.Pinned.LargestScope != nil {
			largest = map[string]any{"value": c.Pinned.LargestScope.Value, "tokens": c.Pinned.LargestScope.Tokens}
		}
		pinned = map[string]any{"tokens": c.Pinned.Tokens, "cap": c.Pinned.Cap, "largest_scope": largest}
	}
	for k, v := range map[string]any{"scope_key": nilIfEmpty(c.ScopeKey), "chunks": c.Chunks, "tokens": c.Tokens, "version": c.Version,
		"status_counts": counts, "pinned": pinned} {
		if _, sent := c.Raw[k]; sent {
			m[k] = v
		}
	}
}

// checkDistinctHeader checks that exactly count requests carry the header,
// each with its own value.
func checkDistinctHeader(spec map[string]any, log []mockRecord) error {
	name := strings.ToLower(spec["name"].(string))
	seen := map[string]bool{}
	n := 0
	for _, r := range log {
		if suffix, ok := spec["path_suffix"].(string); ok && !strings.HasSuffix(r.Path, suffix) {
			continue
		}
		if method, ok := spec["method"].(string); ok && r.Method != method {
			continue
		}
		v, present := r.Headers[name]
		if !present || seen[v] {
			return fmt.Errorf("distinct_header %s: a repeated or missing value on %s %s", name, r.Method, r.Path)
		}
		seen[v] = true
		n++
	}
	if want := int(spec["count"].(float64)); n != want {
		return fmt.Errorf("distinct_header %s: expected %d requests, got %d", name, want, n)
	}
	return nil
}
