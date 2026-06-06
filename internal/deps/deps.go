// Package deps is a build-time dependency anchor. It blank-imports each direct
// third-party dependency pinned in go.mod (Appendix A.11 + go-mp3) so the pins
// survive `go mod tidy` before any owning piece imports them for real, and so
// `go.sum` stays consistent from the very first commit (F0 ships only
// import-free package skeletons elsewhere).
//
// Each owning piece (config, stream/source, audio/sink, cluster, web, ...) will
// import these packages for real; as it does, the corresponding line here may be
// removed. This file exists ONLY to keep the F0 skeleton's module graph pinned
// and reproducible — it contains and must contain no logic.
package deps

import (
	_ "github.com/coder/websocket"           // UI live status push (A.11)
	_ "github.com/grandcat/zeroconf"         // mDNS discovery (A.11)
	_ "github.com/hajimehoshi/go-mp3"        // mp3 source decode (A.11)
	_ "github.com/hashicorp/memberlist"      // gossip membership (A.11)
	_ "github.com/mewkiz/flac"               // FLAC source decode (A.11)
	_ "golang.org/x/crypto/argon2"           // admin pw + PIN hash (A.11)
	_ "golang.org/x/crypto/chacha20poly1305" // adoption AEAD (A.11)
	_ "golang.org/x/crypto/hkdf"             // adoption key derivation (A.11)
	_ "golang.org/x/sys/unix"                // direct-ioctl ALSA sink (A.11, D12)
	_ "gopkg.in/yaml.v3"                     // YAML config (A.11)
)
