package server

import (
	"context"
	"strings"
)

// Device is one INDI device hosted by a Server.
type Device interface {
	// Name is the INDI device name, the addressing key on every message; it must
	// be unique within a Server and stable across restarts.
	Name() string

	// Properties returns the device's property set.
	Properties() []*Property

	// HandleNew applies a client's new*Vector for the named property; long actions
	// must run asynchronously so the connection's read loop is not blocked.
	HandleNew(pub Publisher, name string, members []NewMember)
}

// Starter is an optional Device capability whose Start runs background work,
// typically a status poll, until ctx is cancelled.
type Starter interface {
	Start(ctx context.Context, pub Publisher)
}

// NewMember is one element of an inbound new*Vector, carrying its raw chardata.
type NewMember struct {
	Name  string
	Value string
}

// Float parses Value as an INDI number, plain decimal or sexagesimal, reporting
// ok=false on malformed input so a bad RA is refused rather than slewing to 0.
func (m NewMember) Float() (f float64, ok bool) {
	return ParseNumber(m.Value)
}

// On reports whether Value is the switch "On" token.
func (m NewMember) On() bool { return strings.EqualFold(strings.TrimSpace(m.Value), "On") }

// Publisher pushes property changes to every connected client, not only the one
// that asked.
type Publisher interface {
	// Define emits a def*Vector for a new or structurally changed property.
	Define(p *Property)
	// Update emits a set*Vector for a value or state change.
	Update(p *Property)
	// Message emits a <message> log line.
	Message(device, msg string)
	// Delete emits a <delProperty>.
	Delete(device, name string)
	// SendBLOB emits a setBLOBVector with one element of the given format (e.g.
	// ".fits") to the clients that enabled BLOB delivery for it.
	SendBLOB(device, name, elem, format string, data []byte)
}

// BLOBSizedSender is an optional Publisher extension for pre-compressed payloads,
// letting the caller supply the uncompressed size instead of making the hub
// inflate the data to compute it.
type BLOBSizedSender interface {
	SendBLOBSized(device, name, elem, format string, data []byte, uncompressedSize int)
}
