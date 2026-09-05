// Package server implements the INDI wire protocol's server side: XML framing, a
// property/device model, and a hub that multiplexes devices over one TCP port.
package server

import (
	"strconv"
	"sync"
)

// PropType is the INDI property vector type.
type PropType int

const (
	NumberType PropType = iota
	SwitchType
	TextType
	LightType
	BLOBType
)

func (t PropType) String() string {
	switch t {
	case NumberType:
		return "Number"
	case SwitchType:
		return "Switch"
	case TextType:
		return "Text"
	case LightType:
		return "Light"
	case BLOBType:
		return "BLOB"
	}
	return "PropType(" + strconv.Itoa(int(t)) + ")"
}

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

// Property is an INDI vector with shared state and permissions.
// Its value and state methods are synchronized. Set exported metadata before
// publishing and do not mutate the supplied Member pointers concurrently.
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

// NewProperty builds a property with the given members, in state Idle.
func NewProperty(device, name string, t PropType, perm Perm, members ...*Member) *Property {
	return &Property{Device: device, Name: name, Type: t, Perm: perm, members: members}
}

// State reports the vector's state.
func (p *Property) State() State { p.mu.Lock(); defer p.mu.Unlock(); return p.state }

// SetState sets the vector's state.
func (p *Property) SetState(s State) { p.mu.Lock(); p.state = s; p.mu.Unlock() }

// member returns the named member; the caller holds mu.
func (p *Property) member(name string) *Member {
	for _, m := range p.members {
		if m.Name == name {
			return m
		}
	}
	return nil
}

// SetNumber sets a number member's value, ignoring an unknown name.
func (p *Property) SetNumber(name string, v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		m.Num = v
	}
}

// Number returns a number member's value, or 0 if there is no such member.
func (p *Property) Number(name string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		return m.Num
	}
	return 0
}

// SetSwitch sets a switch member, enforcing the vector's rule: OneOfMany and
// AtMostOne turn the other members Off, and OneOfMany refuses an Off that would
// leave none On.
func (p *Property) SetSwitch(name string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.member(name)
	if m == nil {
		return
	}
	switch {
	case on && (p.Rule == OneOfMany || p.Rule == AtMostOne):
		for _, o := range p.members {
			o.On = o == m
		}
	case !on && p.Rule == OneOfMany:
		for _, o := range p.members {
			if o != m && o.On {
				m.On = false
				return
			}
		}
		// Falling out of the loop means m is the only member On: refuse.
	default:
		m.On = on
	}
}

// Switch reports whether a switch member is On.
func (p *Property) Switch(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		return m.On
	}
	return false
}

// SetText sets a text member's value, ignoring an unknown name.
func (p *Property) SetText(name, s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.member(name); m != nil {
		m.Text = s
	}
}

// snapshot copies the state and members under lock so the marshaler cannot race a
// concurrent device update.
func (p *Property) snapshot() (State, []Member) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ms := make([]Member, len(p.members))
	for i, m := range p.members {
		ms[i] = *m
	}
	return p.state, ms
}
