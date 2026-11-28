package worker

import (
	"bytes"
	"errors"
	"io"
	"os"
)

const rdbTransportMarkerBytes = 40
const rdbFooterBytes = 9

func hasRDBTransportMarker(header, tail []byte) bool {
	if len(header) != 9 || !bytes.Equal(header[:5], []byte("REDIS")) || len(tail) != rdbFooterBytes+rdbTransportMarkerBytes || tail[0] != 0xff {
		return false
	}
	for _, value := range header[5:] {
		if value < '0' || value > '9' {
			return false
		}
	}
	for _, value := range tail[rdbFooterBytes:] {
		if !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F') {
			return false
		}
	}
	return true
}

func databaseDumpOutput(database *DatabaseBackup, out io.Writer, limit int64) (io.Writer, func() error) {
	bounded := &sizeBoundWriter{Writer: out, remaining: limit}
	if database == nil || database.Engine != "redis" && database.Engine != "valkey" {
		return bounded, func() error { return nil }
	}
	w := &rdbDumpWriter{out: bounded, remaining: limit + rdbTransportMarkerBytes}
	return w, w.finish
}

type rdbDumpWriter struct {
	out       io.Writer
	remaining int64
	header    []byte
	tail      []byte
	closed    bool
	err       error
}

func (w *rdbDumpWriter) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	n, err := w.out.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return err
}

func (w *rdbDumpWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = errors.New("database dump exceeds the configured byte limit")
		return 0, w.err
	}
	count := len(p)
	w.header = append(w.header, p[:min(len(p), 9-len(w.header))]...)
	flush := max(0, len(w.tail)+len(p)-rdbFooterBytes-rdbTransportMarkerBytes)
	old := min(flush, len(w.tail))
	if err := w.write(w.tail[:old]); err != nil {
		return 0, err
	}
	w.tail = w.tail[old:]
	flush -= old
	if err := w.write(p[:flush]); err != nil {
		return 0, err
	}
	w.tail = append(w.tail, p[flush:]...)
	w.remaining -= int64(count)
	return count, nil
}

func (w *rdbDumpWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return io.ErrClosedPipe
	}
	w.closed = true
	// CLI stdout retains the diskless transfer marker after the RDB EOF and checksum.
	// A plain RDB cannot end in forty hex bytes: its EOF opcode must be nine bytes from the end.
	if hasRDBTransportMarker(w.header, w.tail) {
		w.tail = w.tail[:rdbFooterBytes]
	}
	return w.write(w.tail)
}

func normalizeStagedRDB(path string) (result error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDatabaseDumpBytes+rdbTransportMarkerBytes {
		return errors.New("database dump staging file is missing, unsafe or oversized")
	}
	if info.Size() < 9+rdbFooterBytes+rdbTransportMarkerBytes {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) || current.Size() != info.Size() {
		return errors.New("database staging file changed before normalization")
	}
	header, tail := make([]byte, 9), make([]byte, rdbFooterBytes+rdbTransportMarkerBytes)
	if _, err := file.ReadAt(header, 0); err != nil {
		return err
	}
	if _, err := file.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return err
	}
	if !hasRDBTransportMarker(header, tail) {
		return nil
	}
	if err := file.Truncate(info.Size() - rdbTransportMarkerBytes); err != nil {
		return err
	}
	return file.Sync()
}
