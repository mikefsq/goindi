package client

import (
	"encoding/xml"
	"strings"
)

type wGetProperties struct {
	XMLName xml.Name `xml:"getProperties"`
	Version string   `xml:"version,attr"`
	Device  string   `xml:"device,attr,omitempty"`
	Name    string   `xml:"name,attr,omitempty"`
}

type wOne struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

// wNewVector marshals to newNumberVector, newSwitchVector or newTextVector per
// its XMLName; only the matching child slice is populated.
type wNewVector struct {
	XMLName  xml.Name
	Device   string `xml:"device,attr"`
	Name     string `xml:"name,attr"`
	Numbers  []wOne `xml:"oneNumber,omitempty"`
	Switches []wOne `xml:"oneSwitch,omitempty"`
	Texts    []wOne `xml:"oneText,omitempty"`
}

type wEnableBLOB struct {
	XMLName xml.Name `xml:"enableBLOB"`
	Device  string   `xml:"device,attr,omitempty"`
	Name    string   `xml:"name,attr,omitempty"`
	Mode    string   `xml:",chardata"`
}

type wDefMember struct {
	Name   string `xml:"name,attr"`
	Label  string `xml:"label,attr"`
	Format string `xml:"format,attr"`
	Min    string `xml:"min,attr"`
	Max    string `xml:"max,attr"`
	Step   string `xml:"step,attr"`
	Value  string `xml:",chardata"`
}

// wDefVector decodes any def*Vector; the element name, carried separately,
// selects the property type.
type wDefVector struct {
	Device   string       `xml:"device,attr"`
	Name     string       `xml:"name,attr"`
	Label    string       `xml:"label,attr"`
	Group    string       `xml:"group,attr"`
	State    string       `xml:"state,attr"`
	Perm     string       `xml:"perm,attr"`
	Rule     string       `xml:"rule,attr"`
	Numbers  []wDefMember `xml:"defNumber"`
	Switches []wDefMember `xml:"defSwitch"`
	Texts    []wDefMember `xml:"defText"`
	Lights   []wDefMember `xml:"defLight"`
	BLOBs    []wDefMember `xml:"defBLOB"`
}

type wSetVector struct {
	Device   string `xml:"device,attr"`
	Name     string `xml:"name,attr"`
	State    string `xml:"state,attr"`
	Message  string `xml:"message,attr"`
	Numbers  []wOne `xml:"oneNumber"`
	Switches []wOne `xml:"oneSwitch"`
	Texts    []wOne `xml:"oneText"`
	Lights   []wOne `xml:"oneLight"`
}

type wDelProperty struct {
	Device string `xml:"device,attr"`
	Name   string `xml:"name,attr"`
}

type wMessage struct {
	Device    string `xml:"device,attr"`
	Timestamp string `xml:"timestamp,attr"`
	Message   string `xml:"message,attr"`
}

func typeOf(elem string) string {
	switch {
	case strings.Contains(elem, "Number"):
		return "Number"
	case strings.Contains(elem, "Switch"):
		return "Switch"
	case strings.Contains(elem, "Text"):
		return "Text"
	case strings.Contains(elem, "Light"):
		return "Light"
	case strings.Contains(elem, "BLOB"):
		return "BLOB"
	}
	return ""
}

// atof discards ParseNumber's ok result, for callers that read a missing or
// malformed value as zero.
func atof(s string) float64 {
	f, _ := ParseNumber(s)
	return f
}

func isOn(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "On") }

func onoff(b bool) string {
	if b {
		return "On"
	}
	return "Off"
}
