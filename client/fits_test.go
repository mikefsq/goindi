package client_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/mikefsq/goindi/client"
)

// buildFITS assembles a primary-HDU payload from cards and raw big-endian data.
func buildFITS(cards []string, data []byte) []byte {
	var b strings.Builder
	write := func(c string) {
		if len(c) > 80 {
			c = c[:80]
		}
		b.WriteString(c + strings.Repeat(" ", 80-len(c)))
	}
	for _, c := range cards {
		write(c)
	}
	write("END")
	for b.Len()%2880 != 0 {
		b.WriteString(strings.Repeat(" ", 80))
	}
	out := append([]byte(b.String()), data...)
	for len(out)%2880 != 0 {
		out = append(out, 0)
	}
	return out
}

func card(key, val string) string { return fmt.Sprintf("%-8s= %s", key, val) }

// mono16 builds the payload every INDI camera sends at 16 bits: BITPIX=16 with
// BZERO=32768 carrying unsigned samples in a signed type.
func mono16(w, h int, pix []uint16, extra ...string) []byte {
	data := make([]byte, 2*len(pix))
	for i, v := range pix {
		binary.BigEndian.PutUint16(data[2*i:], v^0x8000) // stored signed
	}
	cards := append([]string{
		card("SIMPLE", "T"), card("BITPIX", "16"), card("NAXIS", "2"),
		card("NAXIS1", fmt.Sprint(w)), card("NAXIS2", fmt.Sprint(h)),
		card("BZERO", "32768"), card("BSCALE", "1"),
	}, extra...)
	return buildFITS(cards, data)
}

func TestDecodeFITSReadsUnsigned16(t *testing.T) {
	want := []uint16{0, 1, 32768, 65535}
	fr, err := client.DecodeFITS(mono16(2, 2, want))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Width != 2 || fr.Height != 2 {
		t.Errorf("size = %dx%d, want 2x2", fr.Width, fr.Height)
	}
	for i, w := range want {
		if fr.Pix[i] != w {
			t.Errorf("pix[%d] = %d, want %d", i, fr.Pix[i], w)
		}
	}
}

func TestDecodeFITSAppliesBZERO(t *testing.T) {
	raw := make([]byte, 2)
	binary.BigEndian.PutUint16(raw, 0x8000) // signed -32768, physical value 0
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "16"), card("NAXIS", "2"),
		card("NAXIS1", "1"), card("NAXIS2", "1"),
		card("BZERO", "32768"), card("BSCALE", "1"),
	}
	fr, err := client.DecodeFITS(buildFITS(cards, raw))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Pix[0] != 0 {
		t.Errorf("pix[0] = %d, want 0", fr.Pix[0])
	}
}

func TestDecodeFITSReadsBigEndian(t *testing.T) {
	fr, err := client.DecodeFITS(mono16(1, 1, []uint16{0x1234}))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Pix[0] != 0x1234 {
		t.Errorf("pix[0] = %#04x, want 0x1234", fr.Pix[0])
	}
}

func TestDecodeFITSRowOrder(t *testing.T) {
	pix := []uint16{10, 11, 20, 21} // row 0 then row 1
	top, err := client.DecodeFITS(mono16(2, 2, pix))
	if err != nil {
		t.Fatal(err)
	}
	if top.Pix[0] != 10 {
		t.Errorf("no ROWORDER: first sample = %d, want 10 (top-down)", top.Pix[0])
	}
	bot, err := client.DecodeFITS(mono16(2, 2, pix, card("ROWORDER", "'BOTTOM-UP'")))
	if err != nil {
		t.Fatal(err)
	}
	if bot.Pix[0] != 20 {
		t.Errorf("ROWORDER=BOTTOM-UP: first sample = %d, want 20", bot.Pix[0])
	}
}

func TestDecodeFITSBayerPhase(t *testing.T) {
	cases := []struct {
		pat        string
		extra      []string
		wantX      int
		wantY      int
		wantMosaic bool
	}{
		{pat: "RGGB", wantX: 0, wantY: 0, wantMosaic: true},
		{pat: "BGGR", wantX: 1, wantY: 1, wantMosaic: true},
		{pat: "GRBG", wantX: 1, wantY: 0, wantMosaic: true},
		{pat: "GBRG", wantX: 0, wantY: 1, wantMosaic: true},
		// A subframe on an odd column inverts the mosaic; the driver reports it
		// as an offset rather than by renaming the pattern.
		{pat: "RGGB", extra: []string{card("XBAYROFF", "1")}, wantX: 1, wantY: 0, wantMosaic: true},
		// A bottom-up flip inverts the phase on an even height.
		{pat: "RGGB", extra: []string{card("ROWORDER", "'BOTTOM-UP'")}, wantX: 0, wantY: 1, wantMosaic: true},
	}
	for _, tc := range cases {
		t.Run(tc.pat+strings.Join(tc.extra, ""), func(t *testing.T) {
			extra := append([]string{card("BAYERPAT", "'"+tc.pat+"'")}, tc.extra...)
			fr, err := client.DecodeFITS(mono16(2, 2, []uint16{1, 2, 3, 4}, extra...))
			if err != nil {
				t.Fatal(err)
			}
			if fr.Bayer != tc.wantMosaic {
				t.Fatalf("Bayer = %v, want %v", fr.Bayer, tc.wantMosaic)
			}
			if fr.BayerX != tc.wantX || fr.BayerY != tc.wantY {
				t.Errorf("phase = (%d,%d), want (%d,%d)", fr.BayerX, fr.BayerY, tc.wantX, tc.wantY)
			}
		})
	}
}

func TestDecodeFITSWithoutBayerPatIsMono(t *testing.T) {
	fr, err := client.DecodeFITS(mono16(2, 2, []uint16{1, 2, 3, 4}))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Bayer {
		t.Error("Bayer = true with no BAYERPAT card, want false")
	}
}

func TestDecodeFITSThreeAxisIsNotMosaiced(t *testing.T) {
	data := make([]byte, 2*4*3)
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "16"), card("NAXIS", "3"),
		card("NAXIS1", "2"), card("NAXIS2", "2"), card("NAXIS3", "3"),
		card("BZERO", "32768"), card("BSCALE", "1"), card("BAYERPAT", "'RGGB'"),
	}
	fr, err := client.DecodeFITS(buildFITS(cards, data))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Bayer {
		t.Error("Bayer = true on a 3-axis frame, want false")
	}
}

func TestDecodeFITS8BitIsNotScaledUp(t *testing.T) {
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "8"), card("NAXIS", "2"),
		card("NAXIS1", "2"), card("NAXIS2", "1"),
	}
	fr, err := client.DecodeFITS(buildFITS(cards, []byte{0, 255}))
	if err != nil {
		t.Fatal(err)
	}
	if fr.Pix[0] != 0 || fr.Pix[1] != 255 {
		t.Errorf("pix = %v, want [0 255]", fr.Pix[:2])
	}
}

func TestDecodeFITSFloatSaturates(t *testing.T) {
	data := make([]byte, 4*3)
	for i, v := range []float32{-5, 100.4, 1e9} {
		binary.BigEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "-32"), card("NAXIS", "2"),
		card("NAXIS1", "3"), card("NAXIS2", "1"),
	}
	fr, err := client.DecodeFITS(buildFITS(cards, data))
	if err != nil {
		t.Fatal(err)
	}
	want := []uint16{0, 100, 65535}
	for i, w := range want {
		if fr.Pix[i] != w {
			t.Errorf("pix[%d] = %d, want %d", i, fr.Pix[i], w)
		}
	}
}

func TestDecodeFITSCardValueKeepsSlashesInsideQuotes(t *testing.T) {
	fr, err := client.DecodeFITS(mono16(1, 1, []uint16{1},
		card("OBJECT", "'M42 / Orion'          / target")))
	if err != nil {
		t.Fatal(err)
	}
	if got := fr.Header["OBJECT"]; got != "M42 / Orion" {
		t.Errorf("OBJECT = %q, want %q", got, "M42 / Orion")
	}
}

func TestDecodeFITSRejectsNonFITS(t *testing.T) {
	if _, err := client.DecodeFITS(make([]byte, 4096)); err == nil {
		t.Error("DecodeFITS accepted a non-FITS payload, want an error")
	}
}

func TestDecodeFITSRejectsShortData(t *testing.T) {
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "16"), card("NAXIS", "2"),
		card("NAXIS1", "4096"), card("NAXIS2", "4096"),
		card("BZERO", "32768"), card("BSCALE", "1"),
	}
	if _, err := client.DecodeFITS(buildFITS(cards, []byte{1, 2, 3, 4})); err == nil {
		t.Error("DecodeFITS accepted 4 bytes for a 4096x4096 frame, want an error")
	}
}

func TestDecodeFITSRejectsUnsupportedBitpix(t *testing.T) {
	cards := []string{
		card("SIMPLE", "T"), card("BITPIX", "24"), card("NAXIS", "2"),
		card("NAXIS1", "1"), card("NAXIS2", "1"),
	}
	if _, err := client.DecodeFITS(buildFITS(cards, []byte{0, 0, 0})); err == nil {
		t.Error("DecodeFITS accepted BITPIX=24, want an error")
	}
}

func TestTrackRatesMatchLibindi(t *testing.T) {
	if got := client.TrackRateSidereal; math.Abs(got-15.041067179) > 1e-6 {
		t.Errorf("TrackRateSidereal = %v, want ~15.041067", got)
	}
	if got := client.TrackRateSolar; got != 15.0 {
		t.Errorf("TrackRateSolar = %v, want 15", got)
	}
	if client.TrackRateSidereal <= client.TrackRateSolar {
		t.Error("sidereal rate is not greater than solar")
	}
}
