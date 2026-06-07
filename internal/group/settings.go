package group

import (
	"ensemble/internal/contracts"
	"ensemble/internal/stream"
)

// settings bounds (§9.1).
const (
	minBufferMs = 20
	maxBufferMs = 2000
)

// defaultSettings is the cluster-wide default group settings (§8.5).
func defaultSettings() contracts.GroupSettings {
	return contracts.GroupSettings{
		Codec:     contracts.DefaultCodec,
		Transport: contracts.DefaultTransport,
		BufferMs:  contracts.DefaultBufferMs,
	}
}

// fillDefaults replaces empty/zero fields with the defaults (§8.5).
func fillDefaults(s contracts.GroupSettings) contracts.GroupSettings {
	if s.Codec == "" {
		s.Codec = contracts.DefaultCodec
	}
	if s.Transport == "" {
		s.Transport = contracts.DefaultTransport
	}
	if s.BufferMs == 0 {
		s.BufferMs = contracts.DefaultBufferMs
	}
	return s
}

// validateSettings normalizes + validates group settings (§8.3/§8.4/§9.1).
// codec ∈ {pcm,opus} (opus requires caps.Codecs to list it); transport ∈
// {udp,tcp}; bufferMs clamped to [20,2000], 0 → default. Unknown codec/transport
// → ErrBadSettings.
func validateSettings(s contracts.GroupSettings, caps contracts.Capabilities) (contracts.GroupSettings, error) {
	s = fillDefaults(s)

	switch s.Codec {
	case "pcm":
	case "opus":
		if !hasCodec(caps.Codecs, "opus") {
			return contracts.GroupSettings{}, ErrNoOpus
		}
	default:
		return contracts.GroupSettings{}, ErrBadSettings
	}

	switch s.Transport {
	case "udp", "tcp":
	default:
		return contracts.GroupSettings{}, ErrBadSettings
	}

	if s.BufferMs < minBufferMs {
		s.BufferMs = minBufferMs
	}
	if s.BufferMs > maxBufferMs {
		s.BufferMs = maxBufferMs
	}
	return s, nil
}

// SetSettings validates + writes group settings (master-only, §9.1) and applies
// them LIVE (D23): bump generation, write the settings record, re-arm the source
// ring, broadcast RECONFIG so subscribers re-read + resubscribe, and re-point the
// local plumbing to (self,newGen). If a session is running it re-stamps under the
// new gen mid-stream.
func (e *Engine) SetSettings(s contracts.GroupSettings) error {
	v, err := validateSettings(s, e.p.Caps)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	snap := e.p.Cluster.Snapshot()
	mv := myGroup(snap, e.self)
	if !mv.found || mv.role == roleFollower {
		return ErrNotMaster
	}
	groupID := mv.group.ID

	e.p.Cluster.SetGroupSettings(groupID, v)
	e.log.Info("group settings applied", "group", groupID.String(),
		"codec", v.Codec, "transport", v.Transport, "bufferMs", v.BufferMs,
		"live", e.sess != nil)

	if e.sess == nil {
		return nil // idle: record only; next Play uses it
	}

	// Live apply: bump gen, re-arm the ring under the new gen, broadcast RECONFIG.
	e.gen++
	gen := e.gen
	e.sess.gen.Store(gen)
	e.sess.transport = v.Transport
	e.sess.bufferMs = v.BufferMs
	// Note: codec changes do not rebuild the running encoder mid-session — a codec
	// change takes effect at the next Play. Transport/bufferMs apply live.
	e.p.Source.StartSession(gen, stream.ParseTransport(v.Transport), v.BufferMs)
	e.repointLocked(mv.master, gen, v.Transport, true)
	return nil
}
