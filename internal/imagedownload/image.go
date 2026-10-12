// Image content validation (ARCHITECTURE §5.9): magic-number sniffing for
// the binary formats, a restricted streaming XML check for SVG, the
// Content-Type cross-check, and ConsumeBody — the one pass that streams
// the response body through the byte cap into the caller's sink while
// validating what flows by.

package imagedownload

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"io"
	"mime"
)

// Detected names a validated image type.
type Detected struct {
	// MIME is the detected type ("image/png", ..., "image/svg+xml").
	MIME string
	// Ext is the canonical extension, including the dot (".png", ".jpg").
	Ext string
}

// sniffMagic identifies the binary formats from the first bytes of the
// body (§5.9). ok=false means "not a known binary image" — the caller then
// runs the SVG check; anything that passes neither fails as EImageType.
func sniffMagic(b []byte) (Detected, bool) {
	switch {
	case len(b) >= 16 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) &&
		bytes.Equal(b[12:16], []byte("IHDR")):
		return Detected{MIME: "image/png", Ext: ".png"}, true
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return Detected{MIME: "image/jpeg", Ext: ".jpg"}, true
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return Detected{MIME: "image/gif", Ext: ".gif"}, true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return Detected{MIME: "image/webp", Ext: ".webp"}, true
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")) &&
		(bytes.Equal(b[8:12], []byte("avif")) || bytes.Equal(b[8:12], []byte("avis"))):
		return Detected{MIME: "image/avif", Ext: ".avif"}, true
	default:
		return Detected{}, false
	}
}

// svgDetected is the Detected value for a body whose root check passed.
var svgDetected = Detected{MIME: "image/svg+xml", Ext: ".svg"}

// svgRootCheck runs the restricted XML scan over the first
// Limits.SVGPrologBytes of the stream (§5.9): DTDs and directives are
// rejected outright (no entity declarations of any kind can pass), only
// the XML declaration processing instruction is tolerated, and the first
// element must be an svg in the SVG namespace. Bytes already consumed by
// the check stay consumed — the reader is the shared passthrough stream,
// so nothing is read twice.
//
// The scan is deliberately front-loaded: once the root element is
// confirmed, the remainder is streamed raw. SVGs are stored byte-exact
// and NOT sanitized (§5.9) — well-formedness of the tail adds nothing to
// that posture, and skipping it keeps XML token memory bounded by the
// prolog limit instead of the file size.
func (f *Fetcher) svgRootCheck(r io.Reader) error {
	dec := xml.NewDecoder(io.LimitReader(r, f.Limits.SVGPrologBytes+1))
	for {
		tok, err := dec.Token()
		if err != nil {
			return &Error{Kind: ErrType, Detail: "svg root element not found before the prolog limit"}
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			if t.Target != "xml" {
				return &Error{Kind: ErrType, Detail: "processing instructions are not allowed"}
			}
		case xml.Directive:
			// Includes <!DOCTYPE — every entity declaration path dies here.
			return &Error{Kind: ErrType, Detail: "DTD or directives are not allowed"}
		case xml.Comment:
			// Inert; allowed.
		case xml.CharData:
			if !isXMLSpace(t) {
				return &Error{Kind: ErrType, Detail: "content appears before the root element"}
			}
		case xml.StartElement:
			if t.Name.Local != "svg" || t.Name.Space != "http://www.w3.org/2000/svg" {
				return &Error{Kind: ErrType, Detail: "the root element is not an svg in the SVG namespace"}
			}
			return nil
		default:
			return &Error{Kind: ErrType, Detail: "unexpected XML structure"}
		}
	}
}

// isXMLSpace reports whether b is XML whitespace only.
func isXMLSpace(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}

// ConsumeBody streams resp's body exactly once through the byte cap into
// sink, validating image content on the way, and returns the byte count
// and the detected type (§5.9). The caller's sink receives the exact
// stored bytes (it typically hashes and writes them to the temp file);
// a sink write error aborts the copy and is returned as a plain error so
// the caller can classify storage failures itself. resp.Body is NOT
// closed here — the caller owns it.
func (f *Fetcher) ConsumeBody(resp *Response, sink io.Writer) (int64, Detected, error) {
	limited := &capReader{r: resp.Body, remain: f.Limits.MaxImageBytes + 1}
	src := io.TeeReader(limited, sink)
	n := int64(0)

	// Head: the first SniffBytes drive the magic verdict.
	head := make([]byte, f.Limits.SniffBytes)
	hn, err := io.ReadFull(src, head)
	n += int64(hn)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return n, Detected{}, classifyBody(err)
	}
	if hn == 0 {
		return n, Detected{}, &Error{Kind: ErrType, Detail: ReasonEmpty}
	}

	var det Detected
	if d, ok := sniffMagic(head[:hn]); ok {
		det = d
	} else {
		// SVG candidate: the XML check reads the reassembled head, then
		// continues from src (whose tee already forwarded the head bytes).
		if err := f.svgRootCheck(io.MultiReader(bytes.NewReader(head[:hn]), src)); err != nil {
			return n, Detected{}, err
		}
		det = svgDetected
	}

	// Tail: stream the rest through the same tee (counted, capped,
	// forwarded to sink).
	buf := make([]byte, f.Limits.CopyBuffer)
	tn, err := io.CopyBuffer(io.Discard, src, buf)
	n += tn
	if err != nil {
		if limited.tooLarge {
			return n, Detected{}, &Error{Kind: ErrTooLarge}
		}
		return n, det, classifyBody(err)
	}
	if limited.tooLarge || n > f.Limits.MaxImageBytes {
		return n, det, &Error{Kind: ErrTooLarge}
	}
	if resp.ContentLength > 0 && n < resp.ContentLength {
		return n, det, &Error{Kind: ErrNet, Detail: ReasonTrunc}
	}
	if err := crossCheckContentType(resp.Header.Get("Content-Type"), det.MIME); err != nil {
		return n, det, err
	}
	return n, det, nil
}

// crossCheckContentType compares the declared Content-Type against the
// detected type (§5.9): a missing or octet-stream declaration accepts
// whatever the bytes prove; an image/* declaration must agree with the
// detection; any non-image declaration (text/html, application/pdf, ...)
// is a conflict.
func crossCheckContentType(header, detected string) error {
	if header == "" {
		return nil
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return &Error{Kind: ErrType, Detail: ReasonConflict}
	}
	switch mt {
	case "application/octet-stream":
		return nil
	case detected:
		return nil
	}
	return &Error{Kind: ErrType, Detail: ReasonConflict}
}

// capReader is the byte-cap enforcement: reads stay under remain, and the
// read that would cross the cap fails with ErrTooLarge (the +1 headroom
// lets the caller distinguish over-cap from exactly-at-cap).
type capReader struct {
	r        io.Reader
	remain   int64
	tooLarge bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.remain <= 0 {
		c.tooLarge = true
		return 0, &Error{Kind: ErrTooLarge}
	}
	if int64(len(p)) > c.remain {
		p = p[:c.remain]
	}
	n, err := c.r.Read(p)
	c.remain -= int64(n)
	return n, err
}

// classifyBody maps body-read errors onto the taxonomy. Sink (storage)
// errors are surfaced as-is (plain errors the caller classifies); network
// and deadline errors are re-worded to the fixed vocabulary. TLS alert
// truncations after handshake read like body errors and land on StageTLS.
func classifyBody(err error) error {
	var ours *Error
	if errors.As(err, &ours) {
		return ours
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: ErrTimeout, Detail: StageBody}
	}
	type timeouter interface{ Timeout() bool }
	var t timeouter
	if errors.As(err, &t) && t.Timeout() {
		return &Error{Kind: ErrTimeout, Detail: StageBody}
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return &Error{Kind: ErrNet, Detail: StageTLS}
	}
	return &Error{Kind: ErrNet, Detail: StageBody}
}
