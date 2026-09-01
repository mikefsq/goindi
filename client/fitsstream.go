package client

import (
	"errors"
	"fmt"
	"io"
)

// FITSWriter decodes a FITS payload as it arrives, so the encoded bytes never
// have to be held whole.
//
// DecodeFITS needs the complete payload in memory and then allocates the samples
// beside it, which for a 62 MP 16-bit frame is two ~124 MB buffers live at once.
// This holds one: the header (a few KB) and the output samples. Rows are
// converted as they land, so the cost is the same single pass either way.
//
// Write runs wherever the BLOB sink runs, which for a Client is the read loop
// shared by every device on the connection. The work per row is a byte swap and
// a bit flip — the same work the buffering path does later, moved earlier — but
// a caller that must not occupy the read loop at all should keep buffering.
type FITSWriter struct {
	hdrBuf []byte // header bytes, until END is found
	hdr    map[string]string

	w, h, bitpix, per, stride int
	bzero, bscale             float64
	bottomUp                  bool

	pix []uint16
	// row is the partial row carried across a Write boundary; payloads arrive in
	// arbitrary chunks and a row that spans two of them cannot be converted from
	// either alone.
	row  []byte
	held int // bytes valid in row
	y    int // the next SOURCE row to convert

	err  error
	done bool
}

// NewFITSWriter returns a writer that decodes one FITS payload.
func NewFITSWriter() *FITSWriter { return &FITSWriter{} }

// ErrIncompleteFITS reports a payload that ended before every row arrived.
var ErrIncompleteFITS = errors.New("indi: FITS payload ended early")

// Write consumes payload bytes. A decode error is latched and returned from
// every later call, including Close.
func (f *FITSWriter) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n := len(p)
	for len(p) > 0 {
		if f.pix == nil {
			rest, ok, err := f.consumeHeader(p)
			if err != nil {
				f.err = err
				return 0, err
			}
			if !ok {
				return n, nil // header still incomplete
			}
			p = rest
			continue
		}
		if f.y >= f.h {
			return n, nil // padding after the last row
		}
		// A whole row already in hand, and nothing carried over: convert straight
		// out of the caller's buffer without copying it first.
		if f.held == 0 && len(p) >= f.stride {
			f.convert(p[:f.stride])
			p = p[f.stride:]
			continue
		}
		take := f.stride - f.held
		if take > len(p) {
			take = len(p)
		}
		copy(f.row[f.held:], p[:take])
		f.held += take
		p = p[take:]
		if f.held == f.stride {
			f.convert(f.row)
			f.held = 0
		}
	}
	return n, nil
}

// convert writes one source row to its place in the output, applying the row
// flip as a destination choice rather than a second pass.
func (f *FITSWriter) convert(src []byte) {
	dst := f.y
	if f.bottomUp {
		dst = f.h - 1 - f.y
	}
	convertFITSRow(f.pix[dst*f.w:(dst+1)*f.w], src, f.bitpix, f.bzero, f.bscale)
	f.y++
}

// consumeHeader accumulates header blocks until END, then sizes the output.
// It reports whether the header is complete, and returns the unconsumed bytes.
func (f *FITSWriter) consumeHeader(p []byte) (rest []byte, ok bool, err error) {
	// Only whole blocks are examined, so at most one block is buffered beyond the
	// one carrying END.
	f.hdrBuf = append(f.hdrBuf, p...)
	if len(f.hdrBuf) < fitsBlock {
		return nil, false, nil
	}
	hdr, off, err := scanFITSHeader(f.hdrBuf)
	if err != nil {
		// No END card yet is not a failure while bytes are still arriving; a
		// payload that is not FITS at all is, and is decided on the first block.
		if len(f.hdrBuf) >= fitsBlock && !isFITSMagic(f.hdrBuf) {
			return nil, false, err
		}
		return nil, false, nil
	}
	if err := f.size(hdr); err != nil {
		return nil, false, err
	}
	f.hdr = hdr
	rest = f.hdrBuf[off:]
	f.hdrBuf = nil
	return rest, true, nil
}

// size allocates the output from the header, applying the same rules DecodeFITS
// does so the two agree on what they will accept.
func (f *FITSWriter) size(hdr map[string]string) error {
	naxis := hdrInt(hdr, "NAXIS", 0)
	if naxis != 2 && naxis != 3 {
		return fmt.Errorf("indi: FITS NAXIS=%d, want a 2-D mono or 3-D colour image", naxis)
	}
	f.w, f.h = hdrInt(hdr, "NAXIS1", 0), hdrInt(hdr, "NAXIS2", 0)
	if f.w <= 0 || f.h <= 0 {
		return fmt.Errorf("indi: FITS dimensions %dx%d", f.w, f.h)
	}
	f.bitpix = hdrInt(hdr, "BITPIX", 0)
	f.per = f.bitpix / 8
	if f.per < 0 {
		f.per = -f.per
	}
	if f.per == 0 || !fitsSupported(f.bitpix) {
		return fmt.Errorf("indi: unsupported FITS BITPIX %d", f.bitpix)
	}
	f.bzero, f.bscale = hdrFloat(hdr, "BZERO", 0), hdrFloat(hdr, "BSCALE", 1)
	if f.bscale == 0 {
		f.bscale = 1
	}
	f.bottomUp, _ = fitsBottomUp(hdr)
	f.stride = f.w * f.per
	f.pix = make([]uint16, f.w*f.h)
	f.row = make([]byte, f.stride)
	return nil
}

// Close ends the payload. It reports an incomplete frame rather than handing
// back one whose missing rows read as black sky.
func (f *FITSWriter) Close() error {
	if f.err != nil {
		return f.err
	}
	f.done = true
	if f.pix == nil {
		f.err = fmt.Errorf("indi: BLOB of %d bytes is not FITS — is CCD_TRANSFER_FORMAT set to FORMAT_FITS?",
			len(f.hdrBuf))
		return f.err
	}
	if f.y < f.h {
		f.err = fmt.Errorf("%w: %d of %d rows", ErrIncompleteFITS, f.y, f.h)
		return f.err
	}
	return nil
}

// Frame returns the decoded payload, once Close has accepted it.
func (f *FITSWriter) Frame() (*Frame, error) {
	if f.err != nil {
		return nil, f.err
	}
	if !f.done {
		return nil, errors.New("indi: FITS payload is not complete")
	}
	fr := &Frame{Width: f.w, Height: f.h, Pix: f.pix, BitPix: f.bitpix, Header: f.hdr}
	// A 3-axis FITS is already colour-separated, so only the 2-D case has a phase.
	if hdrInt(f.hdr, "NAXIS", 0) == 2 {
		fitsMosaic(fr, f.hdr, f.bottomUp)
	}
	return fr, nil
}

// isFITSMagic reports whether a payload opens with the SIMPLE keyword.
func isFITSMagic(b []byte) bool {
	return len(b) >= 6 && string(b[:6]) == "SIMPLE"
}

var _ io.WriteCloser = (*FITSWriter)(nil)
