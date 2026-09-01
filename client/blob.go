package client

import (
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// BlobMode is an enableBLOB policy.
type BlobMode string

const (
	BlobNever BlobMode = "Never" // no BLOBs; the server's default for a new client
	BlobAlso  BlobMode = "Also"  // BLOBs alongside all other property traffic
	BlobOnly  BlobMode = "Only"  // BLOBs and nothing else, for a dedicated connection
)

// EnableBLOB asks the server to deliver BLOBs, which it silently sends to nobody
// until asked; an empty name applies the mode to every property of the device.
func (c *Client) EnableBLOB(device, name string, mode BlobMode) error {
	return c.send(wEnableBLOB{Device: device, Name: name, Mode: string(mode)})
}

// BlobInfo describes one arriving payload, passed to the sink before its bytes.
type BlobInfo struct {
	Device, Property, Member string

	// Size is the byte count the sender declared (0 if none), measured after
	// base64 decoding and decompression, so a compressed payload delivers fewer
	// bytes than this to the sink.
	Size int64

	// Format is the sender's extension, e.g. ".fits", with any ".z" removed.
	Format string

	// Compressed reports that the format ended in ".z", so the bytes are zlib
	// data the client hands on without decompressing.
	Compressed bool
}

// BlobSink routes each BLOB payload's decoded bytes to a writer chosen per
// payload, closing it when the payload ends (through CloseWithError if it ended
// incomplete); returning nil discards the payload.
//
// One sink serves the whole connection. Consumers that own a single device
// should use BlobSinkFor instead: registering here twice replaces the first
// sink, which on a server with two cameras silently sends both streams to
// whichever consumer registered last.
func (c *Client) BlobSink(fn func(BlobInfo) io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobSink = fn
}

// BlobSinkFor routes one device's BLOBs, so several consumers can share a
// connection without displacing each other. A device with no sink of its own
// falls back to BlobSink's.
//
// An indiserver multiplexes every device onto one connection, so two cameras
// arrive on the same stream; a client that registers a sink per camera through
// BlobSink keeps only the last, and the other camera exposes forever without
// ever producing an image. Passing nil removes the device's sink.
func (c *Client) BlobSinkFor(device string, fn func(BlobInfo) io.Writer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		delete(c.blobSinks, device)
		return
	}
	if c.blobSinks == nil {
		c.blobSinks = map[string]func(BlobInfo) io.Writer{}
	}
	c.blobSinks[device] = fn
}

// BufferBlobs collects each payload in memory and hands it to fn complete; fn
// runs on the read loop, so it must not block.
func (c *Client) BufferBlobs(fn func(BlobInfo, []byte)) {
	c.BlobSink(func(info BlobInfo) io.Writer {
		b := &blobBuf{info: info, fn: fn}
		if info.Size > 0 {
			b.buf = make([]byte, 0, info.Size)
		}
		return b
	})
}

// BufferBlobsFor is BufferBlobs for one device, so several consumers can share a
// connection without displacing each other's sinks. See BlobSinkFor.
func (c *Client) BufferBlobsFor(device string, fn func(BlobInfo, []byte)) {
	c.BlobSinkFor(device, func(info BlobInfo) io.Writer {
		b := &blobBuf{info: info, fn: fn}
		if info.Size > 0 {
			b.buf = make([]byte, 0, info.Size)
		}
		return b
	})
}

type blobBuf struct {
	info BlobInfo
	fn   func(BlobInfo, []byte)
	buf  []byte
}

func (b *blobBuf) Write(p []byte) (int, error) { b.buf = append(b.buf, p...); return len(p), nil }
func (b *blobBuf) Close() error                { b.fn(b.info, b.buf); return nil }
func (b *blobBuf) CloseWithError(error) error  { b.buf = nil; return nil } // incomplete: never delivered to fn

// sinkFor picks the sink for a payload: the device's own if it has one, else
// the connection-wide one.
func (c *Client) sinkFor(device string) func(BlobInfo) io.Writer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn, ok := c.blobSinks[device]; ok {
		return fn
	}
	return c.blobSink
}

// readBlobVector handles a setBLOBVector token by token rather than through
// DecodeElement, so no decoded copy of the payload is retained.
func (c *Client) readBlobVector(dec *xml.Decoder, se xml.StartElement) error {
	device, prop, state := attr(se, "device"), attr(se, "name"), attr(se, "state")
	message := attr(se, "message")
	var seen []Member
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "oneBLOB" {
				if err := dec.Skip(); err != nil {
					return err
				}
				continue
			}
			m, err := c.readOneBlob(dec, device, prop, t)
			if err != nil {
				return err
			}
			seen = append(seen, m)
		case xml.EndElement:
			if t.Name.Local == se.Name.Local {
				c.applyBlobMeta(device, prop, state, message, seen)
				return nil
			}
		}
	}
}

func (c *Client) readOneBlob(dec *xml.Decoder, device, prop string, se xml.StartElement) (Member, error) {
	info := BlobInfo{
		Device: device, Property: prop, Member: attr(se, "name"),
		Format: attr(se, "format"),
	}
	sizeAttr := strings.TrimSpace(attr(se, "size"))
	info.Size, _ = strconv.ParseInt(sizeAttr, 10, 64)
	if strings.HasSuffix(info.Format, ".z") {
		info.Compressed = true
		info.Format = strings.TrimSuffix(info.Format, ".z")
	}
	m := Member{Name: info.Member, Format: attr(se, "format"), Size: info.Size}

	// Servers send <oneBLOB size='0' enclen='0'> for state-only updates, which
	// must not reach the sink as empty payloads.
	fn := c.sinkFor(device)
	var dst io.Writer
	b64 := &b64Writer{} // nil w discards, keeping the stream in sync
	engage := func() {
		if fn != nil {
			dst = fn(info)
			b64.w = dst
		}
		fn = nil
	}
	if info.Size > 0 {
		engage()
	}

	// The size attr is the uncompressed count, so checking it for a .z payload
	// means inflating a teed copy; the sink still gets the compressed stream.
	var ic *inflateCounter
	if info.Compressed && sizeAttr != "" && info.Size > 0 {
		ic = newInflateCounter()
		b64.tee = ic.Write
		defer ic.stop() // reap the reader goroutine on every exit path
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			return m, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if fn != nil && hasB64Data(t) {
				engage()
			}
			if err := b64.Write(t); err != nil {
				return m, err // malformed base64: the stream is no longer trustworthy
			}
		case xml.StartElement:
			if err := dec.Skip(); err != nil {
				return m, err
			}
		case xml.EndElement:
			err := b64.finish()
			if err == nil && sizeAttr != "" {
				switch {
				case !info.Compressed:
					if b64.total != info.Size {
						c.softError("indi client: BLOB %s.%s.%s: decoded %d bytes, size attr says %d",
							device, prop, info.Member, b64.total, info.Size)
					}
				case ic != nil:
					if n, ierr := ic.result(); ierr != nil {
						c.softError("indi client: BLOB %s.%s.%s: compressed payload does not inflate: %v",
							device, prop, info.Member, ierr)
					} else if n != info.Size {
						c.softError("indi client: BLOB %s.%s.%s: inflates to %d bytes, size attr says %d",
							device, prop, info.Member, n, info.Size)
					}
				}
			}
			if b64.werr != nil {
				c.softError("indi client: BLOB %s.%s.%s: sink write failed, payload discarded: %v",
					device, prop, info.Member, b64.werr)
			}
			if ferr := firstErr(err, b64.werr); ferr != nil {
				abortSink(dst, ferr)
			} else if cl, ok := dst.(io.Closer); ok {
				err = cl.Close()
			}
			return m, err
		}
	}
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// abortSink ends a payload that did not complete, reporting why to a sink that
// offers CloseWithError.
func abortSink(dst io.Writer, reason error) {
	if cwe, ok := dst.(interface{ CloseWithError(error) error }); ok {
		_ = cwe.CloseWithError(reason)
		return
	}
	if cl, ok := dst.(io.Closer); ok {
		_ = cl.Close()
	}
}

func hasB64Data(p []byte) bool {
	for _, ch := range p {
		switch ch {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return true
	}
	return false
}

func (c *Client) applyBlobMeta(device, prop, state, message string, seen []Member) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dm := c.store[device]
	if dm == nil {
		return
	}
	p := dm[prop]
	if p == nil {
		return
	}
	if state != "" {
		p.State = state
	}
	if message != "" {
		p.Message = message
	}
	for _, nm := range seen {
		found := false
		for i := range p.Members {
			if p.Members[i].Name == nm.Name {
				p.Members[i].Format, p.Members[i].Size = nm.Format, nm.Size
				found = true
			}
		}
		if !found {
			p.Members = append(p.Members, nm)
		}
	}
	p.Rev++
	c.signalLocked()
}

// b64Writer decodes base64 one chardata token at a time; a failing destination is
// demoted to discard, since the payload must still be consumed or the XML stream
// desynchronises.
type b64Writer struct {
	w       io.Writer
	tee     func(p []byte)
	werr    error
	total   int64
	scratch []byte
	quad    [4]byte
	n       int
	done    bool // a padded quad ended the payload; further data is malformed
}

func (b *b64Writer) Write(p []byte) error {
	out := b.scratch[:0]
	if need := len(p)/4*3 + 3; cap(out) < need {
		out = make([]byte, 0, need)
	}
	var dec [3]byte
	for _, ch := range p {
		switch ch {
		case ' ', '\t', '\r', '\n':
			continue
		}
		if b.done {
			return fmt.Errorf("indi client: base64 data after padding")
		}
		b.quad[b.n] = ch
		b.n++
		if b.n < 4 {
			continue
		}
		b.n = 0
		n, err := decodeQuad(&b.quad, &dec)
		if err != nil {
			return err
		}
		if n < 3 {
			b.done = true
		}
		b.total += int64(n)
		out = append(out, dec[:n]...)
	}
	b.scratch = out[:0] // keep the grown buffer for the payload's next token
	if len(out) == 0 {
		return nil
	}
	if b.tee != nil {
		b.tee(out) // even after a sink failure: the count must stay honest
	}
	if b.w == nil {
		return nil
	}
	if _, err := b.w.Write(out); err != nil {
		b.werr = err
		b.w = nil
	}
	return nil
}

func (b *b64Writer) finish() error {
	if b.n != 0 {
		return fmt.Errorf("indi client: BLOB base64 truncated (%d trailing chars)", b.n)
	}
	return nil
}

// inflateCounter measures how many bytes a zlib stream inflates to without
// retaining any of it.
type inflateCounter struct {
	pw     *io.PipeWriter
	broken bool
	done   chan struct{}
	n      int64 // valid after done is closed
	err    error // valid after done is closed
}

func newInflateCounter() *inflateCounter {
	pr, pw := io.Pipe()
	ic := &inflateCounter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(ic.done)
		zr, err := zlib.NewReader(pr)
		if err != nil {
			ic.err = err
			pr.CloseWithError(err) // unblock any pending Write
			return
		}
		ic.n, ic.err = io.Copy(io.Discard, zr) // reaching EOF verifies the adler32 too
		if cerr := zr.Close(); ic.err == nil {
			ic.err = cerr
		}
		// The zlib stream can end before the payload does; fail further writes
		// rather than blocking them forever.
		pr.CloseWithError(io.ErrClosedPipe)
	}()
	return ic
}

// Write pushes decoded payload bytes into the inflater, absorbing errors so a
// stream that stops inflating surfaces through result instead.
func (ic *inflateCounter) Write(p []byte) {
	if ic.broken {
		return
	}
	if _, err := ic.pw.Write(p); err != nil {
		ic.broken = true
	}
}

// result ends the stream and reports the inflated byte count, or why inflation
// failed.
func (ic *inflateCounter) result() (int64, error) {
	ic.pw.Close()
	<-ic.done
	return ic.n, ic.err
}

// stop abandons the count and reaps the reader goroutine; it is safe to call
// more than once and after result.
func (ic *inflateCounter) stop() {
	ic.pw.Close() // a second Close is a no-op
	<-ic.done
}

// decodeQuad decodes one base64 quantum; padding is legal only as the final one
// or two bytes ("xxx=" or "xx=="), anywhere else it is malformed.
func decodeQuad(quad *[4]byte, out *[3]byte) (int, error) {
	n := 3
	switch {
	case quad[0] == '=' || quad[1] == '=':
		return 0, fmt.Errorf("indi client: malformed base64 padding")
	case quad[2] == '=':
		if quad[3] != '=' {
			return 0, fmt.Errorf("indi client: base64 data after padding")
		}
		n = 1
	case quad[3] == '=':
		n = 2
	}
	var v [4]int8
	for i, ch := range quad {
		if ch == '=' {
			continue // v[i] stays 0
		}
		d := b64rev[ch]
		if d < 0 {
			return 0, fmt.Errorf("indi client: invalid base64 byte %q", ch)
		}
		v[i] = d
	}
	out[0] = byte(v[0]<<2 | v[1]>>4)
	out[1] = byte(v[1]<<4 | v[2]>>2)
	out[2] = byte(v[2]<<6 | v[3])
	return n, nil
}

var b64rev = func() (t [256]int8) {
	for i := range t {
		t[i] = -1
	}
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alpha); i++ {
		t[alpha[i]] = int8(i)
	}
	return
}()

func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
