// Package server is a clean-room, dependency-free implementation of the INDI
// wire protocol's server side: the XML framing, a property/device model, and a
// hub that multiplexes any number of devices over one TCP port (the conventional
// :7624) by the device name on every message — exactly what indiserver does
// internally, but in-process, with Go device objects instead of spawned binaries.
//
// It is the INDI analogue of goalpaca/server: a driver implements the Device
// interface and registers it; the same underlying hardware object (a lx200.Mount,
// an asicam camera) is the single source of truth, and this server is just one
// more native front-end onto it alongside Alpaca and the LX200 bridge.
package server

import "sync"

// PropType is the INDI property vector type.
type PropType int

const (
	NumberType PropType = iota
	SwitchType
	TextType
	LightType
	BLOBType
)

// State is the INDI property state (light), shared by a whole vector.
type State int

const (
	Idle State = iota
	Ok
	Busy
	Alert
)

func (s State) String() string {
	switch s {
	case Ok:
		return "Ok"
	case Busy:
		return "Busy"
	case Alert:
		return "Alert"
	default:
		return "Idle"
	}
}

// Perm is the client's access permission to a property.
type Perm int

const (
	RO Perm = iota // read-only (status)
	WO             // write-only (command)
	RW             // read-write
)

func (p Perm) String() string {
	switch p {
	case WO:
		return "wo"
	case RW:
		return "rw"
	default:
		return "ro"
	}
}

// SwitchRule constrains how many switch members may be On at once.
type SwitchRule int

const (
	OneOfMany SwitchRule = iota // exactly one On (radio)
	AtMostOne                   // zero or one On
	AnyOfMany                   // any number On (checkboxes)
)

func (r SwitchRule) String() string {
	switch r {
	case AtMostOne:
		return "AtMostOne"
	case AnyOfMany:
		return "AnyOfMany"
	default:
		return "OneOfMany"
	}
}

// Member is one element of a property vector. Only the fields relevant to the
// owning property's Type are meaningful.
type Member struct {
	Name  string
	Label string

	// NumberType
	Format         string
	Min, Max, Step float64
	Num            float64

	// SwitchType
	On bool

	// TextType
	Text string

	// LightType
	Light State
}

// Property is an INDI property vector: a named group of members with a shared
// state and permission. It is the unit a client defines/sets and a device
// updates. Safe for concurrent use by a device's command handler and its
// background poll loop.
type Property struct {
	Device  string
	Name    string
	Label   string
	Group   string
	Type    PropType
	Perm    Perm
	Rule    SwitchRule // SwitchType only
	Timeout int

	mu      sync.Mutex
	state   State
	members []*Member
}

// NewProperty builds a property with the given members (state defaults to Idle).
func NewProperty(device, name string, t PropType, perm Perm, members ...*Member) *Property {
	return &Property{Device: device, Name: name, Type: t, Perm: perm, members: members}
}

func (p *Property) State() State     { p.mu.Lock(); defer p.mu.Unlock(); return p.state }
func (p *Property) SetState(s State) { p.mu.Lock(); p.state = s; p.mu.Unlock() }

// member returns the named member; caller holds mu.
func (p *Property) member(name string) *Member {
	for _, m := range p.members {
		if m.Name == name {
			return m
		}
	}
	return nil
}

func (p *Property) SetNumber(name string, v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		m.Num = v
	}
}

func (p *Property) Number(name string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		return m.Num
	}
	return 0
}

// SetSwitch sets a switch member. For a OneOfMany vector, turning one On turns the
// rest Off, preserving the radio invariant.
func (p *Property) SetSwitch(name string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Rule == OneOfMany && on {
		for _, m := range p.members {
			m.On = m.Name == name
		}
		return
	}
	if m := p.member(name); m != nil {
		m.On = on
	}
}

func (p *Property) Switch(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		return m.On
	}
	return false
}

func (p *Property) SetText(name, s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		m.Text = s
	}
}

// snapshot returns the state and a value-copy of the members under lock, for the
// marshaler to render without racing a concurrent device update.
func (p *Property) snapshot() (State, []Member) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ms := make([]Member, len(p.members))
	for i, m := range p.members {
		ms[i] = *m
	}
	return p.state, ms
}
