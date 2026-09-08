package api

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/objectstore"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

type uploadObjects struct {
	bucket string
	puts   []string
}

func (u *uploadObjects) PresignGet(context.Context, string, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (u *uploadObjects) PresignPut(_ context.Context, bucket, key string, _ time.Duration) (string, time.Time, error) {
	u.puts = append(u.puts, bucket+"/"+key)
	return "https://s3.example/put?" + key, time.Now().Add(objectstore.DefaultPresignPutTTL), nil
}

func (u *uploadObjects) PutObject(context.Context, string, string, io.Reader, int64) error {
	return nil
}

func (u *uploadObjects) Bucket() string { return u.bucket }

func TestCreateArtifactUpload(t *testing.T) {
	st := store.NewMemory(nil)
	objs := &uploadObjects{bucket: "artifacts"}
	srv := New(st, nil, nil, nil).WithObjectStore(objs)

	_, err := srv.CreateArtifactUpload(context.Background(), connect.NewRequest(&pb.CreateArtifactUploadRequest{
		Name: "ml", Version: "v1", Digest: "sha256:abcd",
	}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.CreateArtifactUpload(context.Background(), connect.NewRequest(&pb.CreateArtifactUploadRequest{
		Name: "ml", Version: "v1", Digest: "sha256:abcd", Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Msg.GetPutUrl(), "artifacts/ml/v1/abcd") {
		t.Fatalf("put_url = %q", resp.Msg.GetPutUrl())
	}
	if resp.Msg.GetS3Uri() != "s3://artifacts/artifacts/ml/v1/abcd" {
		t.Fatalf("s3_uri = %q", resp.Msg.GetS3Uri())
	}
	if resp.Msg.GetExpiresAt() == nil {
		t.Fatal("expires_at missing")
	}
}

func TestCreateArtifactUploadRequiresStoreAndDigest(t *testing.T) {
	srv := New(store.NewMemory(nil), nil, nil, nil)
	_, err := srv.CreateArtifactUpload(context.Background(), connect.NewRequest(&pb.CreateArtifactUploadRequest{
		Name: "ml", Version: "v1", Digest: "sha256:abcd",
	}))
	if err == nil {
		t.Fatal("expected FailedPrecondition without object store")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v", connect.CodeOf(err))
	}

	srv = New(store.NewMemory(nil), nil, nil, nil).WithObjectStore(&uploadObjects{bucket: "artifacts"})
	_, err = srv.CreateArtifactUpload(context.Background(), connect.NewRequest(&pb.CreateArtifactUploadRequest{
		Name: "ml", Version: "v1", Digest: "md5:nope",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument for bad digest, got %v", err)
	}
}
