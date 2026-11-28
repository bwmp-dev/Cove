package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveVolumeArchiveRefusesTraversalAndCrossVolumeLinks(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers []*tar.Header
		valid   bool
	}{
		{"normal", []*tar.Header{{Name: "./", Typeflag: tar.TypeDir}, {Name: "./000/", Typeflag: tar.TypeDir}, {Name: "./000/data", Typeflag: tar.TypeReg}, {Name: "./000/link", Typeflag: tar.TypeLink, Linkname: "./000/data"}, {Name: "./000/relative", Typeflag: tar.TypeSymlink, Linkname: "data"}}, true},
		{"parent", []*tar.Header{{Name: "000/../../host", Typeflag: tar.TypeReg}}, false},
		{"symlink-parent", []*tar.Header{{Name: "000/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, {Name: "000/link/passwd", Typeflag: tar.TypeReg}}, false},
		{"foreign-volume", []*tar.Header{{Name: "001/data", Typeflag: tar.TypeReg}}, false},
		{"cross-volume-hardlink", []*tar.Header{{Name: "000/data", Typeflag: tar.TypeReg}, {Name: "000/link", Typeflag: tar.TypeLink, Linkname: "001/data"}}, false},
		{"device", []*tar.Header{{Name: "000/device", Typeflag: tar.TypeChar}}, false},
		{"repeat", []*tar.Header{{Name: "000/data", Typeflag: tar.TypeReg}, {Name: "000/data", Typeflag: tar.TypeReg}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var data bytes.Buffer
			tw := tar.NewWriter(&data)
			for _, h := range test.headers {
				h.Mode = 0600
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(t.TempDir(), "volume.tar")
			if err := os.WriteFile(filename, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			err := validateMoveVolumes(filename, 1)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestMoveRecoveryRefusesChangedDockerDaemon(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.Run = func(_ context.Context, _ string, args, environment []string, out io.Writer) error {
		_, err := io.WriteString(out, "new-daemon\n")
		return err
	}
	if _, err = e.Move(context.Background(), MoveRequest{ID: "move-id", Workload: "resource", Action: "status", Daemon: "original-daemon"}); err == nil {
		t.Fatal("replacement node was accepted as proof that the old destination stopped")
	}
	state, err := e.Move(context.Background(), MoveRequest{ID: "move-id", Workload: "resource", Action: "status", Daemon: "new-daemon"})
	if err != nil || state.Phase != "absent" {
		t.Fatal("matching endpoint failed readonly recovery", err)
	}
}

func TestMoveUploadReceiptUsesVerifiedByteCountInStreamProtocol(t *testing.T) {
	e, err := NewEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("abc"))
	sum := hex.EncodeToString(digest[:])
	j := moveJournal{MoveState: MoveState{ID: "move-id", Workload: "resource", Role: "target", Phase: "imported", Bytes: 3, SHA256: sum}}
	if err = e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/rpc", nil)
	e.serveMoveStream(response, request, &StreamRequest{Action: "move_upload", Move: &MoveRequest{ID: "move-id", Workload: "resource"}, Size: 3, SHA256: sum}, bytes.NewReader([]byte("abc")))
	meta, _, err := ReadStreamResponse(response.Body)
	if err != nil {
		t.Fatal("successful move upload emitted an invalid receipt", err)
	}
	if meta.BackupID != "move-id" || meta.Size != 3 || meta.SHA256 != sum {
		t.Fatal("move receipt did not bind the verified bytes")
	}
}

func TestMoveFenceSurvivesWorkerRestartAndBlocksNewWork(t *testing.T) {
	root := t.TempDir()
	e, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	j := moveJournal{MoveState: MoveState{ID: "move-id", Workload: "resource", Role: "source", Phase: "frozen"}}
	if err = e.saveMove(&j); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewEngine(root)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.moving("resource") {
		t.Fatal("restart forgot the persistent source fence")
	}
	if _, err = restarted.Submit(Request{ID: "deploy", Workload: "resource", Action: "stop"}); err == nil {
		t.Fatal("ordinary operation bypassed move fence")
	}
	state, err := restarted.Move(context.Background(), MoveRequest{ID: "move-id", Workload: "resource", Action: "status"})
	if err != nil || state.Phase != "frozen" {
		t.Fatal("move cannot resume after restart", err)
	}
	if err = os.WriteFile(e.movePath("resource"), []byte("invalid journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if !restarted.moving("resource") {
		t.Fatal("corrupt move receipt allowed resource writes")
	}
}

func TestMoveRefusesRolloutAndManagedDatabaseBeforeDocker(t *testing.T) {
	for _, state := range []deployedState{{Generation: "staged"}, {Rollout: &RolloutOptions{}}, {DatabaseMetadata: []byte(`{"engine":"postgresql"}`)}} {
		e, err := NewEngine(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(e.Root, "workloads", "resource")
		if err = os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err = writeJSON(filepath.Join(directory, "current.json"), state); err != nil {
			t.Fatal(err)
		}
		e.Run = func(context.Context, string, []string, []string, io.Writer) error {
			t.Fatal("unsupported move reached Docker")
			return nil
		}
		if _, _, _, err = e.inspectMove(t.Context(), "resource"); err == nil {
			t.Fatal("unsupported release was moved")
		}
	}
}
