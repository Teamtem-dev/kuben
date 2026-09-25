package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameError is why a frame could not be read or written.
//
//sumtype:decl
type FrameError interface {
	error
	frameError()
}

// LinkError is a failure of the stream itself.
type LinkError struct{ Err error }

// TooLargeError is a frame longer than the limit.
type TooLargeError struct{ Len, Max uint64 }

// MalformedError is a frame that is not a valid message.
type MalformedError struct{ Err error }

func (LinkError) frameError()      {}
func (TooLargeError) frameError()  {}
func (MalformedError) frameError() {}

func (e LinkError) Error() string { return fmt.Sprintf("the link failed: %v", e.Err) }

func (e TooLargeError) Error() string {
	return fmt.Sprintf("a frame of %d bytes exceeds the %d-byte limit", e.Len, e.Max)
}

func (e MalformedError) Error() string {
	return fmt.Sprintf("a frame is not a valid message: %v", e.Err)
}

// Unwrap is the stream's error.
func (e LinkError) Unwrap() error { return e.Err }

// Unwrap is the decoding error.
func (e MalformedError) Unwrap() error { return e.Err }

// WriteFrame sends m as one frame.
func WriteFrame(w io.Writer, m Message) error {
	body := Encode(m)
	if len(body) > MaxFrame {
		return TooLargeError{Len: uint64(len(body)), Max: MaxFrame}
	}
	frame := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body))) //nolint:gosec // bounded by MaxFrame above
	frame = append(frame, body...)
	if _, err := w.Write(frame); err != nil {
		return LinkError{Err: err}
	}
	return nil
}

// ReadFrame is the next message; ok is false when the peer closed the
// stream between frames. A stream that ends inside a frame is a LinkError.
func ReadFrame(r io.Reader) (m Message, ok bool, err error) {
	var header [4]byte
	if n, err := io.ReadFull(r, header[:1]); n == 0 {
		if errors.Is(err, io.EOF) {
			return nil, false, nil
		}
		return nil, false, LinkError{Err: err}
	}
	if _, err := io.ReadFull(r, header[1:]); err != nil {
		return nil, false, LinkError{Err: unexpected(err)}
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > MaxFrame {
		return nil, false, TooLargeError{Len: uint64(n), Max: MaxFrame}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, false, LinkError{Err: unexpected(err)}
	}
	m, err = Decode(body)
	if err != nil {
		return nil, false, MalformedError{Err: err}
	}
	return m, true, nil
}

// unexpected turns a clean end inside a frame into io.ErrUnexpectedEOF, as
// read_exact did.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
