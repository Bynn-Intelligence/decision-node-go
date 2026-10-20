package decisionnode

import (
	"bytes"
	"container/list"
	"context"
	"crypto/md5" //nolint:gosec // Content-MD5 is what the storage checks; it is not used for security.
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// cacheEntries bounds the upload cache; the least recently used entry
	// goes first.
	cacheEntries = 256
	// cacheMargin is how long before its expires_at an upload stops being
	// reused.
	cacheMargin = 5 * time.Minute
	// maxUploadFiles is the most files one POST /v1/uploads takes.
	maxUploadFiles = 16
)

// cacheKey identifies local image bytes: their SHA-256 and media type.
type cacheKey struct {
	sum       [32]byte
	mediaType string
}

type cacheEntry struct {
	key       cacheKey
	uploadID  string
	expiresAt time.Time
}

// uploadCache maps image bytes to the upload that holds them, in the
// client's memory only, so repeat calls, DecideMany, session frames and batch
// lines reuse an upload until 5 minutes before it expires.
type uploadCache struct {
	mu      sync.Mutex
	order   *list.List // front: most recently used
	entries map[cacheKey]*list.Element
	// flights holds the uploads in progress, so concurrent calls with the
	// same bytes upload them once.
	flights map[cacheKey]*flight
}

type flight struct {
	done   chan struct{}
	result UploadResult
	err    error
}

// UploadResult is one image's upload.
type UploadResult struct {
	// ID is the upload_id: name the image in later calls with ImageUpload.
	ID string
	// ExpiresAt is when the upload stops being usable as a reference, as the
	// API sent it. Zero for an id given with ImageUpload, whose expiry the
	// client does not know.
	ExpiresAt time.Time
}

func newUploadCache() *uploadCache {
	return &uploadCache{order: list.New(), entries: map[cacheKey]*list.Element{}, flights: map[cacheKey]*flight{}}
}

func (c *uploadCache) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries) == 0
}

// get returns the live upload for the key, judged at now.
func (c *uploadCache) get(k cacheKey, now time.Time) (UploadResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(k, now)
}

func (c *uploadCache) getLocked(k cacheKey, now time.Time) (UploadResult, bool) {
	el, ok := c.entries[k]
	if !ok {
		return UploadResult{}, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.expiresAt.Add(-cacheMargin)) {
		c.order.Remove(el)
		delete(c.entries, k)
		return UploadResult{}, false
	}
	c.order.MoveToFront(el)
	return UploadResult{ID: e.uploadID, ExpiresAt: e.expiresAt}, true
}

func (c *uploadCache) putLocked(k cacheKey, uploadID string, expiresAt time.Time) {
	if el, ok := c.entries[k]; ok {
		e := el.Value.(*cacheEntry)
		e.uploadID, e.expiresAt = uploadID, expiresAt
		c.order.MoveToFront(el)
		return
	}
	c.entries[k] = c.order.PushFront(&cacheEntry{key: k, uploadID: uploadID, expiresAt: expiresAt})
	for c.order.Len() > cacheEntries {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.entries, last.Value.(*cacheEntry).key)
	}
}

// drop forgets the entries naming these uploads.
func (c *uploadCache) drop(uploadIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range uploadIDs {
		for k, el := range c.entries {
			if el.Value.(*cacheEntry).uploadID == id {
				c.order.Remove(el)
				delete(c.entries, k)
			}
		}
	}
}

// placement is the wire form of a request's images and the uploads the SDK
// placed in it (from the cache or uploaded now), which a stale-reference
// answer drops.
type placement struct {
	wires  [][]byte
	placed []string
}

// placeImages decides how each image travels: a URL or an upload id as
// given; local bytes by their cached upload id whatever their size, else
// inline when the request's local images total under the inline threshold,
// else uploaded now. withID is false for a session frame, whose image has no
// id.
func (c *Client) placeImages(ctx context.Context, images []Image, withID bool) (placement, error) {
	var p placement
	if len(images) == 0 {
		return p, nil
	}
	p.wires = make([][]byte, len(images))
	total := 0
	var local []int
	for i, img := range images {
		switch {
		case img.url != "":
			p.wires[i] = urlWire(img.id, img.url)
		case img.uploadID != "":
			p.wires[i] = uploadWire(img.id, img.uploadID, img.mediaType, withID)
		default:
			total += len(img.src.data)
			local = append(local, i)
		}
	}
	if len(local) == 0 {
		return p, nil
	}
	// Hash only when a hash can matter: the cache holds something, or the
	// images will be uploaded.
	upload := total >= c.inlineThreshold
	var misses []int
	if upload || !c.uploads.empty() {
		now := c.now()
		for _, i := range local {
			if hit, ok := c.uploads.get(images[i].cacheKey(), now); ok {
				p.wires[i] = uploadWire(images[i].id, hit.ID, "", withID)
				p.placed = append(p.placed, hit.ID)
				continue
			}
			misses = append(misses, i)
		}
	} else {
		misses = local
	}
	if len(misses) == 0 {
		return p, nil
	}
	if !upload {
		for _, i := range misses {
			p.wires[i] = inlineWire(images[i], withID)
		}
		return p, nil
	}
	toUpload := make([]Image, len(misses))
	for j, i := range misses {
		toUpload[j] = images[i]
	}
	ups, err := c.uploadImages(ctx, toUpload)
	if err != nil {
		return placement{}, err
	}
	for j, i := range misses {
		p.wires[i] = uploadWire(images[i].id, ups[j].ID, "", withID)
		p.placed = append(p.placed, ups[j].ID)
	}
	return p, nil
}

// uploadIDChars is the length of an upload id, upl_ and 32 hex digits.
const uploadIDChars = 36

// imagesSize is the length of the body's `,"images":[...]` member as
// placeImages would write it, without uploading anything: an image it would
// upload is counted by an upload id's length.
func (c *Client) imagesSize(images []Image) int {
	if len(images) == 0 {
		return 0
	}
	n := len(`,"images":[]`) + len(images) - 1
	total := 0
	var local []int
	for i, img := range images {
		switch {
		case img.url != "":
			n += len(urlWire(img.id, img.url))
		case img.uploadID != "":
			n += len(uploadWire(img.id, img.uploadID, img.mediaType, true))
		default:
			total += len(img.src.data)
			local = append(local, i)
		}
	}
	upload := total >= c.inlineThreshold
	byID := len(uploadWire("", strings.Repeat("u", uploadIDChars), "", true)) - len(appendIDMember(nil, ""))
	now := c.now()
	for _, i := range local {
		img := images[i]
		if upload {
			n += byID + len(appendIDMember(nil, img.id))
			continue
		}
		if !c.uploads.empty() {
			if _, ok := c.uploads.get(img.cacheKey(), now); ok {
				n += byID + len(appendIDMember(nil, img.id))
				continue
			}
		}
		// {"id":...,"media_type":"...","data":"<base64>"}
		n += len(appendIDMember(nil, img.id)) + len(`{,"media_type":"","data":""}`) +
			len(img.wireMediaType()) + base64.StdEncoding.EncodedLen(len(img.src.data))
	}
	return n
}

// Upload uploads local images, whatever their size, and returns the upload
// of each, in order: its upload_id and when it expires. The images not in the
// cache go in one POST /v1/uploads, then one PUT each, in parallel. An image
// uploaded before by this client is not uploaded again while its upload is
// live. An image made with ImageUpload returns its own id with a zero
// ExpiresAt. A URL image cannot be uploaded: that is an argument error. Name
// an upload in later calls with ImageUpload.
func (c *Client) Upload(ctx context.Context, images []Image) ([]UploadResult, error) {
	if c.err != nil {
		return nil, c.err
	}
	if len(images) > maxUploadFiles {
		// One presign takes 1 to 16 files: its own 400, before anything is sent.
		msg := fmt.Sprintf("files: 1 to %d files, got %d.", maxUploadFiles, len(images))
		return nil, localBadRequest("api_usage_error", msg, msg)
	}
	if err := checkImages(images, c.limitsFor("")); err != nil {
		return nil, err
	}
	out := make([]UploadResult, len(images))
	var todo []Image
	var where []int
	now := c.now()
	for i, img := range images {
		switch {
		case img.url != "":
			return nil, fmt.Errorf("decisionnode: image %q is a URL; only local bytes can be uploaded", img.id)
		case img.uploadID != "":
			out[i] = UploadResult{ID: img.uploadID}
		default:
			if hit, ok := c.uploads.get(img.cacheKey(), now); ok {
				out[i] = hit
				continue
			}
			todo = append(todo, img)
			where = append(where, i)
		}
	}
	if len(todo) == 0 {
		return out, nil
	}
	ups, err := c.uploadImages(ctx, todo)
	if err != nil {
		return nil, err
	}
	for j, i := range where {
		out[i] = ups[j]
	}
	return out, nil
}

// uploadImages uploads local images and returns their uploads, in order.
// Identical bytes are uploaded once, also across concurrent calls on this
// client, and every new upload goes into the cache.
func (c *Client) uploadImages(ctx context.Context, imgs []Image) ([]UploadResult, error) {
	keys := make([]cacheKey, len(imgs))
	for i, img := range imgs {
		keys[i] = img.cacheKey()
	}
	ids := make(map[cacheKey]UploadResult, len(imgs))
	for {
		var mine []Image
		var mineKeys []cacheKey
		var waits []*flight
		var waitKeys []cacheKey
		now := c.now()
		c.uploads.mu.Lock()
		for i, k := range keys {
			if _, done := ids[k]; done || containsKey(mineKeys, k) || containsKey(waitKeys, k) {
				continue
			}
			if hit, ok := c.uploads.getLocked(k, now); ok {
				ids[k] = hit
				continue
			}
			if f, ok := c.uploads.flights[k]; ok {
				waits = append(waits, f)
				waitKeys = append(waitKeys, k)
				continue
			}
			c.uploads.flights[k] = &flight{done: make(chan struct{})}
			mine = append(mine, imgs[i])
			mineKeys = append(mineKeys, k)
		}
		c.uploads.mu.Unlock()

		var err error
		if len(mine) > 0 {
			var got []UploadResult
			got, err = c.uploadNow(ctx, mine, mineKeys)
			for j, k := range mineKeys {
				if err == nil {
					ids[k] = got[j]
				}
			}
		}
		retry := false
		for j, f := range waits {
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, &Error{msg: "the request was canceled", Err: ctx.Err()}
			}
			switch {
			case f.err == nil:
				ids[waitKeys[j]] = f.result
			case isCanceled(f.err) && ctx.Err() == nil:
				// The other call was canceled, not the upload refused: upload
				// these bytes here.
				retry = true
			case err == nil:
				err = f.err
			}
		}
		if err != nil {
			return nil, err
		}
		if !retry {
			break
		}
	}
	out := make([]UploadResult, len(imgs))
	for i, k := range keys {
		out[i] = ids[k]
	}
	return out, nil
}

func containsKey(keys []cacheKey, k cacheKey) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// presigned is one entry of the POST /v1/uploads answer.
type presigned struct {
	UploadID  string            `json:"upload_id"`
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt string            `json:"expires_at"`
}

// uploadNow presigns the images in one call, PUTs them in parallel, caches
// them and settles their flights. A PUT refused 403 after its URL's lifetime
// (expires_in from the presign answer, by the client's clock) is presigned
// again once.
func (c *Client) uploadNow(ctx context.Context, imgs []Image, keys []cacheKey) (ids []UploadResult, err error) {
	defer func() {
		c.uploads.mu.Lock()
		for j, k := range keys {
			f := c.uploads.flights[k]
			delete(c.uploads.flights, k)
			if f == nil {
				continue
			}
			if err != nil {
				f.err = err
			} else {
				f.result = ids[j]
			}
			close(f.done)
		}
		c.uploads.mu.Unlock()
	}()

	ups, urlsExpire, err := c.presign(ctx, imgs, keys)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, len(imgs))
	var wg sync.WaitGroup
	for i := range imgs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := c.put(pctx, ups[i], imgs[i].src.data, c.uploadTimeout)
			var e *Error
			if errors.As(err, &e) && e.Status == http.StatusForbidden && !c.now().Before(urlsExpire) {
				// The URL outlived its lifetime before the bytes landed:
				// presign this image again, once.
				var again []presigned
				if again, _, err = c.presign(pctx, imgs[i:i+1], keys[i:i+1]); err == nil {
					ups[i] = again[0]
					err = c.put(pctx, ups[i], imgs[i].src.data, c.uploadTimeout)
				}
			}
			if errs[i] = err; err != nil {
				cancel()
			}
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		// The first error that is not the cancellation the others caused.
		if e != nil && !(isCanceled(e) && ctx.Err() == nil) {
			return nil, e
		}
	}
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	ids = make([]UploadResult, len(imgs))
	c.uploads.mu.Lock()
	for i, up := range ups {
		ids[i] = UploadResult{ID: up.UploadID}
		if t, perr := time.Parse(time.RFC3339, up.ExpiresAt); perr == nil {
			ids[i].ExpiresAt = t
			c.uploads.putLocked(keys[i], up.UploadID, t)
		}
	}
	c.uploads.mu.Unlock()
	return ids, nil
}

// presign asks POST /v1/uploads for one URL per image and checks every URL
// before any byte is sent. It returns when the URLs stop being valid, by the
// client's clock.
func (c *Client) presign(ctx context.Context, imgs []Image, keys []cacheKey) ([]presigned, time.Time, error) {
	type file struct {
		ContentType string `json:"content_type"`
		ByteSize    int    `json:"byte_size"`
		Checksum    string `json:"checksum"`
		SHA256      string `json:"sha256"`
	}
	files := make([]file, len(imgs))
	for i, img := range imgs {
		data := img.src.data
		md := md5.Sum(data) //nolint:gosec // see the import
		files[i] = file{
			ContentType: keys[i].mediaType,
			ByteSize:    len(data),
			Checksum:    base64.StdEncoding.EncodeToString(md[:]),
			SHA256:      base64.StdEncoding.EncodeToString(keys[i].sum[:]),
		}
	}
	payload, err := marshal(map[string]any{"files": files})
	if err != nil {
		return nil, time.Time{}, encodeError(err)
	}
	return c.presignFiles(ctx, payload, len(imgs))
}

// presignFiles sends one POST /v1/uploads of n files, already encoded, and
// checks every URL of its answer before any byte is sent. It returns when
// the URLs stop being valid, by the client's clock.
func (c *Client) presignFiles(ctx context.Context, payload []byte, n int) ([]presigned, time.Time, error) {
	resp, err := c.doPayload(ctx, http.MethodPost, "/v1/uploads", payload)
	if err != nil {
		return nil, time.Time{}, err
	}
	var answer struct {
		Uploads   []presigned `json:"uploads"`
		ExpiresIn *float64    `json:"expires_in"`
	}
	if err := json.Unmarshal(resp.data, &answer); err != nil {
		return nil, time.Time{}, invalidResponse(resp, err)
	}
	if len(answer.Uploads) != n {
		return nil, time.Time{}, invalidResponse(resp, fmt.Errorf("%d uploads for %d files", len(answer.Uploads), n))
	}
	for _, up := range answer.Uploads {
		if err := checkStorageURL(up.URL); err != nil {
			return nil, time.Time{}, err
		}
	}
	life := 300 * time.Second
	if answer.ExpiresIn != nil && *answer.ExpiresIn > 0 {
		life = time.Duration(*answer.ExpiresIn * float64(time.Second))
	}
	return answer.Uploads, c.now().Add(life), nil
}

// checkStorageURL accepts an https storage URL, or plain http on the local
// machine only, as for the base URL.
func checkStorageURL(raw string) error {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		switch u.Scheme {
		case "https":
			return nil
		case "http":
			switch strings.ToLower(u.Hostname()) {
			case "localhost", "127.0.0.1", "::1":
				return nil
			}
		}
	}
	return &Error{ErrorType: "upload_failed", msg: "the storage URL must be https; the file was not sent"}
}

var storageCode = regexp.MustCompile(`<Code>([^<]*)</Code>`)

// put sends one file to its presigned URL with exactly the headers the
// presign returned: no Authorization, none of the client's own headers, no
// redirect followed, each attempt allowed timeout. It is retried on 5xx and
// connection errors like any call; a 412 on a retry means an earlier attempt
// stored the object.
func (c *Client) put(ctx context.Context, up presigned, data []byte, timeout time.Duration) error {
	u, err := url.Parse(up.URL)
	if err != nil {
		return &Error{ErrorType: "upload_failed", msg: "the storage URL is not valid", Err: err}
	}
	path := u.Path
	for attempt := 0; ; attempt++ {
		status, header, body, err := c.putOnce(ctx, up, data, path, attempt, timeout)
		switch {
		case err != nil:
			if ctx.Err() != nil || attempt >= c.maxRetries {
				return err
			}
		case status >= 200 && status <= 299:
			return nil
		case status == http.StatusPreconditionFailed && attempt > 0:
			// Write-once: an earlier attempt stored the object.
			return nil
		case status >= 500 && status <= 599 && attempt < c.maxRetries:
		default:
			e := &Error{Status: status, ErrorType: "upload_failed", Header: header,
				msg: fmt.Sprintf("the upload to storage failed with %d", status)}
			if m := storageCode.FindSubmatch(body); m != nil {
				e.Reason = string(m[1])
			}
			return e
		}
		if err := c.sleep(ctx, backoff(attempt)); err != nil {
			return &Error{msg: "the request was canceled", Err: err}
		}
	}
}

func (c *Client) putOnce(ctx context.Context, up presigned, data []byte, path string, attempt int, timeout time.Duration) (int, http.Header, []byte, error) {
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPut, up.URL, bytes.NewReader(data))
	if err != nil {
		return 0, nil, nil, &Error{ErrorType: "upload_failed", msg: "invalid storage request: " + err.Error(), Err: err}
	}
	// Exactly the presigned headers, as given; an empty User-Agent sends none.
	req.Header = make(http.Header, len(up.Headers)+1)
	for k, v := range up.Headers {
		req.Header[k] = []string{v}
	}
	req.Header["User-Agent"] = []string{""}
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	var body []byte
	if err == nil {
		body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	elapsed := time.Since(start)
	if err != nil {
		c.log(ctx, http.MethodPut, path, 0, "", elapsed, attempt, err)
		if ctx.Err() == nil && (errors.Is(actx.Err(), context.DeadlineExceeded) || isTimeout(err)) {
			return 0, nil, nil, &TimeoutError{&Error{msg: "the upload timed out after " + timeout.String(), Err: err}}
		}
		return 0, nil, nil, c.transportError(ctx, actx, err)
	}
	c.log(ctx, http.MethodPut, path, resp.StatusCode, "", elapsed, attempt, nil)
	if c.onResponse != nil {
		c.onResponse(actx, ResponseInfo{Method: http.MethodPut, Path: path, Attempt: attempt + 1, Status: resp.StatusCode,
			Header: resp.Header.Clone(), Body: bytes.Clone(body)})
	}
	return resp.StatusCode, resp.Header, body, nil
}
