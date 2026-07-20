package filebrowse

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// SendFunc delivers one northbound AgentMessage. It must block (not drop) so
// chunks are not silently lost under backpressure.
type SendFunc func(ctx context.Context, msg *pb.AgentMessage) error

// Fetch streams one or more WorkDir paths as FileChunks.
// A single regular file is sent raw; multiple paths or a directory become a tar.gz.
func Fetch(ctx context.Context, root *os.Root, strategy, requestID string, paths []string, send SendFunc) error {
	if len(paths) == 0 {
		return sendError(ctx, send, requestID, "no paths requested")
	}
	normalized := make([]string, 0, len(paths))
	for _, p := range paths {
		n, err := NormalizeRel(p)
		if err != nil {
			return sendError(ctx, send, requestID, err.Error())
		}
		normalized = append(normalized, n)
	}

	if len(normalized) == 1 {
		info, err := root.Lstat(normalized[0])
		if err != nil {
			return sendError(ctx, send, requestID, fmt.Sprintf("stat: %v", err))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return sendError(ctx, send, requestID, "refusing to fetch symlink as raw file")
		}
		if info.Mode().IsRegular() {
			if info.Size() > MaxSingleFileBytes {
				return sendError(ctx, send, requestID, fmt.Sprintf("file exceeds %d byte limit", MaxSingleFileBytes))
			}
			return streamRawFile(ctx, root, requestID, normalized[0], info.Size(), send)
		}
	}

	files, total, err := collectFiles(root, normalized)
	if err != nil {
		return sendError(ctx, send, requestID, err.Error())
	}
	if len(files) == 0 {
		return sendError(ctx, send, requestID, "no regular files to archive")
	}
	if len(files) > MaxTarballFiles {
		return sendError(ctx, send, requestID, fmt.Sprintf("archive exceeds %d file limit", MaxTarballFiles))
	}
	if total > MaxTarballUncompressedBytes {
		return sendError(ctx, send, requestID, fmt.Sprintf("archive exceeds %d byte uncompressed limit", MaxTarballUncompressedBytes))
	}
	filename := fmt.Sprintf("%s-%s.tar.gz", sanitizeName(strategy), time.Now().UTC().Format("20060102T150405Z"))
	return streamTarball(ctx, root, requestID, filename, files, send)
}

func streamRawFile(ctx context.Context, root *os.Root, requestID, rel string, size int64, send SendFunc) error {
	f, err := root.Open(rel)
	if err != nil {
		return sendError(ctx, send, requestID, fmt.Sprintf("open: %v", err))
	}
	defer f.Close()

	w := newChunkWriter(ctx, send, requestID, path.Base(rel), pb.TransferKind_TRANSFER_KIND_RAW_FILE)
	if size == 0 {
		return w.Close()
	}
	buf := make([]byte, ChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return w.Close()
		}
		if readErr != nil {
			_ = sendError(ctx, send, requestID, fmt.Sprintf("read: %v", readErr))
			return readErr
		}
	}
}

type archiveFile struct {
	rel  string
	size int64
}

func collectFiles(root *os.Root, paths []string) ([]archiveFile, int64, error) {
	var out []archiveFile
	var total int64
	seen := map[string]struct{}{}

	var walk func(rel string) error
	walk = func(rel string) error {
		info, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Do not follow symlinks (avoids escaping via link targets).
			return nil
		}
		if info.IsDir() {
			dir, err := root.Open(rel)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			entries, err := dir.ReadDir(-1)
			_ = dir.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			for _, e := range entries {
				child := e.Name()
				if rel != "." {
					child = path.Join(rel, e.Name())
				}
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if _, ok := seen[rel]; ok {
			return nil
		}
		seen[rel] = struct{}{}
		out = append(out, archiveFile{rel: rel, size: info.Size()})
		total += info.Size()
		if len(out) > MaxTarballFiles {
			return fmt.Errorf("archive exceeds %d file limit", MaxTarballFiles)
		}
		if total > MaxTarballUncompressedBytes {
			return fmt.Errorf("archive exceeds %d byte uncompressed limit", MaxTarballUncompressedBytes)
		}
		return nil
	}

	for _, p := range paths {
		if err := walk(p); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

func streamTarball(ctx context.Context, root *os.Root, requestID, filename string, files []archiveFile, send SendFunc) error {
	cw := newChunkWriter(ctx, send, requestID, filename, pb.TransferKind_TRANSFER_KIND_TARBALL)
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)

	for _, af := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(af.rel)
		if err != nil {
			_ = sendError(ctx, send, requestID, fmt.Sprintf("stat %s: %v", af.rel, err))
			return err
		}
		if !info.Mode().IsRegular() {
			_ = sendError(ctx, send, requestID, fmt.Sprintf("%s vanished or changed type", af.rel))
			return fmt.Errorf("file changed: %s", af.rel)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = sendError(ctx, send, requestID, err.Error())
			return err
		}
		hdr.Name = af.rel
		if err := tw.WriteHeader(hdr); err != nil {
			_ = sendError(ctx, send, requestID, err.Error())
			return err
		}
		f, err := root.Open(af.rel)
		if err != nil {
			_ = sendError(ctx, send, requestID, fmt.Sprintf("open %s: %v", af.rel, err))
			return err
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		if copyErr != nil {
			_ = sendError(ctx, send, requestID, fmt.Sprintf("read %s: %v", af.rel, copyErr))
			return copyErr
		}
	}
	if err := tw.Close(); err != nil {
		_ = sendError(ctx, send, requestID, err.Error())
		return err
	}
	if err := gz.Close(); err != nil {
		_ = sendError(ctx, send, requestID, err.Error())
		return err
	}
	return cw.Close()
}

type chunkWriter struct {
	ctx       context.Context
	send      SendFunc
	requestID string
	filename  string
	kind      pb.TransferKind
	seq       uint32
	buf        []byte
	hash       hash.Hash
	closed     bool
	headerSent bool
}

func newChunkWriter(ctx context.Context, send SendFunc, requestID, filename string, kind pb.TransferKind) *chunkWriter {
	return &chunkWriter{
		ctx:       ctx,
		send:      send,
		requestID: requestID,
		filename:  filename,
		kind:      kind,
		buf:       make([]byte, 0, ChunkSize),
		hash:      sha256.New(),
	}
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("chunk writer closed")
	}
	n := 0
	for len(p) > 0 {
		space := ChunkSize - len(w.buf)
		if space == 0 {
			if err := w.flush(false); err != nil {
				return n, err
			}
			space = ChunkSize
		}
		take := space
		if take > len(p) {
			take = len(p)
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		n += take
	}
	return n, nil
}

func (w *chunkWriter) Close() error {
	if w.closed {
		return nil
	}
	if err := w.flush(true); err != nil {
		return err
	}
	w.closed = true
	return nil
}

func (w *chunkWriter) flush(eof bool) error {
	if !eof && len(w.buf) == 0 {
		return nil
	}
	// Empty file: still emit one eof chunk.
	data := append([]byte(nil), w.buf...)
	if len(data) > 0 {
		_, _ = w.hash.Write(data)
	}
	chunk := &pb.FileChunk{
		RequestId: w.requestID,
		Seq:       w.seq,
		Data:      data,
		Eof:       eof,
	}
	if !w.headerSent {
		chunk.Filename = w.filename
		chunk.TransferKind = w.kind
		w.headerSent = true
	}
	if eof {
		chunk.Sha256 = hex.EncodeToString(w.hash.Sum(nil))
	}
	w.buf = w.buf[:0]
	w.seq++
	return w.send(w.ctx, &pb.AgentMessage{
		Payload: &pb.AgentMessage_FileChunk{FileChunk: chunk},
	})
}

func sendError(ctx context.Context, send SendFunc, requestID, msg string) error {
	return send(ctx, &pb.AgentMessage{
		Payload: &pb.AgentMessage_FileChunk{FileChunk: &pb.FileChunk{
			RequestId: requestID,
			Error:     msg,
			Eof:       true,
		}},
	})
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "files"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
