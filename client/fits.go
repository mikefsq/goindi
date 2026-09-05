package client

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// fitsBlock is FITS's allocation unit: headers and data are padded to a
	// multiple of it, which is what makes the data offset computable.
	fitsBlock = 2880
	// fitsCard is the fixed card width; 36 cards fill a block.
	fitsCard = 80
	// fitsUnsignedBZERO is the offset BITPIX=16 uses to carry unsigned samples
	// in a signed type. cfitsio writes it for every USHORT_IMG.
	fitsUnsignedBZERO = 32768
)

// Frame is a decoded CCD payload: row-major 16-bit samples, still mosaiced.
type Frame struct {
	Width, Height int
	// Pix is one sample per pixel, top row first.
	Pix []uint16
	// Bayer reports that BAYERPAT named a mosaic; BayerX/BayerY are the phase
	// (0 or 1) of the red pixel, already adjusted for XBAYROFF/YBAYROFF and for
	// a bottom-up row order.
	Bayer          bool
	BayerX, BayerY int
	// BitPix is the payload's BITPIX, so a caller can tell how much range the
	// 16-bit conversion discarded.
	BitPix int
	// Header carries every card that had a value indicator, unparsed.
	Header map[string]string
}

// DecodeFITS reads the primary image into row-major unsigned 16-bit samples.
// It applies BSCALE/BZERO, rounds and clamps values to 0–65535, and preserves
// Bayer metadata. For a 3-D image, only the first plane is decoded.
// Keep the original payload if the full numeric range or other HDUs are needed.
func DecodeFITS(blob []byte) (*Frame, error) {
	hdr, off, err := scanFITSHeader(blob)
	if err != nil {
		return nil, err
	}
	naxis := hdrInt(hdr, "NAXIS", 0)
	if naxis != 2 && naxis != 3 {
		return nil, fmt.Errorf("indi: FITS NAXIS=%d, want a 2-D mono or 3-D colour image", naxis)
	}
	w, h := hdrInt(hdr, "NAXIS1", 0), hdrInt(hdr, "NAXIS2", 0)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("indi: FITS dimensions %dx%d", w, h)
	}
	bitpix := hdrInt(hdr, "BITPIX", 0)
	per := bitpix / 8
	if per < 0 {
		per = -per
	}
	if per == 0 || !fitsSupported(bitpix) {
		return nil, fmt.Errorf("indi: unsupported FITS BITPIX %d", bitpix)
	}
	data := blob[off:]
	// Checked by division: the dimensions come off the wire and w*h*per
	// overflows, so a wrapped product would pass a naive length test and then
	// index past the payload.
	if w > (len(data)/per)/h {
		return nil, fmt.Errorf("indi: FITS data is short: %d bytes for %dx%d at %d bits",
			len(data), w, h, bitpix)
	}
	bzero, bscale := hdrFloat(hdr, "BZERO", 0), hdrFloat(hdr, "BSCALE", 1)
	if bscale == 0 {
		bscale = 1 // a zero scale would flatten the frame to a constant
	}

	// ROWORDER absent is undefined in the keyword's own proposal, so top-down
	// is a choice: indiccd.cpp stamps TOP-DOWN on every frame it builds, so
	// silence means a driver that did not stamp it, and this keeps that frame
	// the same way up as a stamped one from the same camera.
	bottomUp, _ := fitsBottomUp(hdr)

	// One traversal. The row flip is expressed as where each output row READS
	// FROM, so byte order, BZERO and row order cost one read and one write per
	// pixel between them rather than three of each — a 122 MB frame on a Pi is
	// the case that makes the difference.
	pix := make([]uint16, w*h)
	stride := w * per
	for y := 0; y < h; y++ {
		src := y
		if bottomUp {
			src = h - 1 - y
		}
		convertFITSRow(pix[y*w:(y+1)*w], data[src*stride:src*stride+stride], bitpix, bzero, bscale)
	}

	fr := &Frame{Width: w, Height: h, Pix: pix, BitPix: bitpix, Header: hdr}
	// A 3-axis FITS is already colour-separated — plane 0 is the red plane, not
	// a mosaic — so only the 2-D case can carry a Bayer phase.
	if naxis == 2 {
		fitsMosaic(fr, hdr, bottomUp)
	}
	return fr, nil
}

// fitsMosaic adjusts the Bayer phase for subframe offsets and row order.
// A vertical flip changes parity only when the image height is even.
func fitsMosaic(fr *Frame, hdr map[string]string, flipped bool) {
	x, y, ok := bayerPhase(strings.ToUpper(strings.TrimSpace(hdr["BAYERPAT"])))
	if !ok {
		return // no pattern, or one this build does not know: mono is the safe reading
	}
	fr.Bayer = true
	fr.BayerX = (x + hdrInt(hdr, "XBAYROFF", 0)) & 1
	fr.BayerY = (y + hdrInt(hdr, "YBAYROFF", 0)) & 1
	if flipped && fr.Height%2 == 0 {
		fr.BayerY ^= 1
	}
}

// bayerPhase maps a BAYERPAT name to the (x, y) offset of the red pixel.
func bayerPhase(name string) (x, y int, ok bool) {
	switch name {
	case "RGGB":
		return 0, 0, true
	case "BGGR":
		return 1, 1, true
	case "GRBG":
		return 1, 0, true
	case "GBRG":
		return 0, 1, true
	}
	return 0, 0, false
}

// convertFITSRow converts big-endian FITS samples to unsigned 16-bit ADU.
// For BITPIX=16, BSCALE=1, BZERO=32768, flipping bit 15 applies the offset.
func convertFITSRow(dst []uint16, src []byte, bitpix int, bzero, bscale float64) {
	if bitpix == 16 && bscale == 1 && bzero == fitsUnsignedBZERO {
		for i := range dst {
			dst[i] = binary.BigEndian.Uint16(src[2*i:]) ^ 0x8000
		}
		return
	}
	switch bitpix {
	case 8:
		// Not scaled up to fill 16 bits: CCD_BITSPERPIXEL reports 255 as the
		// full scale of an 8-bit readout, and scaling the pixels but not the
		// ceiling gives a caller a frame whose median is 256x its own maximum.
		for i := range dst {
			dst[i] = clampADU(float64(src[i])*bscale + bzero)
		}
	case 16:
		for i := range dst {
			dst[i] = clampADU(float64(int16(binary.BigEndian.Uint16(src[2*i:])))*bscale + bzero)
		}
	case 32:
		for i := range dst {
			dst[i] = clampADU(float64(int32(binary.BigEndian.Uint32(src[4*i:])))*bscale + bzero)
		}
	case -32:
		for i := range dst {
			dst[i] = clampADU(float64(math.Float32frombits(binary.BigEndian.Uint32(src[4*i:])))*bscale + bzero)
		}
	case -64:
		for i := range dst {
			dst[i] = clampADU(math.Float64frombits(binary.BigEndian.Uint64(src[8*i:]))*bscale + bzero)
		}
	}
}

// fitsSupported reports whether a BITPIX is one DecodeFITS reads. cfitsio emits
// 8, 16 and 32 for INDI's three bit depths; the float forms are here so a driver
// that writes one produces a frame rather than an error.
func fitsSupported(bitpix int) bool {
	switch bitpix {
	case 8, 16, 32, -32, -64:
		return true
	}
	return false
}

// clampADU rounds a physical sample into 16-bit ADU, saturating.
func clampADU(v float64) uint16 {
	switch {
	case v <= 0:
		return 0
	case v >= 65535:
		return 65535
	}
	return uint16(v + 0.5)
}

// scanFITSHeader validates SIMPLE, collects primary-header cards, and returns
// the data offset.
func scanFITSHeader(raw []byte) (map[string]string, int, error) {
	if len(raw) < fitsBlock || strings.TrimSpace(string(raw[:6])) != "SIMPLE" {
		return nil, 0, fmt.Errorf("indi: BLOB of %d bytes is not FITS — is CCD_TRANSFER_FORMAT set to FORMAT_FITS?",
			len(raw))
	}
	hdr := map[string]string{}
	for off := 0; off+fitsBlock <= len(raw); off += fitsBlock {
		for c := 0; c < fitsBlock; c += fitsCard {
			card := raw[off+c : off+c+fitsCard]
			key := strings.TrimSpace(string(card[:8]))
			if key == "END" {
				// Headers are padded, never packed, so the data starts at the
				// end of the block this END card sits in.
				return hdr, off + fitsBlock, nil
			}
			// COMMENT, HISTORY, blank and the occasional vendor card carry no
			// value indicator; skipping them keeps the map to assertions.
			if key == "" || card[8] != '=' {
				continue
			}
			hdr[key] = fitsCardValue(string(card[9:]))
		}
	}
	return nil, 0, fmt.Errorf("indi: FITS header has no END card in %d bytes", len(raw))
}

// fitsCardValue removes quoting and trailing comments, preserving slashes
// inside strings and decoding doubled quotes.
func fitsCardValue(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "'") {
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			if v[i] == '\'' {
				if i+1 < len(v) && v[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				break
			}
			b.WriteByte(v[i])
		}
		// FITS pads strings to at least 8 characters.
		return strings.TrimRight(b.String(), " ")
	}
	if i := strings.Index(v, "/"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// fitsBottomUp reads ROWORDER, reporting whether it was stated at all. An
// unrecognised value is no statement rather than a default.
func fitsBottomUp(hdr map[string]string) (bottomUp, stated bool) {
	v, ok := hdr["ROWORDER"]
	if !ok {
		return false, false
	}
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "BOTTOM-UP", "BOTTOMUP", "BOTTOM_UP":
		return true, true
	case "TOP-DOWN", "TOPDOWN", "TOP_DOWN":
		return false, true
	}
	return false, false
}

// hdrInt reads a numeric header value, returning def when absent or unparseable.
func hdrInt(h map[string]string, k string, def int) int {
	if v, ok := h[k]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// hdrFloat reads a floating-point header value, returning def when absent or
// unparseable.
func hdrFloat(h map[string]string, k string, def float64) float64 {
	if v, ok := h[k]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return def
}
