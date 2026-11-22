package worker

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestRPCFramePreservesBinaryBodyAndRejectsAmbiguousFrames(t *testing.T) {
	payload := append([]byte{'\n', 0, 255, '{', '}'}, bytes.Repeat([]byte("binary"), 400000)...)
	header, err := json.Marshal(RPCRequest{Method: "stream", Stream: &StreamRequest{Action: "backup_download"}})
	if err != nil {
		t.Fatal(err)
	}
	request, body, err := decodeRPCFrame(io.MultiReader(bytes.NewReader(append(header, '\n')), bytes.NewReader(payload)))
	if err != nil || request.Method != "stream" {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("binary body changed at frame boundary", err)
	}
	for _, invalid := range []string{`{"method":"probe"}{"method":"drain"}`, `{"method":"probe","unexpected":true}`, `{"method":"stream"}body`, `{"method":"probe","id":"` + strings.Repeat("x", 2<<20) + `"}`} {
		if _, _, err := decodeRPCFrame(strings.NewReader(invalid)); err == nil {
			t.Fatal("ambiguous or oversized request accepted")
		}
	}
}

func TestStreamResponseFramingAndLimits(t *testing.T) {
	header, err := json.Marshal(StreamResponse{Size: 4, SHA256: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	meta, body, err := ReadStreamResponse(bytes.NewReader(append(append(header, '\n'), []byte{0, '\n', 255, 1}...)))
	if err != nil || meta.Size != 4 {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(actual, []byte{0, '\n', 255, 1}) {
		t.Fatal("response framing changed binary data")
	}
	for _, response := range []string{`{"size":0,"sha256":"` + strings.Repeat("a", 64) + `"}` + "\n", `{"size":68719476737,"sha256":"` + strings.Repeat("a", 64) + `"}` + "\n", `{"size":4,"sha256":"bad"}` + "\n", `{"error":"denied"}` + "\n", strings.Repeat("x", 8192) + "\n"} {
		if _, _, err := ReadStreamResponse(strings.NewReader(response)); err == nil {
			t.Fatal("unsafe response metadata accepted")
		}
	}
}
