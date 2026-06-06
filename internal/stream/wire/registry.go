package wire

// This file owns the name<->id registry (README §6.5, 07 §6): "String enums in
// JSON; integer ids only on the wire … mapped to/from these names via a name<->id
// registry." wire owns it because it owns the wire boundary. The mappings are
// fixed, total, O(1), and pure: every lookup returns a (value, known bool) — never
// a panic, never a default-to-PCM guess. An unknown name is a negotiation/config
// bug the caller must surface; an unknown id is a foreign packet the receiver
// drops (05 §5.6.3).

// CodecID is the integer wire codec id (README §6.3). It exists ONLY on the wire
// (header offset 6); JSON/ConfigDoc/API always use the string name (07 §6).
type CodecID uint8

const (
	CodecPCM  CodecID = 0 // S16LE, mandatory baseline (05 §5.4.1, A.10 m5)
	CodecOpus CodecID = 1 // optional, capability-gated (05 §5.4.2)
)

// FECID is the integer wire FEC id (README §6.3). Wire-only, like CodecID.
type FECID uint8

const (
	FECNone      FECID = 0 // identity (05 §5.5, §5.9 TCP path)
	FECXORParity FECID = 1 // default (05 §5.5.1)
	FECDuplicate FECID = 2 // MCU-friendly alt (05 §5.5.4)
)

// codecNames is the canonical id->string table (README §6.5). The reverse
// direction is derived from it so the two can never disagree.
var codecNames = map[CodecID]string{
	CodecPCM:  "pcm",
	CodecOpus: "opus",
}

var fecNames = map[FECID]string{
	FECNone:      "none",
	FECXORParity: "xorParity",
	FECDuplicate: "duplicate",
}

// CodecName maps a CodecID to its canonical JSON string ("pcm"|"opus"). Returns
// ("", false) for an unknown id (a malformed/foreign packet — caller drops it).
func CodecName(id CodecID) (string, bool) {
	name, ok := codecNames[id]
	return name, ok
}

// ParseCodec maps a JSON/ConfigDoc string ("pcm"|"opus") to its CodecID. Returns
// (0, false) on an unknown name (negotiation/profile bug — caller surfaces it).
func ParseCodec(name string) (CodecID, bool) {
	for id, n := range codecNames {
		if n == name {
			return id, true
		}
	}
	return 0, false
}

// FECName maps a FECID to its canonical JSON string
// ("none"|"xorParity"|"duplicate"). Returns ("", false) for an unknown id.
func FECName(id FECID) (string, bool) {
	name, ok := fecNames[id]
	return name, ok
}

// ParseFEC maps a JSON/ConfigDoc string ("none"|"xorParity"|"duplicate") to its
// FECID. Returns (0, false) on an unknown name.
func ParseFEC(name string) (FECID, bool) {
	for id, n := range fecNames {
		if n == name {
			return id, true
		}
	}
	return 0, false
}
