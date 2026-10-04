package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ProtocolError reports a record or stream that violates the RPC framing and
// JSON invariants. The failing record is retained for diagnostics only; it is
// never logged (the component logs metadata, not payloads).
type ProtocolError struct {
	Record []byte
	Err    error
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("protocol error: %v", e.Err)
}

func (e *ProtocolError) Unwrap() error { return e.Err }

// Scanner splits a byte stream into LF-terminated records without a length
// limit. It splits only on LF (0x0a) and strips one optional preceding CR
// (0x0d), so Unicode line and paragraph separators (U+2028, U+2029) inside a
// JSON string never become record boundaries.
type Scanner struct {
	r       io.Reader
	pending []byte
}

// NewScanner returns a Scanner reading from r.
func NewScanner(r io.Reader) *Scanner { return &Scanner{r: r} }

// Next returns the next record with its LF and one optional preceding CR
// removed. It returns io.EOF when the stream ends at a record boundary. A
// trailing record without a final LF is returned once before io.EOF; the caller
// decides whether an unterminated record is a protocol violation.
func (s *Scanner) Next() ([]byte, error) {
	for {
		if i := bytes.IndexByte(s.pending, '\n'); i >= 0 {
			rec := s.pending[:i]
			s.pending = s.pending[i+1:]
			return stripCR(rec), nil
		}
		chunk := make([]byte, 32*1024)
		n, err := s.r.Read(chunk)
		if n > 0 {
			s.pending = append(s.pending, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(s.pending) == 0 {
					return nil, io.EOF
				}
				rec := s.pending
				s.pending = nil
				return stripCR(rec), nil
			}
			return nil, err
		}
	}
}

// stripCR removes one optional preceding CR from the end of a record. A raw CR
// cannot legally terminate a JSON string, so a trailing CR is always the CRLF
// artifact the protocol permits.
func stripCR(rec []byte) []byte {
	if n := len(rec); n > 0 && rec[n-1] == '\r' {
		return rec[:n-1]
	}
	return rec
}

// DecodeRecord decodes one record into v, reporting malformed JSON as a typed
// ProtocolError.
func DecodeRecord(rec []byte, v any) error {
	if err := json.Unmarshal(rec, v); err != nil {
		return &ProtocolError{
			Record: append([]byte(nil), rec...),
			Err:    fmt.Errorf("decode record: %w", err),
		}
	}
	return nil
}

// WriteRecord marshals v and writes it as one LF-terminated record with a
// full-write loop that retries short writes.
func WriteRecord(w io.Writer, v any) error {
	rec, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	rec = append(rec, '\n')
	for len(rec) > 0 {
		n, err := w.Write(rec)
		if err != nil {
			return fmt.Errorf("write record: %w", err)
		}
		rec = rec[n:]
	}
	return nil
}
