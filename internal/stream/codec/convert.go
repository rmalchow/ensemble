package codec

// f32ToS16LE converts interleaved float32 samples in [-1,1] to little-endian
// signed 16-bit PCM, writing 2 bytes per sample into dst (len(dst) must be
// 2*len(src)). Out-of-range inputs are clamped to [-1,1] so a hot sample never
// wraps to the opposite rail. Scale is 32767 (symmetric, never overflows the
// +1.0 rail into +32768). It is a pure function so it is table-tested directly.
//
// Copied verbatim (loop body) from ../media internal/audio/sink_exec.go; moved
// into package codec and kept unexported. The encoder uses 32767 while the
// decoder (s16LEToF32) uses 1/32768 — see s16LEToF32 for why this asymmetry is
// intentional and standard (A.10 m5, P4.3 §3 / Q4).
func f32ToS16LE(src []float32, dst []byte) {
	for i, v := range src {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		// Round to nearest to avoid a consistent downward bias on conversion.
		var n int32
		if v >= 0 {
			n = int32(v*32767 + 0.5)
		} else {
			n = int32(v*32767 - 0.5)
		}
		u := uint16(int16(n))
		dst[i*2] = byte(u)
		dst[i*2+1] = byte(u >> 8)
	}
}

// s16LEToF32 is the inverse of f32ToS16LE: it reads len(dst) signed little-endian
// int16 samples from src (len(src) must be 2*len(dst)) and widens each into a
// float32 in [-1,1], writing into dst.
//
// De-quantization scale is 1/32768 (not the encoder's 32767). This asymmetry is
// intentional and standard: it uses the full negative rail so int16(-32768) maps
// exactly to -1.0, while int16(+32767) maps to +0.99997 — there is no +32768
// code to map to +1.0, and the encoder never emits one. Round-trip error is
// bounded to one LSB (1/32768), which the round-trip test asserts.
func s16LEToF32(src []byte, dst []float32) {
	for i := range dst {
		u := uint16(src[i*2]) | uint16(src[i*2+1])<<8
		dst[i] = float32(int16(u)) / 32768
	}
}
