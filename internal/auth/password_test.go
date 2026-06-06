package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	phc, err := HashPassword(pw, DefaultArgon2id())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Errorf("PHC = %q, want $argon2id$v=19$ prefix", phc)
	}

	// Tamper the SECOND-TO-LAST hash char with a guaranteed-different value.
	// Not the last char: the PHC hash is unpadded base64, whose final char
	// carries unused trailing bits that Go's (non-Strict) decoder ignores — so
	// several distinct final chars decode to the SAME bytes and a last-char
	// tamper is flakily a no-op. Every other char is fully significant.
	ti := len(phc) - 2
	tb := byte('x')
	if phc[ti] == 'x' {
		tb = 'y'
	}
	tampered := phc[:ti] + string(tb) + phc[ti+1:]

	tests := []struct {
		name string
		pw   string
		phc  string
		want bool
	}{
		{"correct", pw, phc, true},
		{"wrong password", "wrong", phc, false},
		{"empty password", "", phc, false},
		{"tampered hash", pw, tampered, false},
		{"truncated phc", pw, phc[:len(phc)/2], false},
		{"empty phc", pw, "", false},
		{"garbage phc", pw, "not-a-phc-string", false},
		{"wrong scheme", pw, strings.Replace(phc, "argon2id", "argon2i", 1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyPassword(tc.pw, tc.phc); got != tc.want {
				t.Errorf("VerifyPassword(%q, ...) = %v, want %v", tc.pw, got, tc.want)
			}
		})
	}
}

func TestHashPasswordRandomSalt(t *testing.T) {
	const pw = "same-pw"
	a, err := HashPassword(pw, DefaultArgon2id())
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword(pw, DefaultArgon2id())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; salt is not random")
	}
	if !VerifyPassword(pw, a) || !VerifyPassword(pw, b) {
		t.Error("both independently-salted hashes must verify")
	}
}

// TestVerifyHonorsEmbeddedParams hashes under non-default params and confirms the
// verifier reads them from the PHC string (not from any default), so a hash made
// before a cluster-wide cost bump still verifies.
func TestVerifyHonorsEmbeddedParams(t *testing.T) {
	const pw = "params-pw"
	cheap := Argon2id{MemKiB: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}
	phc, err := HashPassword(pw, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(phc, "m=8192,t=1,p=1") {
		t.Fatalf("PHC %q does not carry the cheap params", phc)
	}
	if !VerifyPassword(pw, phc) {
		t.Error("verify must honor params embedded in the PHC, not a default")
	}
}

func TestDefaultArgon2idParams(t *testing.T) {
	p := DefaultArgon2id()
	if p.MemKiB != 65536 || p.Time != 3 || p.Threads != 4 || p.KeyLen != 32 || p.SaltLen != 16 {
		t.Errorf("defaults = %+v, want m=65536,t=3,p=4,keyLen=32,saltLen=16", p)
	}
}

// TestHashPasswordZeroParamsFallback confirms a zero-value Argon2id still
// produces a valid, default-cost hash (never a weak or unparseable one).
func TestHashPasswordZeroParamsFallback(t *testing.T) {
	const pw = "zero-params"
	phc, err := HashPassword(pw, Argon2id{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(phc, "m=65536,t=3,p=4") {
		t.Errorf("zero params did not fall back to defaults: %q", phc)
	}
	if !VerifyPassword(pw, phc) {
		t.Error("default-fallback hash must verify")
	}
}

func TestMalformedPHCNeverPanics(t *testing.T) {
	bad := []string{
		"", "$", "$$$$$", "$argon2id$v=19$m=0,t=0,p=0$AAAA$AAAA",
		"$argon2id$v=99$m=8192,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=x,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=8192,t=1,p=1$!!!!$AAAA",
		"$argon2id$v=19$m=8192,t=1,p=1$AAAA$!!!!",
		"$bcrypt$v=19$m=8192,t=1,p=1$AAAA$AAAA",
	}
	for _, b := range bad {
		if VerifyPassword("x", b) {
			t.Errorf("VerifyPassword unexpectedly true for malformed PHC %q", b)
		}
	}
}
