package decisionnode

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is what the storage checks; it is not used for security.
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The limits of a document, checked locally before any request.
const (
	// MaxDocumentBytes is the largest file a document may be: 50 MB.
	MaxDocumentBytes = 52_428_800
	// MaxTitleChars is the longest title a document may have, in characters
	// (code points), once trimmed.
	MaxTitleChars = 200
	// MaxPinRank is the highest pin rank.
	MaxPinRank = 1000
	// DefaultDocumentUploadTimeout is the time allowed for each attempt of a
	// document's PUT to storage, unless WithUploadTimeout was given.
	DefaultDocumentUploadTimeout = 120 * time.Second
	// WaitTimeout is the time a call with Wait is allowed for each attempt,
	// unless WithTimeout was given: the server holds it up to 10 s.
	WaitTimeout = 20 * time.Second
	// DefaultReadyTimeout is how long WaitUntilReady waits by default.
	DefaultReadyTimeout = 300 * time.Second
	// DefaultReadyPollInterval is WaitUntilReady's first wait between reads; each
	// later wait is 1.5 times the last, up to MaxReadyPollInterval.
	DefaultReadyPollInterval = time.Second
	// MinReadyPollInterval is the shortest poll interval WaitUntilReady takes.
	MinReadyPollInterval = 100 * time.Millisecond
	// MaxReadyPollInterval is the longest wait between WaitUntilReady's reads,
	// unless a Retry-After asks for longer.
	MaxReadyPollInterval = 10 * time.Second
)

// The document formats of a text, and the item statuses.
const (
	FormatMarkdown = "markdown"
	FormatText     = "text"

	StatusQueued     = "queued"
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// KnowledgeContentTypes are the file types a document may be, in the order
// the API lists them.
var KnowledgeContentTypes = []string{
	"text/plain",
	"text/markdown",
	"application/pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"text/html",
	"image/png",
	"image/jpeg",
	"image/tiff",
	"image/webp",
}

// contentTypeByExt is the type of a file name's extension, lower case.
var contentTypeByExt = map[string]string{
	".txt":      "text/plain",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".pdf":      "application/pdf",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".html":     "text/html",
	".htm":      "text/html",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".tif":      "image/tiff",
	".tiff":     "image/tiff",
	".webp":     "image/webp",
}

// Document is the content of one document of a text collection: a text held
// in memory, a local file, a file's bytes or a reader, or an upload made
// before with UploadDocuments. Make one with DocumentText, DocumentFile,
// DocumentData, DocumentReader or DocumentUpload.
//
// A text or Markdown file under the client's inline threshold that is UTF-8
// with no NUL is sent as text, in one call; every other file is uploaded
// first and named by its upload_id. A file's type is its content type when
// given, else its file name's extension, else its first bytes (PDF, PNG,
// JPEG, TIFF, WebP); a DOCX, PPTX or XLSX and a text file need a content type
// or a file name.
type Document struct {
	kind        docKind
	text        string
	format      string
	path        string
	data        []byte
	reader      io.Reader
	uploadID    string
	contentType string
	filename    string
}

type docKind int

const (
	docNone docKind = iota
	docText
	docPath
	docData
	docReader
	docUpload
)

// DocumentText is a text held in memory, sent as it is whatever its size. A
// format of "" is not sent, so the API's default, Markdown, applies;
// FormatMarkdown or FormatText is sent as given. Its title must be given.
func DocumentText(text, format string) Document {
	return Document{kind: docText, text: text, format: format}
}

// DocumentFile is a local file, read when the call is made. contentType and
// filename may be empty: the file name is the path's base name, and the type
// is taken from it.
func DocumentFile(path, contentType, filename string) Document {
	return Document{kind: docPath, path: path, contentType: contentType, filename: filename}
}

// DocumentData is a file's bytes. contentType and filename may be empty. The
// slice is not copied: do not change it while a call that uses it runs.
func DocumentData(data []byte, contentType, filename string) Document {
	return Document{kind: docData, data: data, contentType: contentType, filename: filename}
}

// DocumentReader is a file read from r when the call is made, to its end,
// into memory, before anything is sent (its size and checksums go first).
// Reading stops past MaxDocumentBytes, and the call is then refused as over
// the limit. A Document made with it is used once.
func DocumentReader(r io.Reader, contentType, filename string) Document {
	return Document{kind: docReader, reader: r, contentType: contentType, filename: filename}
}

// DocumentUpload names a knowledge upload made before with UploadDocuments,
// sent as given. An upload not named within its hour expires.
func DocumentUpload(uploadID string) Document {
	return Document{kind: docUpload, uploadID: uploadID}
}

// DocumentOptions are a document's fields on AddDocument and
// ReplaceDocument; a nil field is not sent.
type DocumentOptions struct {
	// Title is 1 to 200 characters once trimmed. Required for a text; a
	// file's default is its file name without its last extension.
	Title *string
	// Reference is the caller's own key for the document, unique in the
	// collection: 1 to 64 characters of A-Z, a-z, 0-9, _ . : and -.
	Reference *string
	// Pinned makes the document apply to every decision that names the
	// collection (in its scope), whatever the question: for a rule that
	// must always apply.
	Pinned *bool
	// PinRank orders pinned documents, 0 to 1,000.
	PinRank *int
	// Label is at most 128 characters.
	Label *string
	// Metadata is the document's own keys and values, as an item's. A
	// scoped collection's scope value is its scope key here; without it,
	// the document is global.
	Metadata map[string]any
	// Wait asks the server to hold the call up to 10 s until the document
	// is ready, sent as ?wait=true. The item then comes back ready, or as
	// it stands. Each attempt is allowed WaitTimeout unless WithTimeout was
	// given.
	Wait bool
}

// docFile is a file document's bytes, type and name, read at the call.
type docFile struct {
	data        []byte
	contentType string
	filename    string
}

// AddDocument adds a document to a text collection with POST
// /v1/collections/{id}/items and returns the item: queued (202), or with
// Wait ready (201) or as it stands after the server's 10 s. Ingestion is
// asynchronous: WaitUntilReady waits for it. A file is uploaded first unless
// it goes as text; the upload is consumed by the item.
//
// The fields are checked locally as the API checks them (a
// *ValidationError), and a file's size and type (a *BadRequestError), before
// anything is sent. The call carries an Idempotency-Key, new for each call
// and the same on every attempt, and is retried like Create. When an upload
// this call made went stale before the item was made, the file is uploaded
// again and the call sent once more, with a new key.
func (s *CollectionService) AddDocument(ctx context.Context, collectionID string, doc Document, o DocumentOptions) (*CollectionItem, error) {
	return s.sendDocument(ctx, http.MethodPost, collectionPath(collectionID)+"/items", doc, o)
}

// ReplaceDocument replaces a document whole with PUT
// /v1/collections/{id}/items/{item_id}: a new version, the old one serving
// until it is ready (202), or nothing to do when the text is the same (200).
// It is sent, checked and retried as AddDocument. A text for a file's
// document, or a file for a text's, is the API's 409 (not_an_article,
// not_a_file).
func (s *CollectionService) ReplaceDocument(ctx context.Context, collectionID, itemID string, doc Document, o DocumentOptions) (*CollectionItem, error) {
	return s.sendDocument(ctx, http.MethodPut, collectionPath(collectionID)+"/items/"+url.PathEscape(itemID), doc, o)
}

// CheckDocument runs every local check AddDocument and ReplaceDocument would
// run on doc and o, and returns the same error, sending and uploading
// nothing; nil means the call would send it. A file is read to be checked,
// so a Document made with DocumentReader is used up by it.
func (s *CollectionService) CheckDocument(doc Document, o DocumentOptions) error {
	if s.c.err != nil {
		return s.c.err
	}
	_, err := s.c.prepareDocument(doc, o)
	return err
}

// preparedDocument is a checked document: its text when it goes inline, else
// its file to upload or the caller's upload id, and its title.
type preparedDocument struct {
	inline       bool
	text, format string
	file         *docFile
	title        *string
}

// prepareDocument reads a document and runs the local checks of its content
// and fields.
func (c *Client) prepareDocument(doc Document, o DocumentOptions) (*preparedDocument, error) {
	var file *docFile
	inline := false
	text, format := "", ""
	switch doc.kind {
	case docText:
		inline, text, format = true, doc.text, doc.format
	case docUpload:
	case docNone:
		return nil, errors.New("decisionnode: the document is empty; make one with DocumentText, DocumentFile, DocumentData, DocumentReader or DocumentUpload")
	default:
		f, err := readDocument(doc, 0)
		if err != nil {
			return nil, err
		}
		file = f
		if t, ok := c.inlineText(f); ok {
			inline, text = true, t
			format = FormatText
			if f.contentType == "text/markdown" {
				format = FormatMarkdown
			}
		}
	}
	title := o.Title
	if inline && title == nil && file != nil && file.filename != "" {
		stem := fileStem(file.filename)
		title = &stem
	}
	entries, reason := documentEntries(o, title)
	if inline && title == nil {
		entries = append([]any{map[string]any{"type": "missing", "loc": []any{"body", "title"}, "msg": "Field required", "input": nil}}, entries...)
		reason = "title: Field required"
	}
	if doc.kind == docText {
		if doc.format != "" && doc.format != FormatMarkdown && doc.format != FormatText {
			entries = append(entries, map[string]any{"type": "literal_error", "loc": []any{"body", "format"}, "msg": "Input should be 'markdown' or 'text'",
				"input": echoed(doc.format)})
			if reason == "" {
				reason = "format must be markdown or text"
			}
		}
		if doc.text == "" {
			entries = append(entries, map[string]any{"type": "string_too_short", "loc": []any{"body", "text"}, "msg": "The text is empty", "input": ""})
			if reason == "" {
				reason = "the text is empty"
			}
		}
	}
	if len(entries) > 0 {
		return nil, localValidation(reason, entries...)
	}
	return &preparedDocument{inline: inline, text: text, format: format, file: file, title: title}, nil
}

func (s *CollectionService) sendDocument(ctx context.Context, method, path string, doc Document, o DocumentOptions) (*CollectionItem, error) {
	c := s.c
	if c.err != nil {
		return nil, c.err
	}
	pd, err := c.prepareDocument(doc, o)
	if err != nil {
		return nil, err
	}
	inline, text, format, file, title := pd.inline, pd.text, pd.format, pd.file, pd.title
	query := ""
	opts := callOptions{policy: retryIdempotent}
	if o.Wait {
		query = "?wait=true"
		if !c.timeoutSet {
			opts.timeout = WaitTimeout
		}
	}
	uploadID := doc.uploadID
	for resend := 0; ; resend++ {
		placed := false
		if !inline && file != nil {
			ups, err := c.uploadDocuments(ctx, []*docFile{file})
			if err != nil {
				return nil, err
			}
			uploadID, placed = ups[0].ID, true
		}
		var buf bytes.Buffer
		buf.WriteByte('{')
		member := func(name string, v any) error {
			if buf.Len() > 1 {
				buf.WriteByte(',')
			}
			return writeMember(&buf, name, v)
		}
		var err error
		if inline {
			if err = member("title", *title); err == nil {
				err = member("text", text)
			}
			if err == nil && format != "" {
				err = member("format", format)
			}
		} else {
			err = member("upload_id", uploadID)
			if err == nil && title != nil {
				err = member("title", *title)
			}
		}
		if err == nil {
			err = documentFields(member, o)
		}
		if err != nil {
			return nil, encodeError(err)
		}
		buf.WriteByte('}')
		if err := checkBodySize(buf.Len()); err != nil {
			return nil, err
		}
		// A resend after a stale upload is another body, so it has its own key.
		key, err := newIdempotencyKey()
		if err != nil {
			return nil, err
		}
		opts.idempotencyKey = key
		resp, err := c.call(ctx, method, path+query, buf.Bytes(), opts)
		if err != nil && resend == 0 && placed && staleUpload(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		item, err := parseItem(resp.data)
		if err != nil {
			return nil, invalidResponse(resp, err)
		}
		return item, nil
	}
}

// documentFields writes a document's fields after its content, each only
// when given.
func documentFields(member func(string, any) error, o DocumentOptions) error {
	if o.Reference != nil {
		if err := member("reference", *o.Reference); err != nil {
			return err
		}
	}
	if o.Pinned != nil {
		if err := member("pinned", *o.Pinned); err != nil {
			return err
		}
	}
	if o.PinRank != nil {
		if err := member("pin_rank", *o.PinRank); err != nil {
			return err
		}
	}
	if o.Label != nil {
		if err := member("label", *o.Label); err != nil {
			return err
		}
	}
	if o.Metadata != nil {
		if err := member("metadata", o.Metadata); err != nil {
			return err
		}
	}
	return nil
}

// documentEntries are the API's 422 entries for a document's fields, with
// the reason of the first.
func documentEntries(o DocumentOptions, title *string) ([]any, string) {
	var entries []any
	reason := ""
	add := func(e map[string]any, why string) {
		entries = append(entries, e)
		if reason == "" {
			reason = why
		}
	}
	if title != nil {
		if e := titleEntry(*title); e != nil {
			add(e, fmt.Sprintf("title must be 1 to %d characters", MaxTitleChars))
		}
	}
	if o.Reference != nil {
		if e := textReferenceEntry(*o.Reference); e != nil {
			add(e, fmt.Sprintf("reference must be 1 to %d characters of A-Z, a-z, 0-9, _ . : and -", MaxItemReferenceChars))
		}
	}
	if o.PinRank != nil {
		if e := pinRankEntry(*o.PinRank); e != nil {
			add(e, fmt.Sprintf("pin_rank must be an integer from 0 to %d", MaxPinRank))
		}
	}
	if o.Label != nil {
		if e := textLabelEntry(*o.Label); e != nil {
			add(e, fmt.Sprintf("label must be 1 to %d characters", MaxItemLabelChars))
		}
	}
	if o.Metadata != nil {
		if ms := metadataEntries(o.Metadata); len(ms) > 0 {
			entries = append(entries, ms...)
			if reason == "" {
				reason = "metadata: " + ms[0].(map[string]any)["msg"].(string)
			}
		}
	}
	return entries, reason
}

// titleEntry is the API's entry for a title that, trimmed, is not 1 to 200
// characters; nil when it is.
func titleEntry(title string) map[string]any {
	n := utf8.RuneCountInString(strings.TrimSpace(title))
	if n >= 1 && n <= MaxTitleChars {
		return nil
	}
	return map[string]any{"type": "string_too_long", "loc": []any{"body", "title"}, "msg": "A title is 1 to 200 characters", "input": echoed(title)}
}

// textReferenceEntry is a text item's entry for a reference not of its
// pattern; nil when it is.
func textReferenceEntry(ref string) map[string]any {
	if itemReference.MatchString(ref) {
		return nil
	}
	return map[string]any{"type": "string_pattern_mismatch", "loc": []any{"body", "reference"},
		"msg": fmt.Sprintf("String should match pattern '%s'", itemReferencePattern), "input": echoed(ref)}
}

// textLabelEntry is a text item's entry for a label empty or over 128
// characters; nil when it is neither.
func textLabelEntry(label string) map[string]any {
	n := utf8.RuneCountInString(label)
	if n >= 1 && n <= MaxItemLabelChars {
		return nil
	}
	return map[string]any{"type": "string_too_long", "loc": []any{"body", "label"},
		"msg": "A label is 1 to 128 characters with no control or invisible characters", "input": echoed(label)}
}

func pinRankEntry(rank int) map[string]any {
	if rank >= 0 && rank <= MaxPinRank {
		return nil
	}
	return map[string]any{"type": "int_range", "loc": []any{"body", "pin_rank"}, "msg": "Input should be an integer from 0 to 1000", "input": rank}
}

// readDocument reads a file document's bytes and settles its type and file
// name, refusing locally what the presign would refuse: over 50 MB, empty,
// or of a type that is not a knowledge type or cannot be told. i is the
// file's place in the presign, for the message.
func readDocument(doc Document, i int) (*docFile, error) {
	f := &docFile{contentType: doc.contentType, filename: doc.filename}
	switch doc.kind {
	case docText:
		f.data = []byte(doc.text)
		if f.contentType == "" {
			f.contentType = "text/markdown"
			if doc.format == FormatText {
				f.contentType = "text/plain"
			}
		}
	case docPath:
		r, err := os.Open(doc.path)
		if err != nil {
			return nil, fmt.Errorf("decisionnode: document %q: %w", doc.path, err)
		}
		defer func() { _ = r.Close() }()
		if f.data, err = readLimited(r); err != nil {
			return nil, fmt.Errorf("decisionnode: document %q: %w", doc.path, err)
		}
		if f.filename == "" {
			f.filename = filepath.Base(doc.path)
		}
	case docData:
		f.data = doc.data
	case docReader:
		if doc.reader == nil {
			return nil, errors.New("decisionnode: the document's reader is nil")
		}
		var err error
		if f.data, err = readLimited(doc.reader); err != nil {
			return nil, fmt.Errorf("decisionnode: document: %w", err)
		}
	default:
		return nil, errors.New("decisionnode: the document has no content")
	}
	files := fmt.Sprintf("files[%d]", i)
	switch {
	case len(f.data) > MaxDocumentBytes:
		msg := fmt.Sprintf("%s.byte_size: %d is over the limit of %d bytes.", files, len(f.data), MaxDocumentBytes)
		return nil, localBadRequest("api_usage_error", msg, msg)
	case len(f.data) == 0:
		msg := files + ".byte_size: must be at least 1."
		return nil, localBadRequest("api_usage_error", msg, msg)
	}
	if f.contentType == "" && f.filename != "" {
		f.contentType = contentTypeByExt[strings.ToLower(path.Ext(f.filename))]
	}
	if f.contentType == "" {
		f.contentType = sniffDocument(f.data)
	}
	if !knowledgeType(f.contentType) {
		msg := files + ".content_type: must be one of " + strings.Join(KnowledgeContentTypes, ", ") + "."
		return nil, localBadRequest("api_usage_error", msg, msg)
	}
	return f, nil
}

// readLimited reads r to its end, stopping one byte past MaxDocumentBytes.
func readLimited(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(r, MaxDocumentBytes+1)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func knowledgeType(t string) bool {
	for _, k := range KnowledgeContentTypes {
		if t == k {
			return true
		}
	}
	return false
}

// sniffDocument is a file's type from its first bytes, for the types that
// can be told that way; "" for any other.
func sniffDocument(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("II*\x00")), bytes.HasPrefix(b, []byte("MM\x00*")):
		return "image/tiff"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// inlineText is a text or Markdown file's text when it goes inline: under
// the inline threshold, UTF-8 and with no NUL.
func (c *Client) inlineText(f *docFile) (string, bool) {
	if f.contentType != "text/plain" && f.contentType != "text/markdown" {
		return "", false
	}
	if len(f.data) >= c.inlineThreshold || !utf8.Valid(f.data) || bytes.IndexByte(f.data, 0) >= 0 {
		return "", false
	}
	return string(f.data), true
}

// fileStem is a file name without its last extension.
func fileStem(name string) string {
	return strings.TrimSuffix(name, path.Ext(name))
}

// UploadDocuments uploads 1 to 16 documents as files, whatever their type
// and size (a text as text/markdown, or text/plain for FormatText), and
// returns each one's upload, in order: name it later with DocumentUpload. An
// upload not named within its hour expires. A Document made with
// DocumentUpload returns its own id with a zero ExpiresAt. The documents are
// presigned in one POST /v1/uploads, then sent in parallel. A knowledge
// upload needs a full-scope key.
func (c *Client) UploadDocuments(ctx context.Context, docs []Document) ([]UploadResult, error) {
	if c.err != nil {
		return nil, c.err
	}
	if len(docs) == 0 || len(docs) > maxUploadFiles {
		msg := fmt.Sprintf("files: 1 to %d files, got %d.", maxUploadFiles, len(docs))
		return nil, localBadRequest("api_usage_error", msg, msg)
	}
	out := make([]UploadResult, len(docs))
	var files []*docFile
	var where []int
	for i, doc := range docs {
		if doc.kind == docUpload {
			out[i] = UploadResult{ID: doc.uploadID}
			continue
		}
		f, err := readDocument(doc, len(files))
		if err != nil {
			return nil, err
		}
		files = append(files, f)
		where = append(where, i)
	}
	if len(files) == 0 {
		return out, nil
	}
	ups, err := c.uploadDocuments(ctx, files)
	if err != nil {
		return nil, err
	}
	for j, i := range where {
		out[i] = ups[j]
	}
	return out, nil
}

// uploadDocuments presigns the files with the knowledge purpose in one call
// and PUTs them in parallel, each attempt allowed the document upload
// timeout. A PUT refused 403 after its URL's lifetime is presigned again
// once. Nothing is cached: an upload is consumed by the item it makes.
func (c *Client) uploadDocuments(ctx context.Context, files []*docFile) ([]UploadResult, error) {
	timeout := DefaultDocumentUploadTimeout
	if c.uploadTimeoutSet {
		timeout = c.uploadTimeout
	}
	ups, urlsExpire, err := c.presignDocuments(ctx, files)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(files))
	var wg sync.WaitGroup
	for i := range files {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := c.put(pctx, ups[i], files[i].data, timeout)
			var e *Error
			if errors.As(err, &e) && e.Status == http.StatusForbidden && !c.now().Before(urlsExpire) {
				var again []presigned
				if again, _, err = c.presignDocuments(pctx, files[i:i+1]); err == nil {
					ups[i] = again[0]
					err = c.put(pctx, ups[i], files[i].data, timeout)
				}
			}
			if errs[i] = err; err != nil {
				cancel()
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil && !(isCanceled(e) && ctx.Err() == nil) {
			return nil, e
		}
	}
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	out := make([]UploadResult, len(files))
	for i, up := range ups {
		out[i] = UploadResult{ID: up.UploadID}
		if t, perr := time.Parse(time.RFC3339, up.ExpiresAt); perr == nil {
			out[i].ExpiresAt = t
		}
	}
	return out, nil
}

func (c *Client) presignDocuments(ctx context.Context, files []*docFile) ([]presigned, time.Time, error) {
	type file struct {
		Purpose     string `json:"purpose"`
		ContentType string `json:"content_type"`
		ByteSize    int    `json:"byte_size"`
		Checksum    string `json:"checksum"`
		SHA256      string `json:"sha256"`
		Filename    string `json:"filename,omitempty"`
	}
	list := make([]file, len(files))
	for i, f := range files {
		md := md5.Sum(f.data) //nolint:gosec // see the import
		sum := sha256.Sum256(f.data)
		list[i] = file{
			Purpose:     "knowledge",
			ContentType: f.contentType,
			ByteSize:    len(f.data),
			Checksum:    base64.StdEncoding.EncodeToString(md[:]),
			SHA256:      base64.StdEncoding.EncodeToString(sum[:]),
			Filename:    f.filename,
		}
	}
	payload, err := marshal(map[string]any{"files": list})
	if err != nil {
		return nil, time.Time{}, encodeError(err)
	}
	return c.presignFiles(ctx, payload, len(files))
}

// GetItem reads one item with GET /v1/collections/{id}/items/{item_id}; with
// includeText, a document's stored text too (?include=text), nil while no
// version serves. Including the text needs a full-scope key.
func (s *CollectionService) GetItem(ctx context.Context, collectionID, itemID string, includeText bool) (*CollectionItem, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	p := collectionPath(collectionID) + "/items/" + url.PathEscape(itemID)
	if includeText {
		p += "?include=text"
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	item, err := parseItem(resp.data)
	if err != nil {
		return nil, invalidResponse(resp, err)
	}
	return item, nil
}

// Chunk is one passage of a document as it is served: its number, heading,
// UTF-8 byte offsets into the stored text (end exclusive), pages, tokens and
// text, each as sent.
type Chunk struct {
	Chunk int
	// Heading is the heading the chunk is under; "" when none.
	Heading   string
	ByteStart int
	ByteEnd   int
	// PageStart and PageEnd are a paged file's pages; nil otherwise.
	PageStart *int
	PageEnd   *int
	Tokens    int
	Text      string
	Raw       map[string]any
}

// ChunkPage is one page of ListChunks.
type ChunkPage struct {
	Chunks []Chunk
	Offset int
	Limit  int
	Total  int
	// NextOffset is the offset of the next page, nil on the last.
	NextOffset *int
	Raw        map[string]any
}

// ChunkListOptions pages ListChunks, as ItemListOptions pages ListItems.
type ChunkListOptions struct {
	Offset *int
	Limit  *int
}

// ListChunks reads one page of the chunks a document is served as, with GET
// /v1/collections/{id}/items/{item_id}/chunks: for seeing how a document was
// cut. Offset and Limit are checked as ListItems checks them.
func (s *CollectionService) ListChunks(ctx context.Context, collectionID, itemID string, opts ChunkListOptions) (*ChunkPage, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	query, err := pageQuery(opts.Offset, opts.Limit)
	if err != nil {
		return nil, err
	}
	resp, err := s.c.doPayload(ctx, http.MethodGet, collectionPath(collectionID)+"/items/"+url.PathEscape(itemID)+"/chunks"+query, nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	page := &ChunkPage{Chunks: []Chunk{}, Raw: raw}
	page.Offset, _ = wholeNumber(raw["offset"])
	page.Limit, _ = wholeNumber(raw["limit"])
	page.Total, _ = wholeNumber(raw["total"])
	if n, ok := wholeNumber(raw["next_offset"]); ok {
		page.NextOffset = &n
	}
	list, _ := raw["chunks"].([]any)
	for _, v := range list {
		m, _ := v.(map[string]any)
		ch := Chunk{Raw: m}
		ch.Chunk, _ = wholeNumber(m["chunk"])
		ch.Heading, _ = m["heading"].(string)
		ch.ByteStart, _ = wholeNumber(m["byte_start"])
		ch.ByteEnd, _ = wholeNumber(m["byte_end"])
		if n, ok := wholeNumber(m["page_start"]); ok {
			ch.PageStart = &n
		}
		if n, ok := wholeNumber(m["page_end"]); ok {
			ch.PageEnd = &n
		}
		ch.Tokens, _ = wholeNumber(m["tokens"])
		ch.Text, _ = m["text"].(string)
		page.Chunks = append(page.Chunks, ch)
	}
	return page, nil
}

// FailedDeleted is the answer to DeleteFailedItems: how many failed
// documents were deleted.
type FailedDeleted struct {
	Deleted int
	Raw     map[string]any
}

// DeleteFailedItems deletes every failed document of a text collection with
// DELETE /v1/collections/{id}/items?status=failed. A failed document counts
// toward nothing, but stays visible with its Error until it is deleted.
func (s *CollectionService) DeleteFailedItems(ctx context.Context, collectionID string) (*FailedDeleted, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	resp, err := s.c.doPayload(ctx, http.MethodDelete, collectionPath(collectionID)+"/items?status=failed", nil)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	d := &FailedDeleted{Raw: raw}
	d.Deleted, _ = wholeNumber(raw["deleted"])
	return d, nil
}

// WaitOptions are WaitUntilReady's settings; a zero field takes its default.
type WaitOptions struct {
	// Timeout is how long to wait in all (DefaultReadyTimeout, 300 s). It
	// must not be negative.
	Timeout time.Duration
	// PollInterval is the first wait between reads (DefaultReadyPollInterval,
	// 1 s), at least MinReadyPollInterval; each later wait is 1.5 times the
	// last, up to MaxReadyPollInterval.
	PollInterval time.Duration
}

// WaitUntilReady reads a document with GET
// /v1/collections/{id}/items/{item_id} until its status is ready or failed,
// and returns it either way: a failed document is a value, its Error saying
// why. A status this version does not know is waited on. The first read is
// made at once; the waits between reads grow from the poll interval, or
// follow a 429's or 529's Retry-After when that is longer, and each read is
// retried as any call. Past the timeout it returns a *TimeoutError, and when
// ctx ends, ctx's error.
func (s *CollectionService) WaitUntilReady(ctx context.Context, collectionID, itemID string, o WaitOptions) (*CollectionItem, error) {
	var item *CollectionItem
	p := collectionPath(collectionID) + "/items/" + url.PathEscape(itemID)
	err := s.poll(ctx, p, o, func(data []byte) (bool, error) {
		it, err := parseItem(data)
		if err != nil {
			return false, err
		}
		item = it
		return it.Status == StatusReady || it.Status == StatusFailed, nil
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}

// WaitUntilCollectionReady reads a text collection with GET
// /v1/collections/{id} until none of its documents is queued or processing,
// and returns it, waiting as WaitUntilReady does.
func (s *CollectionService) WaitUntilCollectionReady(ctx context.Context, collectionID string, o WaitOptions) (*Collection, error) {
	var col *Collection
	err := s.poll(ctx, collectionPath(collectionID), o, func(data []byte) (bool, error) {
		cl, err := parseCollection(data)
		if err != nil {
			return false, err
		}
		col = cl
		sc := cl.StatusCounts
		return sc == nil || (sc.Queued == 0 && sc.Processing == 0), nil
	})
	if err != nil {
		return nil, err
	}
	return col, nil
}

// poll reads path until done says so, waiting between reads as
// WaitUntilReady documents.
func (s *CollectionService) poll(ctx context.Context, p string, o WaitOptions, done func([]byte) (bool, error)) error {
	c := s.c
	if o.Timeout < 0 {
		return fmt.Errorf("decisionnode: the timeout must be above 0, got %s", o.Timeout)
	}
	if o.PollInterval < 0 || (o.PollInterval > 0 && o.PollInterval < MinReadyPollInterval) {
		return fmt.Errorf("decisionnode: the poll interval must be at least %s, got %s", MinReadyPollInterval, o.PollInterval)
	}
	if c.err != nil {
		return c.err
	}
	timeout, interval := o.Timeout, o.PollInterval
	if timeout == 0 {
		timeout = DefaultReadyTimeout
	}
	if interval == 0 {
		interval = DefaultReadyPollInterval
	}
	start := time.Now()
	var slept time.Duration
	for {
		var hinted time.Duration
		resp, err := c.call(ctx, http.MethodGet, p, nil, callOptions{retryAfterSeen: &hinted})
		if err != nil {
			return err
		}
		ok, err := done(resp.data)
		if err != nil {
			return invalidResponse(resp, err)
		}
		if ok {
			return nil
		}
		wait := interval
		if hinted > wait {
			wait = hinted
		}
		// The waits are counted as well as the clock, so a sleep that
		// returns early still ends the wait.
		elapsed := max(time.Since(start), slept)
		if elapsed+wait > timeout {
			return &TimeoutError{&Error{msg: fmt.Sprintf("not ready after %s", timeout)}}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return &Error{msg: "the request was canceled", Err: err}
		}
		slept += wait
		interval = min(time.Duration(float64(interval)*1.5), MaxReadyPollInterval)
	}
}

// SearchOptions are a search's settings; a nil field is not sent and the
// server's default applies. They are sent as given and checked by the API.
type SearchOptions struct {
	// Scope is the value of the collection's scope key to search in.
	Scope *string
	// Match is metadata equality, such as {"region": "SE"}: any JSON value,
	// passed through.
	Match any
	// Filter is a typed filter tree: any JSON value, passed through.
	Filter any
	// TopK is the most passages found, pins not counted (0 to 20).
	TopK *int
	// Pinned, set to false, leaves the pinned documents out.
	Pinned *bool
	// MaxTokens is the passages' token budget: an integer, or "auto".
	MaxTokens any
}

// Search searches a text collection with POST /v1/collections/{id}/search,
// for trying a query without a decision, and returns the reading a decision
// with the knowledge booster would get, every passage with its text. It is
// billed as one knowledge call and needs a full-scope key. Nothing in it is
// checked locally.
func (s *CollectionService) Search(ctx context.Context, collectionID, query string, o SearchOptions) (*KnowledgeReading, error) {
	if s.c.err != nil {
		return nil, s.c.err
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	if err := writeMember(&buf, "query", query); err != nil {
		return nil, encodeError(err)
	}
	for _, f := range []struct {
		name  string
		value any
		set   bool
	}{
		{"scope", o.Scope, o.Scope != nil},
		{"match", o.Match, o.Match != nil},
		{"filter", o.Filter, o.Filter != nil},
		{"top_k", o.TopK, o.TopK != nil},
		{"pinned", o.Pinned, o.Pinned != nil},
		{"max_tokens", o.MaxTokens, o.MaxTokens != nil},
	} {
		if !f.set {
			continue
		}
		buf.WriteByte(',')
		if err := writeMember(&buf, f.name, f.value); err != nil {
			return nil, encodeError(err)
		}
	}
	buf.WriteByte('}')
	resp, err := s.c.call(ctx, http.MethodPost, collectionPath(collectionID)+"/search", buf.Bytes(), callOptions{})
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(resp.data, &raw); err != nil {
		return nil, invalidResponse(resp, err)
	}
	r, ok := parseKnowledge(raw)
	if !ok {
		return nil, invalidResponse(resp, errors.New("the answer is not a knowledge reading"))
	}
	return r, nil
}

// BoosterKnowledge retrieves passages from a text collection of the
// workspace's own documents for the decision: see KnowledgeBooster.
const BoosterKnowledge = "knowledge"

// KnowledgeOptions are the knowledge booster's settings beside its
// collection; a nil field is not sent and the server's default applies. They
// are sent as given and checked by the API.
type KnowledgeOptions struct {
	// Query replaces the query built from the request (its questions'
	// instructions, then its state).
	Query *string
	// Scope is the value of the collection's scope key: the documents
	// scoped to it, and the global ones.
	Scope *string
	// Match is metadata equality, such as {"region": "SE"}: any JSON value,
	// passed through.
	Match any
	// Filter is a typed filter tree: any JSON value, passed through.
	Filter any
	// TopK is the most passages found, pins not counted (0 to 20; 0 gives
	// the pinned documents only).
	TopK *int
	// MaxTokens is the passages' token budget: an integer, or "auto".
	MaxTokens any
	// Pinned, set to false, leaves the pinned documents out.
	Pinned *bool
	// MinScore is a similarity floor for passages found by meaning alone,
	// 0 to 1.
	MinScore *float64
	// IncludeText adds each passage's text and the query's to the reading;
	// it needs a full-scope key.
	IncludeText *bool
	// Require makes the request fail when the reading cannot be made: a
	// 529 *OverloadedError while no document is ready yet, or a 409
	// knowledge_collection_empty when there is none. Sent only when true.
	Require bool
}

// KnowledgeBooster is the knowledge booster over a text collection (its col_
// id), with its settings, sent as {"name": "knowledge", "require"?: true,
// "options": {"collection": ..., ...}}. Its reading is a *KnowledgeReading.
// It reads no picture and needs no image in the request.
func KnowledgeBooster(collection string, o KnowledgeOptions) Booster {
	opts := map[string]any{"collection": collection}
	if o.Query != nil {
		opts["query"] = *o.Query
	}
	if o.Scope != nil {
		opts["scope"] = *o.Scope
	}
	if o.Match != nil {
		opts["match"] = o.Match
	}
	if o.Filter != nil {
		opts["filter"] = o.Filter
	}
	if o.TopK != nil {
		opts["top_k"] = *o.TopK
	}
	if o.MaxTokens != nil {
		opts["max_tokens"] = o.MaxTokens
	}
	if o.Pinned != nil {
		opts["pinned"] = *o.Pinned
	}
	if o.MinScore != nil {
		opts["min_score"] = *o.MinScore
	}
	if o.IncludeText != nil {
		opts["include_text"] = *o.IncludeText
	}
	b := Booster{Name: BoosterKnowledge, Options: opts}
	if o.Require {
		b.Require = Ptr(true)
	}
	return b
}

// KnowledgeReading is the knowledge booster's reading, and a search's
// answer: the passages found in the collection, with what was searched and
// why anything was left out. Every field is as sent.
type KnowledgeReading struct {
	readingBase
	Collection string
	// CollectionName is the collection's name; "" when not sent (a
	// publishable key's reading).
	CollectionName string
	// Corpus is the collection's version that was searched.
	Corpus KnowledgeCorpus
	Query  KnowledgeQuery
	Budget KnowledgeBudget
	// Scope is the scope searched; nil when the collection has no scope key.
	Scope *KnowledgeScope
	// Warnings are notes such as "scope_not_named", "over_budget",
	// "budget_floor" or "pinned_filtered"; empty, never nil, when none.
	Warnings []string
	// Passages are in the order the model reads them, pinned first.
	Passages []KnowledgePassage
	// Omitted counts the passages left out, by cause.
	Omitted map[string]int
	// PendingItems is how many documents are not ready yet.
	PendingItems int
	Search       KnowledgeSearch
	// Truncated is true when a passage was dropped to fit the context; false
	// on a search's answer.
	Truncated bool
}

// KnowledgeCorpus is the version of a collection a reading searched.
type KnowledgeCorpus struct {
	Version int
}

// KnowledgeQuery is what was searched for.
type KnowledgeQuery struct {
	// Source is "auto" (built from the request), "request" (the Query
	// option), "none" (TopK 0), or any other value as sent.
	Source string
	Tokens int
	// Text is the query; "" unless the text was included, and on a search.
	Text string
}

// KnowledgeBudget is the passages' token budget.
type KnowledgeBudget struct {
	// Mode is "auto" or "fixed".
	Mode   string
	Tokens int
	// Entry is "short" or "long"; "" when not sent.
	Entry string
	// Cause says what took the room; "" when not sent.
	Cause string
}

// KnowledgeScope is the scope key and the value searched ("" when none was
// named).
type KnowledgeScope struct {
	Key   string
	Value string
}

// KnowledgeSearch is how the search ran.
type KnowledgeSearch struct {
	Plan       string
	Candidates int
	Fallback   bool
}

// KnowledgePassage is one passage of a document. Cite it by Item and Chunk;
// ByteStart and ByteEnd are UTF-8 byte offsets into the document's stored
// text (GetItem with includeText), end exclusive.
type KnowledgePassage struct {
	// N is the passage's number in the block the model reads.
	N         int
	Pinned    bool
	Item      string
	Chunk     int
	ByteStart int
	ByteEnd   int
	// Reference, Title and Heading are "" when not sent (a publishable
	// key's reading).
	Reference string
	Title     string
	Heading   string
	Tokens    int
	// Score is the similarity; nil for a pinned passage.
	Score   *float64
	Lexical bool
	// Rendered is false when the passage was left out to fit; false on a
	// search's passages.
	Rendered bool
	// Text is the passage's text; "" unless the text was included, and on
	// a search.
	Text string
	Raw  map[string]any
}

// parseKnowledge reads a knowledge reading. One with a required field
// missing or of the wrong type, or such a passage, is not one (false).
func parseKnowledge(raw map[string]any) (*KnowledgeReading, bool) {
	collection, ok := raw["collection"].(string)
	if !ok {
		return nil, false
	}
	list, ok := raw["passages"].([]any)
	if !ok {
		return nil, false
	}
	r := &KnowledgeReading{readingBase: readingBase{raw: raw}, Collection: collection, Warnings: []string{}, Passages: []KnowledgePassage{}}
	for _, v := range list {
		p, ok := knowledgePassage(v)
		if !ok {
			return nil, false
		}
		r.Passages = append(r.Passages, p)
	}
	r.CollectionName, _ = raw["collection_name"].(string)
	if m, ok := raw["corpus"].(map[string]any); ok {
		r.Corpus.Version, _ = wholeNumber(m["version"])
	}
	if m, ok := raw["query"].(map[string]any); ok {
		r.Query.Source, _ = m["source"].(string)
		r.Query.Tokens, _ = wholeNumber(m["tokens"])
		r.Query.Text, _ = m["text"].(string)
	}
	if m, ok := raw["budget"].(map[string]any); ok {
		r.Budget.Mode, _ = m["mode"].(string)
		r.Budget.Tokens, _ = wholeNumber(m["tokens"])
		r.Budget.Entry, _ = m["entry"].(string)
		r.Budget.Cause, _ = m["cause"].(string)
	}
	if m, ok := raw["scope"].(map[string]any); ok {
		r.Scope = &KnowledgeScope{}
		r.Scope.Key, _ = m["key"].(string)
		r.Scope.Value, _ = m["value"].(string)
	}
	if ws, ok := raw["warnings"].([]any); ok {
		for _, w := range ws {
			if s, ok := w.(string); ok {
				r.Warnings = append(r.Warnings, s)
			}
		}
	}
	if m, ok := raw["omitted"].(map[string]any); ok {
		r.Omitted = make(map[string]int, len(m))
		for k, v := range m {
			if n, ok := wholeNumber(v); ok {
				r.Omitted[k] = n
			}
		}
	}
	r.PendingItems, _ = wholeNumber(raw["pending_items"])
	if m, ok := raw["search"].(map[string]any); ok {
		r.Search.Plan, _ = m["plan"].(string)
		r.Search.Candidates, _ = wholeNumber(m["candidates"])
		r.Search.Fallback, _ = m["fallback"].(bool)
	}
	r.Truncated, _ = raw["truncated"].(bool)
	return r, true
}

func knowledgePassage(v any) (KnowledgePassage, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return KnowledgePassage{}, false
	}
	p := KnowledgePassage{Raw: m}
	var ok1, ok2, ok3, ok4, ok5, ok6 bool
	p.N, ok1 = wholeNumber(m["n"])
	p.Pinned, ok2 = m["pinned"].(bool)
	p.Item, ok3 = m["item"].(string)
	p.Chunk, ok4 = wholeNumber(m["chunk"])
	p.ByteStart, ok5 = wholeNumber(m["byte_start"])
	p.ByteEnd, ok6 = wholeNumber(m["byte_end"])
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return KnowledgePassage{}, false
	}
	p.Reference, _ = m["reference"].(string)
	p.Title, _ = m["title"].(string)
	p.Heading, _ = m["heading"].(string)
	p.Tokens, _ = wholeNumber(m["tokens"])
	if f, ok := m["score"].(float64); ok {
		p.Score = &f
	}
	p.Lexical, _ = m["lexical"].(bool)
	p.Rendered, _ = m["rendered"].(bool)
	p.Text, _ = m["text"].(string)
	return p, true
}

// readDocumentFields reads a document item's fields into item.
func readDocumentFields(item *CollectionItem, raw map[string]any) {
	item.Title, _ = raw["title"].(string)
	item.Format, _ = raw["format"].(string)
	item.Pinned, _ = raw["pinned"].(bool)
	item.PinRank, _ = wholeNumber(raw["pin_rank"])
	item.Source, _ = raw["source"].(string)
	if m, ok := raw["file"].(map[string]any); ok {
		f := &ItemFile{}
		f.Filename, _ = m["filename"].(string)
		f.ContentType, _ = m["content_type"].(string)
		f.ByteSize, _ = wholeNumber(m["byte_size"])
		if n, ok := wholeNumber(m["pages"]); ok {
			f.Pages = &n
		}
		f.OriginalHeld, _ = m["original_held"].(bool)
		item.File = f
	}
	item.Status, _ = raw["status"].(string)
	item.Stage, _ = raw["stage"].(string)
	if m, ok := raw["progress"].(map[string]any); ok {
		pr := &ItemProgress{}
		pr.ChunksEmbedded, _ = wholeNumber(m["chunks_embedded"])
		pr.ChunksTotal, _ = wholeNumber(m["chunks_total"])
		if n, ok := wholeNumber(m["pages"]); ok {
			pr.Pages = &n
		}
		item.Progress = pr
	}
	item.Serving, _ = raw["serving"].(string)
	item.Error, _ = raw["error"].(string)
	item.Delayed, _ = raw["delayed"].(bool)
	item.Warnings = []string{}
	if ws, ok := raw["warnings"].([]any); ok {
		for _, w := range ws {
			if s, ok := w.(string); ok {
				item.Warnings = append(item.Warnings, s)
			}
		}
	}
	item.Chunks, _ = wholeNumber(raw["chunks"])
	item.Tokens, _ = wholeNumber(raw["tokens"])
	item.ContentSHA256, _ = raw["content_sha256"].(string)
	item.Version, _ = wholeNumber(raw["version"])
	item.QueuedAt, _ = raw["queued_at"].(string)
	item.ReadyAt, _ = raw["ready_at"].(string)
	if t, ok := raw["text"].(string); ok {
		item.Text = &t
	}
}
