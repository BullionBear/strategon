package filebrowse

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestFetchSingleFileSHA256(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello workdir")
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var chunks []*pb.FileChunk
	send := func(_ context.Context, msg *pb.AgentMessage) error {
		chunks = append(chunks, msg.GetFileChunk())
		return nil
	}
	if err := Fetch(context.Background(), root, "s", "req1", []string{"note.txt"}, send); err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	var buf bytes.Buffer
	var sum string
	for _, c := range chunks {
		if c.GetError() != "" {
			t.Fatal(c.GetError())
		}
		buf.Write(c.GetData())
		if c.GetEof() {
			sum = c.GetSha256()
		}
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("got %q want %q", buf.Bytes(), content)
	}
	want := sha256.Sum256(content)
	if sum != hex.EncodeToString(want[:]) {
		t.Fatalf("sha256=%s want %s", sum, hex.EncodeToString(want[:]))
	}
	if chunks[0].GetTransferKind() != pb.TransferKind_TRANSFER_KIND_RAW_FILE {
		t.Fatalf("kind=%v", chunks[0].GetTransferKind())
	}
	if chunks[0].GetFilename() != "note.txt" {
		t.Fatalf("filename=%q", chunks[0].GetFilename())
	}
}

func TestFetchTarballRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var buf bytes.Buffer
	send := func(_ context.Context, msg *pb.AgentMessage) error {
		c := msg.GetFileChunk()
		if c.GetError() != "" {
			t.Fatal(c.GetError())
		}
		buf.Write(c.GetData())
		return nil
	}
	if err := Fetch(context.Background(), root, "s", "req2", []string{"a.txt", "b.txt"}, send); err != nil {
		t.Fatal(err)
	}

	gr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name] = string(b)
	}
	if got["a.txt"] != "aaa" || got["b.txt"] != "bbb" {
		t.Fatalf("archive contents=%v", got)
	}
}

func TestFetchEmptyPathsError(t *testing.T) {
	dir := t.TempDir()
	root, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var errMsg string
	send := func(_ context.Context, msg *pb.AgentMessage) error {
		if c := msg.GetFileChunk(); c != nil && c.GetError() != "" {
			errMsg = c.GetError()
		}
		return nil
	}
	_ = Fetch(context.Background(), root, "s", "req3", nil, send)
	if errMsg == "" {
		t.Fatal("expected error for empty paths")
	}
}

func TestFetchEmptyDirError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var errMsg string
	send := func(_ context.Context, msg *pb.AgentMessage) error {
		if c := msg.GetFileChunk(); c != nil && c.GetError() != "" {
			errMsg = c.GetError()
		}
		return nil
	}
	_ = Fetch(context.Background(), root, "s", "req4", []string{"d"}, send)
	if !strings.Contains(errMsg, "no regular files") {
		t.Fatalf("expected empty-archive error, got %q", errMsg)
	}
}

func TestFetchTarballFileCountCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxTarballFiles+1; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%04d", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := Root(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var errMsg string
	send := func(_ context.Context, msg *pb.AgentMessage) error {
		if c := msg.GetFileChunk(); c != nil && c.GetError() != "" {
			errMsg = c.GetError()
		}
		return nil
	}
	_ = Fetch(context.Background(), root, "s", "req5", []string{"."}, send)
	if !strings.Contains(errMsg, "file limit") {
		t.Fatalf("expected file-limit error, got %q", errMsg)
	}
}
