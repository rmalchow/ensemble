// Package group is the per-node group engine (piece H): follow/unfollow with
// validation, 10s self-heal, master-takeover orchestration, and master-side
// playback orchestration (§5/§6/§8). It consumes the already-derived groups
// from the cluster snapshot (D5 — it does NOT re-derive), points the local
// stream/clock/sink plumbing at the current master, and on the master runs the
// 20 ms release ticker that feeds the audio source server.
//
// All slog logging uses the component attribute "group".
package group
