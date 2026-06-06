package wire

// Packet is a parsed ESND packet: the header plus its payload bytes. It is the
// unit the FEC interface trades in (README §6.3: Recover(p Packet)) and the
// receiver's reorder window holds (05 §5.6.2 step 5). The codec/fec packages
// (P3.2) import this type by its canonical name.
type Packet struct {
	Header  Header
	Payload []byte // codec frame, or FEC repair data (when Header.Flags.Repair())
}

// Repair reports whether this packet is an FEC repair packet (README §6.4 bit0).
func (p Packet) Repair() bool { return p.Header.Flags.Repair() }

// Keyframe reports whether this packet is cold-decodable (README §6.4 bit1).
func (p Packet) Keyframe() bool { return p.Header.Flags.Keyframe() }

// Clone returns a deep copy whose Payload is a freshly-allocated slice, so the
// result is safe to retain past the read buffer's lifetime (the reorder/FEC
// window keeps packets across reads — 05 §5.6.2). Header is a value type,
// copied by assignment. A nil Payload stays nil.
func (p Packet) Clone() Packet {
	c := Packet{Header: p.Header}
	if p.Payload != nil {
		c.Payload = make([]byte, len(p.Payload))
		copy(c.Payload, p.Payload)
	}
	return c
}

// Marshal serializes the packet; the header's PayloadLen is set from
// len(p.Payload). Thin wrapper over wire.Marshal so origin code can hold a
// Packet and emit bytes symmetrically.
func (p Packet) Marshal() ([]byte, error) {
	return Marshal(p.Header, p.Payload)
}
