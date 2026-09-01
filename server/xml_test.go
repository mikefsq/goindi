package server

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"testing"
)

// TestBlobSetXMLEscapesAttrs checks that blobSetXML escapes XML-special
// characters in attribute values.
func TestBlobSetXMLEscapesAttrs(t *testing.T) {
	const device = `R&D "Scope"`
	const format = `.fits<&>'`
	data := []byte{0, 1, 2, 3, 4}
	raw := blobSetXML(device, "CCD1", "CCD1", format, data, len(data), now())

	var v struct {
		XMLName xml.Name `xml:"setBLOBVector"`
		Device  string   `xml:"device,attr"`
		Name    string   `xml:"name,attr"`
		OneBLOB struct {
			Name   string `xml:"name,attr"`
			Size   int    `xml:"size,attr"`
			Format string `xml:"format,attr"`
			Data   string `xml:",chardata"`
		} `xml:"oneBLOB"`
	}
	if err := xml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("blobSetXML output does not parse: %v\n%s", err, raw)
	}
	if v.Device != device {
		t.Errorf("device attr round-trip: got %q, want %q", v.Device, device)
	}
	if v.OneBLOB.Format != format {
		t.Errorf("format attr round-trip: got %q, want %q", v.OneBLOB.Format, format)
	}
	if v.OneBLOB.Size != len(data) {
		t.Errorf("size attr: got %d, want %d", v.OneBLOB.Size, len(data))
	}
	got, err := base64.StdEncoding.DecodeString(v.OneBLOB.Data)
	if err != nil {
		t.Fatalf("payload does not base64-decode: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("payload round-trip: got %v, want %v", got, data)
	}
}
