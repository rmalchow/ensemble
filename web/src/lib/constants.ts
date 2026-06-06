// A.12 starting parameters surfaced in the UI. Quoted verbatim from Appendix
// A.12 — this is a display/default mirror, NOT a redefinition. The protocol
// source of truth is the Go side; the scaffold hard-codes only the two values
// the Setup/Cluster screens need before any config is loaded.

// CONTROL_PORT is the control-plane mTLS port (also the dev-proxy target).
export const CONTROL_PORT = 8443
// CLOCK_PORT / AUDIO_PORT are surfaced for the Cluster screen's network panel.
export const CLOCK_PORT = 9000
export const AUDIO_PORT = 9100

// ADOPTION_PIN_DEFAULT is the placeholder adoption PIN (A.12 / D9). Treated as a
// real secret by the Setup/Cluster screens — never logged, never defaulted into
// a submitted form silently.
export const ADOPTION_PIN_DEFAULT = '0000'

// Canonical audio profile defaults (A.12), used only for display placeholders.
export const CANONICAL_RATE = 48000
export const CANONICAL_CHANNELS = 2
export const FRAMES_PER_CHUNK = 480 // 10 ms @ 48 kHz
export const LEAD_MS = 300
export const MAX_PPM = 200
export const HARD_ERR_SAMPLES = 2400 // 50 ms @ 48 kHz
export const FEC_K = 8
export const FEC_INTERLEAVE = 4
export const FEC_DDUP = 5
