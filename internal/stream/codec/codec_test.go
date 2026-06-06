package codec

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
)

func TestPCMID(t *testing.T) {
	if got := NewPCM(2).ID(); got != PCM {
		t.Fatalf("ID() = %d, want %d (PCM)", got, PCM)
	}
	if PCM != 0 {
		t.Fatalf("PCM = %d, want 0", PCM)
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	nameTests := []struct {
		name string
		id   CodecID
	}{
		{"pcm", PCM},
		{"opus", OPUS},
	}
	for _, tt := range nameTests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := FromName(tt.name)
			if !ok || id != tt.id {
				t.Fatalf("FromName(%q) = %d,%v; want %d,true", tt.name, id, ok, tt.id)
			}
			n, ok := NameOf(tt.id)
			if !ok || n != tt.name {
				t.Fatalf("NameOf(%d) = %q,%v; want %q,true", tt.id, n, ok, tt.name)
			}
		})
	}
	if _, ok := FromName("flac"); ok {
		t.Fatal("FromName(unknown) returned ok=true")
	}
	if _, ok := NameOf(99); ok {
		t.Fatal("NameOf(unknown id) returned ok=true")
	}
}

func TestNewGating(t *testing.T) {
	if _, err := New(PCM); err != nil {
		t.Fatalf("New(PCM) error: %v", err)
	}
	// Under the default (!opus) build opusFactory is nil so New(OPUS) always
	// returns ErrUnsupportedCodec.  Under -tags opus with libopus present the
	// factory is wired and New(OPUS) succeeds — that case is exercised by
	// TestOpusAvailableAndNew in opus_test.go (//go:build opus).
	if _, err := New(OPUS); err != nil && !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("New(OPUS) err = %v, want nil or ErrUnsupportedCodec", err)
	}
	if _, err := New(99); !errors.Is(err, ErrUnsupportedCodec) {
		t.Fatalf("New(99) err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestEncodeSize(t *testing.T) {
	c := NewPCM(2)
	pcm := make([]float32, 480*2) // 480 stereo frames, A.12 canonical
	out, err := c.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	if len(out) != 1920 {
		t.Fatalf("Encode len = %d, want 1920 (doc 05 §5.10)", len(out))
	}
}

func TestEncodeAlignmentError(t *testing.T) {
	c := NewPCM(2)
	tests := []struct {
		name string
		in   []float32
	}{
		{"odd length", make([]float32, 5)},
		{"non-frame-multiple", make([]float32, 481)}, // 481 % 2 != 0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Encode(tt.in); !errors.Is(err, ErrChunkAlloc) {
				t.Fatalf("Encode(%s) err = %v, want ErrChunkAlloc", tt.name, err)
			}
		})
	}
}

func TestDecodeSize(t *testing.T) {
	c := NewPCM(2)
	payload := make([]byte, 1920)
	out, err := c.Decode(payload)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if len(out) != 960 {
		t.Fatalf("Decode len = %d, want 960 (480 stereo frames)", len(out))
	}
}

func TestDecodeAlignment(t *testing.T) {
	c := NewPCM(2)
	// channels=2 → a frame is 2*2 = 4 bytes; len must be a multiple of 4.
	tests := []struct {
		name    string
		in      []byte
		wantErr bool
		wantLen int
	}{
		{"odd byte count", make([]byte, 3), true, 0},
		{"not frame multiple", make([]byte, 6), true, 0}, // 6 % 4 != 0
		{"empty", nil, false, 0},
		{"one frame", make([]byte, 4), false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := c.Decode(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrShortPayload) {
					t.Fatalf("Decode(%s) err = %v, want ErrShortPayload", tt.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decode(%s) unexpected err: %v", tt.name, err)
			}
			if len(out) != tt.wantLen {
				t.Fatalf("Decode(%s) len = %d, want %d", tt.name, len(out), tt.wantLen)
			}
		})
	}
}

func TestRoundTripCodec(t *testing.T) {
	c := NewPCM(2)
	const tol = 2.0 / 32768.0 // ~2 LSB; see TestRoundTripFidelity / P4.3 §3 Q4
	rng := rand.New(rand.NewSource(1))
	pcm := make([]float32, 480*2)
	for i := range pcm {
		pcm[i] = rng.Float32()*2 - 1
	}
	payload, err := c.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := c.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != len(pcm) {
		t.Fatalf("len mismatch: %d vs %d", len(out), len(pcm))
	}
	for i := range pcm {
		if d := math.Abs(float64(out[i] - pcm[i])); d > tol+1e-7 {
			t.Fatalf("sample %d: %v -> %v, err %v > 2 LSB", i, pcm[i], out[i], d)
		}
	}
}

func TestStatelessConcurrent(t *testing.T) {
	shared := NewPCM(2)
	rng := rand.New(rand.NewSource(2))
	a := make([]float32, 480*2)
	b := make([]float32, 480*2)
	for i := range a {
		a[i] = rng.Float32()*2 - 1
		b[i] = rng.Float32()*2 - 1
	}
	// Interleaved calls on the shared codec match separate codec instances.
	sa, _ := shared.Encode(a)
	sb, _ := shared.Encode(b)
	ea, _ := NewPCM(2).Encode(a)
	eb, _ := NewPCM(2).Encode(b)
	if string(sa) != string(ea) || string(sb) != string(eb) {
		t.Fatal("shared codec output differs from separate instances (state leak)")
	}
	// Race coverage: concurrent Encode+Decode on the one instance.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = shared.Encode(a) }()
		go func() { defer wg.Done(); _, _ = shared.Decode(sa) }()
	}
	wg.Wait()
}

func TestCodecAllocs(t *testing.T) {
	c := NewPCM(2)
	pcm := make([]float32, 480*2)
	payload, _ := c.Encode(pcm)
	if n := testing.AllocsPerRun(100, func() { _, _ = c.Encode(pcm) }); n > 1 {
		t.Fatalf("Encode: %v allocs/op, want <=1", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = c.Decode(payload) }); n > 1 {
		t.Fatalf("Decode: %v allocs/op, want <=1", n)
	}
}
