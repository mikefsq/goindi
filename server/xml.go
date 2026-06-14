package server

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// INDI is a stream of top-level XML elements over a socket — no enclosing root
// document and no length framing. This file defines the element structs and the
// marshal helpers; the streaming read loop lives in server.go (an xml.Decoder
// Token loop, since the stream is not a single document).

// ---- shared one-member elements (in both directions) ----

type xoneNumber struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type xoneSwitch struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}
type xoneText struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

// ---- outbound: def*Vector (full property definition) ----

type xdefNumber struct {
	Name   string `xml:"name,attr"`
	Label  string `xml:"label,attr,omitempty"`
	Format string `xml:"format,attr"`
	Min    string `xml:"min,attr"`
	Max    string `xml:"max,attr"`
	Step   string `xml:"step,attr"`
	Value  string `xml:",chardata"`
}
type xdefSwitch struct {
	Name  string `xml:"name,attr"`
	Label string `xml:"label,attr,omitempty"`
	Value string `xml:",chardata"`
}
type xdefText struct {
	Name  string `xml:"name,attr"`
	Label string `xml:"label,attr,omitempty"`
	Value string `xml:",chardata"`
}
type xdefBLOB struct {
	Name  string `xml:"name,attr"`
	Label string `xml:"label,attr,omitempty"`
}

type xdefVector struct {
	XMLName   xml.Name
	Device    string       `xml:"device,attr"`
	Name      string       `xml:"name,attr"`
	Label     string       `xml:"label,attr,omitempty"`
	Group     string       `xml:"group,attr,omitempty"`
	State     string       `xml:"state,attr"`
	Perm      string       `xml:"perm,attr,omitempty"`
	Rule      string       `xml:"rule,attr,omitempty"`
	Timeout   int          `xml:"timeout,attr,omitempty"`
	Timestamp string       `xml:"timestamp,attr,omitempty"`
	Numbers   []xdefNumber `xml:"defNumber,omitempty"`
	Switches  []xdefSwitch `xml:"defSwitch,omitempty"`
	Texts     []xdefText   `xml:"defText,omitempty"`
	BLOBs     []xdefBLOB   `xml:"defBLOB,omitempty"`
}

// ---- outbound: set*Vector (value/state update) ----

type xsetVector struct {
	XMLName   xml.Name
	Device    string       `xml:"device,attr"`
	Name      string       `xml:"name,attr"`
	State     string       `xml:"state,attr,omitempty"`
	Timeout   int          `xml:"timeout,attr,omitempty"`
	Timestamp string       `xml:"timestamp,attr,omitempty"`
	Numbers   []xoneNumber `xml:"oneNumber,omitempty"`
	Switches  []xoneSwitch `xml:"oneSwitch,omitempty"`
	Texts     []xoneText   `xml:"oneText,omitempty"`
}

type xmessage struct {
	XMLName   xml.Name `xml:"message"`
	Device    string   `xml:"device,attr,omitempty"`
	Timestamp string   `xml:"timestamp,attr,omitempty"`
	Message   string   `xml:"message,attr"`
}

type xdelProperty struct {
	XMLName   xml.Name `xml:"delProperty"`
	Device    string   `xml:"device,attr"`
	Name      string   `xml:"name,attr,omitempty"`
	Timestamp string   `xml:"timestamp,attr,omitempty"`
}

// ---- inbound: client → server ----

type xgetProperties struct {
	Device string `xml:"device,attr"`
	Name   string `xml:"name,attr"`
}

// xnewVector covers newNumberVector / newSwitchVector / newTextVector; only the
// child list matching the element is populated by the decoder.
type xnewVector struct {
	Device   string       `xml:"device,attr"`
	Name     string       `xml:"name,attr"`
	Numbers  []xoneNumber `xml:"oneNumber"`
	Switches []xoneSwitch `xml:"oneSwitch"`
	Texts    []xoneText   `xml:"oneText"`
}

type xenableBLOB struct {
	Device string `xml:"device,attr"`
	Name   string `xml:"name,attr"`
	Value  string `xml:",chardata"`
}

// ---- marshal helpers ----

func marshalDef(p *Property, ts string) ([]byte, error) {
	st, members := p.snapshot()
	v := xdefVector{
		Device: p.Device, Name: p.Name, Label: p.Label, Group: p.Group,
		State: st.String(), Perm: p.Perm.String(), Timeout: p.Timeout, Timestamp: ts,
	}
	switch p.Type {
	case NumberType:
		v.XMLName = xml.Name{Local: "defNumberVector"}
		for _, m := range members {
			v.Numbers = append(v.Numbers, xdefNumber{
				Name: m.Name, Label: m.Label, Format: orFmt(m.Format),
				Min: numAttr(m.Min), Max: numAttr(m.Max), Step: numAttr(m.Step),
				Value: numVal(m.Num),
			})
		}
	case SwitchType:
		v.XMLName = xml.Name{Local: "defSwitchVector"}
		v.Rule = p.Rule.String()
		for _, m := range members {
			v.Switches = append(v.Switches, xdefSwitch{Name: m.Name, Label: m.Label, Value: onoff(m.On)})
		}
	case TextType:
		v.XMLName = xml.Name{Local: "defTextVector"}
		for _, m := range members {
			v.Texts = append(v.Texts, xdefText{Name: m.Name, Label: m.Label, Value: m.Text})
		}
	case BLOBType:
		v.XMLName = xml.Name{Local: "defBLOBVector"}
		for _, m := range members {
			v.BLOBs = append(v.BLOBs, xdefBLOB{Name: m.Name, Label: m.Label})
		}
	default:
		return nil, fmt.Errorf("indi: cannot define property type %d", p.Type)
	}
	return xml.Marshal(v)
}

// blobSetXML builds a setBLOBVector by hand (not via encoding/xml) — the base64
// payload is large and the inputs are controlled, so this avoids marshaling a
// multi-megabyte chardata string. format is e.g. ".fits"; size is the raw byte count.
func blobSetXML(device, name, elem, format string, data []byte, ts string) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	b.Grow(len(enc) + 256)
	fmt.Fprintf(&b, `<setBLOBVector device=%q name=%q state="Ok" timestamp=%q>`, device, name, ts)
	fmt.Fprintf(&b, `<oneBLOB name=%q size="%d" format=%q>`, elem, len(data), format)
	b.WriteString(enc)
	b.WriteString(`</oneBLOB></setBLOBVector>`)
	return []byte(b.String())
}

func marshalSet(p *Property, ts string) ([]byte, error) {
	st, members := p.snapshot()
	v := xsetVector{Device: p.Device, Name: p.Name, State: st.String(), Timestamp: ts}
	switch p.Type {
	case NumberType:
		v.XMLName = xml.Name{Local: "setNumberVector"}
		for _, m := range members {
			v.Numbers = append(v.Numbers, xoneNumber{Name: m.Name, Value: numVal(m.Num)})
		}
	case SwitchType:
		v.XMLName = xml.Name{Local: "setSwitchVector"}
		for _, m := range members {
			v.Switches = append(v.Switches, xoneSwitch{Name: m.Name, Value: onoff(m.On)})
		}
	case TextType:
		v.XMLName = xml.Name{Local: "setTextVector"}
		for _, m := range members {
			v.Texts = append(v.Texts, xoneText{Name: m.Name, Value: m.Text})
		}
	default:
		return nil, fmt.Errorf("indi: cannot set property type %d", p.Type)
	}
	return xml.Marshal(v)
}

func numVal(f float64) string  { return strconv.FormatFloat(f, 'f', 6, 64) }
func numAttr(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
func onoff(b bool) string {
	if b {
		return "On"
	}
	return "Off"
}
func orFmt(s string) string {
	if s == "" {
		return "%g"
	}
	return s
}
