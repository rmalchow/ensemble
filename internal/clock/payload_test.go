package clock

import (
	"encoding/binary"
	"errors"
	"testing"

	"ensemble/internal/stream"
)

func TestEncodeDecodeRequestRoundTrip(t *testing.T) {
	var buf [packetSize]byte
	n := encodeClock(buf[:], stream.TypeClockReq, 7, 42, 12345, 0, 0)
	if n != packetSize {
		t.Fatalf("encodeClock returned %d, want %d", n, packetSize)
	}
	h, t1, t2, t3, err := decodeClock(buf[:])
	if err != nil {
		t.Fatalf("decodeClock: %v", err)
	}
	if h.Type != stream.TypeClockReq {
		t.Errorf("Type = %#x, want %#x", h.Type, stream.TypeClockReq)
	}
	if h.Gen != 7 || h.Seq != 42 {
		t.Errorf("Gen/Seq = %d/%d, want 7/42", h.Gen, h.Seq)
	}
	if t1 != 12345 || t2 != 0 || t3 != 0 {
		t.Errorf("t1/t2/t3 = %d/%d/%d, want 12345/0/0", t1, t2, t3)
	}
}

func TestEncodeDecodeReplyRoundTrip(t *testing.T) {
	var buf [packetSize]byte
	encodeClock(buf[:], stream.TypeClockRsp, 99, 1000, -5, 1_000_000_000, 1_000_000_500)
	h, t1, t2, t3, err := decodeClock(buf[:])
	if err != nil {
		t.Fatalf("decodeClock: %v", err)
	}
	if h.Type != stream.TypeClockRsp {
		t.Errorf("Type = %#x, want %#x", h.Type, stream.TypeClockRsp)
	}
	if t1 != -5 || t2 != 1_000_000_000 || t3 != 1_000_000_500 {
		t.Errorf("t1/t2/t3 = %d/%d/%d", t1, t2, t3)
	}
}

func TestDecodeClockShortBuffer(t *testing.T) {
	var buf [packetSize]byte
	encodeClock(buf[:], stream.TypeClockReq, 1, 1, 1, 0, 0)
	_, _, _, _, err := decodeClock(buf[:packetSize-1])
	if !errors.Is(err, stream.ErrShort) {
		t.Fatalf("err = %v, want ErrShort", err)
	}
}

func TestDecodeClockBadMagic(t *testing.T) {
	var buf [packetSize]byte
	encodeClock(buf[:], stream.TypeClockReq, 1, 1, 1, 0, 0)
	buf[0] ^= 0xFF // corrupt magic
	_, _, _, _, err := decodeClock(buf[:])
	if !errors.Is(err, stream.ErrBadMagic) {
		t.Fatalf("err = %v, want ErrBadMagic", err)
	}
}

func TestDecodeClockBigEndianOffsets(t *testing.T) {
	var buf [packetSize]byte
	encodeClock(buf[:], stream.TypeClockRsp, 0, 0, 0x0102030405060708, 0x1112131415161718, 0x2122232425262728)
	if got := binary.BigEndian.Uint64(buf[stream.HeaderSize+0:]); got != 0x0102030405060708 {
		t.Errorf("t1 bytes = %#x", got)
	}
	if got := binary.BigEndian.Uint64(buf[stream.HeaderSize+8:]); got != 0x1112131415161718 {
		t.Errorf("t2 bytes = %#x", got)
	}
	if got := binary.BigEndian.Uint64(buf[stream.HeaderSize+16:]); got != 0x2122232425262728 {
		t.Errorf("t3 bytes = %#x", got)
	}
}
