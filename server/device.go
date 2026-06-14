package server

import (
	"context"
	"strconv"
	"strings"
)

// Device is one INDI device hosted by a Server. Its methods are invoked by the
// server; the device owns no transport. It is the exact analogue of a goalpaca
// device: a thin adapter mapping a standard property set onto an underlying
// source-of-truth object.
type Device interface {
	// Name is the INDI device name — the addressing key on every message. It must
	// be stable across restarts (clients remember the selected device by name) and
	// unique within a Server.
	Name() string

	// Properties returns the device's property set, sent as def*Vectors when a
	// client issues getProperties.
	Properties() []*Property

	// HandleNew applies a client's new*Vector for the named property. members are
	// the supplied elements (their raw chardata values). The device mutates its
	// properties and publishes the resulting changes via pub; long actions should
	// run asynchronously so the read loop is not blocked.
	HandleNew(pub Publisher, name string, members []NewMember)
}

// Starter is an optional Device capability. Start runs background work — typically
// a status poll that publishes set*Vectors so clients see live values — until ctx
// is cancelled. The server calls it once, at startup, with the broadcast Publisher.
type Starter interface {
	Start(ctx context.Context, pub Publisher)
}

// NewMember is one element of an inbound new*Vector. Value is the raw chardata;
// the helpers interpret it per the target property's type.
type NewMember struct {
	Name  string
	Value string
}

// Float parses Value as a number (0 on failure).
func (m NewMember) Float() float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(m.Value), 64)
	return f
}

// On reports whether Value is the switch "On" token.
func (m NewMember) On() bool { return strings.EqualFold(strings.TrimSpace(m.Value), "On") }

// Publisher pushes property changes to every connected client. A device receives
// it both in HandleNew and (via Starter.Start) for background updates; in INDI,
// property changes are broadcast to all clients, not just the one that asked.
type Publisher interface {
	Define(p *Property)         // emit a def*Vector (new/structurally-changed property)
	Update(p *Property)         // emit a set*Vector (value/state change)
	Message(device, msg string) // emit a <message> log line
	Delete(device, name string) // emit a <delProperty>
	// SendBLOB emits a setBLOBVector with one element (e.g. a CCD frame as FITS) to
	// the clients that enabled BLOB delivery for it. format is e.g. ".fits".
	SendBLOB(device, name, elem, format string, data []byte)
}
