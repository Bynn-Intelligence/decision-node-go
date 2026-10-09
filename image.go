package decisionnode

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// The API's fixed image limits, checked locally before any request.
const (
	// MaxImages is the most images one request (or batch line) may carry,
	// URLs included.
	MaxImages = 16
	// MaxBodyBytes is the largest request body the API takes, 25 MiB. Inline
	// images and video frames count toward it; uploads and URLs do not.
	MaxBodyBytes = 26_214_400
	// MaxImageBytes is the largest image, in bytes.
	MaxImageBytes = 10 * 1024 * 1024
	// MaxImagePixels is the largest image, in pixels, read from its header.
	MaxImagePixels = 40_000_000
	// MaxImageURLChars is the longest image URL.
	MaxImageURLChars = 2048
)

// DefaultInlineThreshold is the size, in bytes, under which the local images
// of one request (or one session frame, or one batch line) together go inline
// as base64. At or above it they are uploaded once with POST /v1/uploads and
// named by their upload_id. WithInlineThreshold overrides it.
const DefaultInlineThreshold = 524288

// Image is an image sent beside the state. Every question sees every image.
//
// Build it with ImageURL (recommended: nothing is uploaded and the request
// stays small), ImageFile, ImageData, ImageBase64, ImageReader or
// ImageUpload. Local bytes are checked before any request: JPEG, PNG or WebP
// by their first bytes, at most 10 MB, at most 40 megapixels read from the
// header. A request whose local images together are under the inline
// threshold sends them inline; at or above it they are uploaded first, and an
// image uploaded before is named by its cached upload_id whatever its size.
//
// An Image is a small value: copies share the bytes and what was computed
// from them, so reusing one Image across calls hashes it once.
type Image struct {
	id        string
	url       string
	uploadID  string
	mediaType string
	src       *imageSource
}

// imageSource holds an image's bytes and what is computed from them, once.
type imageSource struct {
	data []byte
	// oversize is set when a reader held more than MaxImageBytes: only the
	// first MaxImageBytes+1 bytes were kept.
	oversize bool

	infoOnce sync.Once
	sniffed  string
	width    int
	height   int
	dimsOK   bool

	hashOnce sync.Once
	sha      [32]byte
}

func newSource(data []byte) *imageSource { return &imageSource{data: data} }

// info returns the media type found in the first bytes and the pixel size
// read from the header.
func (s *imageSource) info() (mediaType string, w, h int, ok bool) {
	s.infoOnce.Do(func() {
		s.sniffed = sniffMediaType(s.data)
		if s.sniffed != "" {
			s.width, s.height, s.dimsOK = imageDimensions(s.data, s.sniffed)
		}
	})
	return s.sniffed, s.width, s.height, s.dimsOK
}

func (s *imageSource) sum() [32]byte {
	s.hashOnce.Do(func() { s.sha = sha256.Sum256(s.data) })
	return s.sha
}

// ImageURL sends an https URL for the server to fetch. It is the fastest
// input: nothing is uploaded and the request stays small. The URL is checked
// locally (https, at most 2,048 characters) and never fetched by the SDK.
func ImageURL(id, url string) Image { return Image{id: id, url: url} }

// ImageFile reads an image file now, when the Image is made, so an unreadable
// file fails here rather than at the call. Its format, size and pixel count
// are checked at the call, before anything is sent: a file that is not JPEG,
// PNG or WebP, or is over the limits, makes the call return a
// *BadRequestError.
func ImageFile(id, path string) (Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return Image{}, fmt.Errorf("decisionnode: image %q: %w", id, err)
	}
	defer func() { _ = f.Close() }()
	img, err := ImageReader(id, f, "")
	if err != nil {
		return Image{}, fmt.Errorf("decisionnode: image %q: %w", id, err)
	}
	return img, nil
}

// ImageData sends image bytes. An empty mediaType is taken from the first
// bytes (JPEG, PNG or WebP); a given one must match them. The bytes are
// checked at the call, before anything is sent. The slice is not copied: do
// not change it while a call that uses the Image runs.
func ImageData(id string, data []byte, mediaType string) Image {
	return Image{id: id, mediaType: mediaType, src: newSource(data)}
}

// ImageBase64 sends image bytes given as base64 text (standard alphabet,
// padded or not; a "data:<type>;base64," prefix is accepted). Text that is
// not base64 is an argument error, returned here.
func ImageBase64(id, text, mediaType string) (Image, error) {
	if rest, ok := strings.CutPrefix(text, "data:"); ok {
		if i := strings.Index(rest, ";base64,"); i >= 0 {
			if mediaType == "" {
				mediaType = rest[:i]
			}
			text = rest[i+len(";base64,"):]
		}
	}
	text = strings.TrimSpace(text)
	enc := base64.StdEncoding
	if !strings.HasSuffix(text, "=") && len(text)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	data, err := enc.DecodeString(text)
	if err != nil {
		return Image{}, fmt.Errorf("decisionnode: image %q is not valid base64: %w", id, err)
	}
	return ImageData(id, data, mediaType), nil
}

// ImageReader reads image bytes from r now. At most MaxImageBytes+1 bytes are
// read: a longer image is refused at the call as over the size limit. A read
// error is returned here.
func ImageReader(id string, r io.Reader, mediaType string) (Image, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("decisionnode: image %q: %w", id, err)
	}
	src := newSource(buf.Bytes())
	src.oversize = n > MaxImageBytes
	return Image{id: id, mediaType: mediaType, src: src}, nil
}

// ImageUpload names an upload made earlier, by Client.Upload or by any client
// of the same workspace, less than an hour ago. mediaType may be empty; when
// given, it must equal the type declared at upload.
func ImageUpload(id, uploadID, mediaType string) Image {
	return Image{id: id, uploadID: uploadID, mediaType: mediaType}
}

// ID returns the image's id, unique within a request. The model sees the
// image as "Picture <n>" (its place in the request's images, from 1) with
// this id beside it, so text in the state or the instructions can refer to it
// by either.
func (img Image) ID() string { return img.id }

// MediaType returns the declared media type, else the one found in the
// image's first bytes, else "".
func (img Image) MediaType() string {
	if img.mediaType != "" || img.src == nil {
		return img.mediaType
	}
	t, _, _, _ := img.src.info()
	return t
}

// wireMediaType is the media type sent with local bytes.
func (img Image) wireMediaType() string {
	if img.mediaType != "" {
		return img.mediaType
	}
	t, _, _, _ := img.src.info()
	return t
}

// cacheKey identifies local bytes in the upload cache.
func (img Image) cacheKey() cacheKey {
	return cacheKey{sum: img.src.sum(), mediaType: img.wireMediaType()}
}

func sniffMediaType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// imageDimensions reads the pixel size from an image header without decoding
// the image.
func imageDimensions(b []byte, mediaType string) (w, h int, ok bool) {
	switch mediaType {
	case "image/png":
		return pngDimensions(b)
	case "image/jpeg":
		return jpegDimensions(b)
	case "image/webp":
		return webpDimensions(b)
	}
	return 0, 0, false
}

func be16(b []byte) int { return int(b[0])<<8 | int(b[1]) }
func be32(b []byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
func le16(b []byte) int { return int(b[0]) | int(b[1])<<8 }
func le24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// pngDimensions reads the IHDR chunk, which the format requires first.
func pngDimensions(b []byte) (int, int, bool) {
	if len(b) < 24 || string(b[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return be32(b[16:20]), be32(b[20:24]), true
}

// jpegDimensions walks the marker segments to the first start-of-frame.
func jpegDimensions(b []byte) (int, int, bool) {
	i := 2
	for i+3 < len(b) {
		if b[i] != 0xFF {
			return 0, 0, false
		}
		// Any number of 0xFF fill bytes may precede a marker.
		for i+1 < len(b) && b[i+1] == 0xFF {
			i++
		}
		if i+3 >= len(b) {
			break
		}
		marker := b[i+1]
		switch {
		case marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			i += 2
			continue
		case marker == 0xD9 || marker == 0xDA:
			// End of image or start of scan before any frame header.
			return 0, 0, false
		}
		length := be16(b[i+2 : i+4])
		if length < 2 {
			return 0, 0, false
		}
		switch marker {
		case 0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
			if i+9 > len(b) {
				return 0, 0, false
			}
			return be16(b[i+7 : i+9]), be16(b[i+5 : i+7]), true
		}
		i += 2 + length
	}
	return 0, 0, false
}

// webpDimensions reads the first chunk: lossy (VP8), lossless (VP8L) or
// extended (VP8X).
func webpDimensions(b []byte) (int, int, bool) {
	if len(b) < 30 {
		return 0, 0, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		return le16(b[26:28]) & 0x3FFF, le16(b[28:30]) & 0x3FFF, true
	case "VP8L":
		if b[20] != 0x2F {
			return 0, 0, false
		}
		v := le32(b[21:25])
		return int(v&0x3FFF) + 1, int((v>>14)&0x3FFF) + 1, true
	case "VP8X":
		return le24(b[24:27]) + 1, le24(b[27:30]) + 1, true
	}
	return 0, 0, false
}
