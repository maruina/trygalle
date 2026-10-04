package rpc

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestScannerSplitsOnLFOnly(t *testing.T) {
	sc := NewScanner(strings.NewReader("one\ntwo\nthree\n"))
	for _, want := range []string{"one", "two", "three"} {
		rec, err := sc.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if got := string(rec); got != want {
			t.Errorf("record = %q, want %q", got, want)
		}
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next() at end = %v, want io.EOF", err)
	}
}

func TestScannerStripCR(t *testing.T) {
	sc := NewScanner(strings.NewReader("one\r\ntwo\r\n"))
	for _, want := range []string{"one", "two"} {
		rec, err := sc.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if got := string(rec); got != want {
			t.Errorf("record = %q, want %q", got, want)
		}
	}
}

func TestScannerBatchedChunks(t *testing.T) {
	// Feed the stream in slices smaller than a record to exercise accumulation.
	raw := "one\nvery long record here\nthree\n"
	chunked := chunkReader{data: []byte(raw), chunk: 4}
	sc := NewScanner(&chunked)
	for _, want := range []string{"one", "very long record here", "three"} {
		rec, err := sc.Next()
		if err != nil {
			t.Fatalf("Next() error: %v", err)
		}
		if got := string(rec); got != want {
			t.Errorf("record = %q, want %q", got, want)
		}
	}
}

func TestScannerUnicodeSeparatorsDoNotSplit(t *testing.T) {
	// U+2028 and U+2029 are valid inside JSON strings and must not become
	// record boundaries.
	rec := "{\"text\":\"line1\xe2\x80\xa8line2\xe2\x80\xa9line3\"}\n"
	sc := NewScanner(strings.NewReader(rec))
	got, err := sc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if string(got) != rec[:len(rec)-1] {
		t.Errorf("record changed across unicode separators: %q", got)
	}
	var v struct {
		Text string `json:"text"`
	}
	if err := DecodeRecord(got, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(v.Text, "\u2028") || !strings.Contains(v.Text, "\u2029") {
		t.Errorf("separators lost in decode: %q", v.Text)
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("trailing Next() = %v, want io.EOF", err)
	}
}

func TestScannerOversizeRecord(t *testing.T) {
	// 1 MiB single-line record must decode without a 64 KiB-style cap.
	big := strings.Repeat("a", 1<<20)
	rec := "{\"text\":\"" + big + "\"}\n"
	sc := NewScanner(strings.NewReader(rec))
	got, err := sc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	var v struct {
		Text string `json:"text"`
	}
	if err := DecodeRecord(got, &v); err != nil {
		t.Fatalf("decode 1 MiB record: %v", err)
	}
	if len(v.Text) != len(big) {
		t.Errorf("text length = %d, want %d", len(v.Text), len(big))
	}
}

func TestScannerUnterminatedTrailingRecord(t *testing.T) {
	// A final record without LF is returned once, then EOF.
	sc := NewScanner(strings.NewReader("one\npartial"))
	rec, err := sc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if string(rec) != "one" {
		t.Errorf("record = %q, want %q", rec, "one")
	}
	rec, err = sc.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}
	if string(rec) != "partial" {
		t.Errorf("trailing record = %q, want %q", rec, "partial")
	}
	if _, err := sc.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("final Next() = %v, want io.EOF", err)
	}
}

func TestDecodeRecordGarbageIsProtocolError(t *testing.T) {
	var v struct{}
	err := DecodeRecord([]byte("not json"), &v)
	var perr *ProtocolError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want *ProtocolError", err)
	}
	if perr.Err == nil {
		t.Error("ProtocolError.Err is nil")
	}
	if string(perr.Record) != "not json" {
		t.Errorf("ProtocolError.Record = %q, want %q", perr.Record, "not json")
	}
}

func TestWriteRecordFullWrite(t *testing.T) {
	var buf = shortWriter{max: 3}
	v := map[string]string{"type": "get_state"}
	if err := WriteRecord(&buf, v); err != nil {
		t.Fatalf("WriteRecord error: %v", err)
	}
	want := "{\"type\":\"get_state\"}\n"
	if buf.String() != want {
		t.Errorf("written = %q, want %q", buf.String(), want)
	}
}

// chunkReader returns data in fixed-size chunks to exercise scanner buffering.
type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := min(c.chunk, len(c.data)-c.pos)
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

// shortWriter simulates short writes to exercise the full-write loop.
type shortWriter struct {
	buf []byte
	max int
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := min(s.max, len(p))
	s.buf = append(s.buf, p[:n]...)
	return n, nil
}

func (s *shortWriter) String() string { return string(s.buf) }
