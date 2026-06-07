package cluster

import (
	"encoding/json"
	"errors"

	"github.com/hashicorp/memberlist"

	"ensemble/internal/id"
)

// kind tags a NotifyMsg broadcast payload (first byte before the JSON).
const (
	kindNodeDelta byte = 'n' // one NodeRecord
	kindGroupName byte = 'g' // one group-name entry
	kindPlayback  byte = 'p' // one playback entry
	kindSettings  byte = 's' // one settings entry
)

// delta is the JSON body of a single-record broadcast: exactly one of the
// pointers is non-nil, selected by the leading kind byte. Group is the map key
// for the group-scoped kinds (ignored for nodes).
type delta struct {
	Group    id.ID                `json:"group,omitempty"`
	Node     *NodeRecord          `json:"node,omitempty"`
	Name     *GroupNameRecord     `json:"name,omitempty"`
	Playback *PlaybackRecord      `json:"playback,omitempty"`
	Settings *GroupSettingsRecord `json:"settings,omitempty"`
}

var errBadDelta = errors.New("cluster: empty delta payload")

// encodeDelta produces []byte{kind} ++ json(delta) for a NotifyMsg broadcast.
func encodeDelta(kind byte, d delta) []byte {
	body, _ := json.Marshal(d)
	out := make([]byte, 0, len(body)+1)
	out = append(out, kind)
	out = append(out, body...)
	return out
}

// decodeDelta parses a NotifyMsg payload back to (kind, delta).
func decodeDelta(msg []byte) (byte, delta, error) {
	if len(msg) < 1 {
		return 0, delta{}, errBadDelta
	}
	kind := msg[0]
	var d delta
	if err := json.Unmarshal(msg[1:], &d); err != nil {
		return kind, delta{}, err
	}
	return kind, d, nil
}

// broadcastKey returns the dedup key for a delta of the given kind: kind byte
// plus the record id, so a newer delta for the same record supersedes the older.
func broadcastKey(kind byte, recordID id.ID) string {
	return string(kind) + recordID.String()
}

// broadcast implements memberlist.Broadcast for a single delta.
type broadcast struct {
	key string
	msg []byte
}

func (b *broadcast) Invalidates(other memberlist.Broadcast) bool {
	o, ok := other.(*broadcast)
	return ok && o.key == b.key
}

func (b *broadcast) Message() []byte { return b.msg }

func (b *broadcast) Finished() {}
