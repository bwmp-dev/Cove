package worker

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type rdbStreamFixture struct {
	Image, Version, Engine string
	Stdout, File           []byte
}

//go:embed testdata/rdb-streams.json
var rdbStreamFixtureJSON []byte

func rdbStreamFixtures(t *testing.T) []rdbStreamFixture {
	t.Helper()
	var fixture struct{ Cases []rdbStreamFixture }
	if err := json.Unmarshal(rdbStreamFixtureJSON, &fixture); err != nil || len(fixture.Cases) != 5 {
		t.Fatal("missing stock-image RDB fixtures", err)
	}
	return fixture.Cases
}

func TestRDBStreamNormalizesStockDumpsAcrossChunks(t *testing.T) {
	for _, fixture := range rdbStreamFixtures(t) {
		t.Run(fixture.Image, func(t *testing.T) {
			want := fixture.Stdout[:len(fixture.Stdout)-rdbTransportMarkerBytes]
			for _, input := range [][]byte{fixture.Stdout, want, fixture.File} {
				expected := input
				if bytes.Equal(input, fixture.Stdout) {
					expected = want
				}
				for chunk := 1; chunk <= len(input)+1; chunk++ {
					var out bytes.Buffer
					writer, finish := databaseDumpOutput(&DatabaseBackup{Engine: fixture.Engine}, &out, int64(len(expected)))
					for offset := 0; offset < len(input); offset += chunk {
						if n, err := writer.Write(input[offset:min(offset+chunk, len(input))]); err != nil || n != min(chunk, len(input)-offset) {
							t.Fatal("dump write failed", chunk, n, err)
						}
					}
					if err := finish(); err != nil || !bytes.Equal(out.Bytes(), expected) {
						t.Fatal("RDB payload/checksum changed", chunk, err)
					}
				}
			}
		})
	}
}

func TestRDBStreamLeavesUnrecognizedTrailersAndOtherEnginesUnchanged(t *testing.T) {
	fixture := rdbStreamFixtures(t)[0]
	badMarker := bytes.Clone(fixture.Stdout)
	badMarker[len(badMarker)-1] = 'z'
	badFooter := bytes.Clone(fixture.Stdout)
	badFooter[len(badFooter)-49] = 0
	badHeader := bytes.Clone(fixture.Stdout)
	badHeader[0] = 'X'
	for _, input := range [][]byte{badMarker, badFooter, badHeader, fixture.Stdout[:len(fixture.Stdout)-1], []byte("short")} {
		var out bytes.Buffer
		writer, finish := databaseDumpOutput(&DatabaseBackup{Engine: "redis"}, &out, int64(len(input)))
		if _, err := writer.Write(input); err != nil {
			t.Fatal(err)
		}
		if err := finish(); err != nil || !bytes.Equal(input, out.Bytes()) {
			t.Fatal("unrecognized bytes were discarded", err)
		}
	}
	for _, database := range []*DatabaseBackup{nil, {Engine: "mongodb"}, {Engine: "postgresql"}} {
		var out bytes.Buffer
		writer, finish := databaseDumpOutput(database, &out, int64(len(fixture.Stdout)))
		if _, err := writer.Write(fixture.Stdout); err != nil {
			t.Fatal(err)
		}
		if err := finish(); err != nil || !bytes.Equal(fixture.Stdout, out.Bytes()) {
			t.Fatal("another engine was changed", err)
		}
	}
}

type rdbFailWriter struct{ err error }

func (w rdbFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRDBStreamBoundsAndOutputFailures(t *testing.T) {
	fixture := rdbStreamFixtures(t)[0]
	for _, input := range [][]byte{fixture.Stdout, fixture.File} {
		var out bytes.Buffer
		writer, finish := databaseDumpOutput(&DatabaseBackup{Engine: "redis"}, &out, 1)
		if _, err := writer.Write(input); err == nil || finish() == nil || out.Len() != 0 {
			t.Fatal("oversized dump bypassed its bound")
		}
	}
	var out bytes.Buffer
	writer, finish := databaseDumpOutput(&DatabaseBackup{Engine: "redis"}, &out, int64(len(fixture.File)-1))
	_, writeErr := writer.Write(fixture.File)
	if writeErr == nil && finish() == nil || out.Len() > len(fixture.File)-1 {
		t.Fatal("unframed payload bypassed its normalized bound")
	}
	for _, failure := range []error{errors.New("sink unavailable"), io.ErrClosedPipe, nil} {
		writer, finish := databaseDumpOutput(&DatabaseBackup{Engine: "redis"}, rdbFailWriter{failure}, 1<<20)
		_, err := writer.Write(fixture.Stdout)
		if err == nil || finish() == nil {
			t.Fatal("output error or short write was swallowed")
		}
	}
}

func TestNormalizeStagedRDBPreservesPayloadAndRejectsUnsafeFiles(t *testing.T) {
	for _, fixture := range rdbStreamFixtures(t) {
		for _, input := range [][]byte{fixture.Stdout, fixture.File} {
			path := filepath.Join(t.TempDir(), "database.dump")
			if err := os.WriteFile(path, input, 0600); err != nil {
				t.Fatal(err)
			}
			if err := normalizeStagedRDB(path); err != nil {
				t.Fatal(err)
			}
			want := input
			if bytes.Equal(input, fixture.Stdout) {
				want = input[:len(input)-rdbTransportMarkerBytes]
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("staged RDB payload/checksum changed", err)
			}
			if err := normalizeStagedRDB(path); err != nil {
				t.Fatal("normalization was not idempotent", err)
			}
		}
	}
	if normalizeStagedRDB(t.TempDir()) == nil || normalizeStagedRDB(filepath.Join(t.TempDir(), "missing")) == nil {
		t.Fatal("unsafe staging file accepted")
	}
}
