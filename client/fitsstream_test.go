package client_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/mikefsq/goindi/client"
)

// streamDecode feeds a payload through FITSWriter in fixed-size chunks.
func streamDecode(t *testing.T, blob []byte, chunk int) (*client.Frame, error) {
	t.Helper()
	w := client.NewFITSWriter()
	for off := 0; off < len(blob); off += chunk {
		end := off + chunk
		if end > len(blob) {
			end = len(blob)
		}
		if _, err := w.Write(blob[off:end]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return w.Frame()
}

func TestFITSWriterAgreesWithDecodeFITS(t *testing.T) {
	pix := make([]uint16, 16*9)
	for i := range pix {
		pix[i] = uint16(i * 251)
	}
	for _, extra := range [][]string{
		nil,
		{card("BAYERPAT", "'GRBG'"), card("XBAYROFF", "1")},
		{card("ROWORDER", "'BOTTOM-UP'")},
	} {
		blob := mono16(16, 9, pix, extra...)
		want, err := client.DecodeFITS(blob)
		if err != nil {
			t.Fatal(err)
		}
		// 1 exercises a row spanning many writes; a prime avoids aligning with
		// the 32-byte row or the 2880-byte block.
		for _, chunk := range []int{1, 7, 31, 2880, 4096, len(blob)} {
			got, err := streamDecode(t, blob, chunk)
			if err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
			if got.Width != want.Width || got.Height != want.Height {
				t.Errorf("chunk %d: size = %dx%d, want %dx%d", chunk, got.Width, got.Height, want.Width, want.Height)
			}
			if !bytes.Equal(u16bytes(got.Pix), u16bytes(want.Pix)) {
				t.Errorf("chunk %d: pixels differ from DecodeFITS", chunk)
			}
			if got.Bayer != want.Bayer || got.BayerX != want.BayerX || got.BayerY != want.BayerY {
				t.Errorf("chunk %d: mosaic = %v(%d,%d), want %v(%d,%d)", chunk,
					got.Bayer, got.BayerX, got.BayerY, want.Bayer, want.BayerX, want.BayerY)
			}
		}
	}
}

func u16bytes(p []uint16) []byte {
	b := make([]byte, 2*len(p))
	for i, v := range p {
		b[2*i], b[2*i+1] = byte(v), byte(v>>8)
	}
	return b
}

func TestFITSWriterRejectsAShortPayload(t *testing.T) {
	// Cut inside the DATA, not the block padding: mono16 pads the payload to a
	// 2880 multiple, so lopping bytes off the end of the blob removes padding and
	// loses no rows at all. The header is one block and each row is 32 bytes, so
	// this delivers eight of nine rows.
	blob := mono16(16, 9, make([]uint16, 16*9))
	w := client.NewFITSWriter()
	if _, err := w.Write(blob[:2880+32*8]); err != nil {
		t.Fatal(err)
	}
	err := w.Close()
	if !errors.Is(err, client.ErrIncompleteFITS) {
		t.Errorf("Close() = %v, want ErrIncompleteFITS", err)
	}
	if _, err := w.Frame(); err == nil {
		t.Error("Frame() succeeded on an incomplete payload")
	}
}

func TestFITSWriterRejectsNonFITSEarly(t *testing.T) {
	w := client.NewFITSWriter()
	junk := make([]byte, 4096)
	if _, err := w.Write(junk); err == nil {
		t.Error("Write accepted a non-FITS payload, want an error on the first block")
	}
}

func TestFITSWriterRejectsAnEmptyPayload(t *testing.T) {
	w := client.NewFITSWriter()
	if err := w.Close(); err == nil {
		t.Error("Close() on an empty payload succeeded, want an error")
	}
}

func TestFITSWriterFrameBeforeCloseFails(t *testing.T) {
	w := client.NewFITSWriter()
	if _, err := w.Write(mono16(2, 2, []uint16{1, 2, 3, 4})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Frame(); err == nil {
		t.Error("Frame() before Close() succeeded")
	}
}

// Both benchmarks include payload accumulation and decoding into a Frame.
func benchChunks(blob []byte) [][]byte {
	var out [][]byte
	for off := 0; off < len(blob); off += 64 << 10 {
		end := off + 64<<10
		if end > len(blob) {
			end = len(blob)
		}
		out = append(out, blob[off:end])
	}
	return out
}

func BenchmarkFITSBuffered(b *testing.B) {
	blob := mono16(2048, 2048, make([]uint16, 2048*2048))
	chunks := benchChunks(blob)
	b.SetBytes(int64(len(blob)))
	b.ReportAllocs()
	for b.Loop() {
		buf := make([]byte, 0, len(blob)) // what blobBuf does, pre-sized from the size attr
		for _, c := range chunks {
			buf = append(buf, c...)
		}
		if _, err := client.DecodeFITS(buf); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFITSStreamed(b *testing.B) {
	blob := mono16(2048, 2048, make([]uint16, 2048*2048))
	chunks := benchChunks(blob)
	b.SetBytes(int64(len(blob)))
	b.ReportAllocs()
	for b.Loop() {
		w := client.NewFITSWriter()
		for _, c := range chunks {
			if _, err := w.Write(c); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
		if _, err := w.Frame(); err != nil {
			b.Fatal(err)
		}
	}
}
