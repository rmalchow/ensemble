package group_test

// External test package so it may import internal/audio/render (which imports
// internal/group) without a cycle. It ties the doc 06 §5.1 channel-role fan-out to
// the multi-group context: group A's two listeners carry left/right roles and,
// reading the SAME group-A timeline sample, produce a coherent L/R image.

import (
	"testing"

	"gitlab.rand0m.me/ruben/go/ensemble/internal/audio/render"
	"gitlab.rand0m.me/ruben/go/ensemble/internal/group"
)

func TestMultiGroup_ChannelRoleAcrossPair(t *testing.T) {
	const rate = 48000
	// One shared group-A timeline (the master), read by both pair members so they
	// are sample-aligned by construction.
	tl := group.NewMasterTimeline(rate)
	tl.Play(0)
	s0, _, ok0 := tl.NowSample()
	s1, _, ok1 := tl.NowSample()
	if !ok0 || !ok1 {
		t.Fatal("master timeline not ok")
	}
	if s1 < s0 {
		t.Fatalf("timeline went backwards: %d -> %d", s0, s1)
	}

	// The same source frame fed to a left node and a right node of group A.
	const L, R = float32(0.6), float32(-0.4)
	src := []float32{L, R}
	const outCh = 2

	leftRole, err := render.ParseChannel("left")
	if err != nil {
		t.Fatalf("ParseChannel(left): %v", err)
	}
	rightRole, err := render.ParseChannel("right")
	if err != nil {
		t.Fatalf("ParseChannel(right): %v", err)
	}

	for oc := 0; oc < outCh; oc++ {
		if v := render.SelectChannel(src, leftRole, outCh, oc); v != L {
			t.Fatalf("group-A left node oc%d=%v, want L=%v", oc, v, L)
		}
		if v := render.SelectChannel(src, rightRole, outCh, oc); v != R {
			t.Fatalf("group-A right node oc%d=%v, want R=%v", oc, v, R)
		}
	}
}
