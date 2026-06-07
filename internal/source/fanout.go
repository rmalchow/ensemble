package source

import (
	"encoding/binary"
	"io"

	"ensemble/internal/stream"
)

// TCP length framing (D13: uint32 big-endian length prefix before each
// header+payload chunk). The client side lives in internal/stream; the source
// re-implements the writer/reader here to stay within its owned files.

func writeTCPFrame(w io.Writer, chunk []byte) error {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(chunk)))
	if _, err := w.Write(lp[:]); err != nil {
		return err
	}
	_, err := w.Write(chunk)
	return err
}

const maxTCPFrameLen = 64 * 1024

func readTCPFrame(r io.Reader) ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lp[:])
	if n < stream.HeaderSize || n > maxTCPFrameLen {
		return nil, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
