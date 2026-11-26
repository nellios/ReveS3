package gateway

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/versity/versitygw/s3response"
)

func makeMultipartInput(bucket, key string) s3response.CreateMultipartUploadInput {
	return s3response.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
}

func initiateMultipart(t *testing.T, sdk *s3.Client, bucket, key string) *string {
	t.Helper()
	got, err := sdk.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil || aws.ToString(got.UploadId) == "" {
		t.Fatal("initiate", err)
	}
	return got.UploadId
}

func uploadTestPart(t *testing.T, sdk *s3.Client, bucket, key string, id *string, number int32, body []byte) types.CompletedPart {
	t.Helper()
	got, err := sdk.UploadPart(context.Background(), &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: id, PartNumber: aws.Int32(number), Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatal("upload part", number, err)
	}
	return types.CompletedPart{PartNumber: aws.Int32(number), ETag: got.ETag}
}

func completeTestMultipart(sdk *s3.Client, bucket, key string, id *string, parts ...types.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
	return sdk.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: id, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
}

func TestMultipartSDKOutOfOrderRetryRestartAndCompletion(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, true, true, true)
	ctx := context.Background()
	key := "nested/大文件.bin"
	id := initiateMultipart(t, sdk, "test", key)
	if _, err := sdk.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("backups"), Key: aws.String(key)}); errorCode(err) != "SlowDown" {
		t.Fatal("parallel bucket task accepted", err)
	}
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("backups"), Key: aws.String("blocked"), Body: strings.NewReader("x")}); errorCode(err) != "SlowDown" {
		t.Fatal("PUT bypassed global multipart task", err)
	}
	partA := bytes.Repeat([]byte{0x19}, 6<<20)
	partB := bytes.Repeat([]byte{0x37}, 6<<20)
	partC := bytes.Repeat([]byte{0x83}, 6<<20)
	last := bytes.Repeat([]byte{0xff}, (1<<20)+17)
	p2 := uploadTestPart(t, sdk, "test", key, id, 2, partB)
	p1 := uploadTestPart(t, sdk, "test", key, id, 1, bytes.Repeat([]byte{0}, 6<<20))
	p1 = uploadTestPart(t, sdk, "test", key, id, 1, partA)
	// A fresh backend has no in-memory upload state to reuse.
	restarted := New(be.Config, be.Client, testSecret)
	if err := restarted.CheckBuckets(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverWrites(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := newMappedBackend(restarted).ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id, MaxParts: aws.Int32(1)})
	if err != nil || len(page.Parts) != 1 || !page.IsTruncated || page.NextPartNumberMarker != 1 {
		t.Fatal("restart/list parts", page, err)
	}
	page2, err := sdk.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id, PartNumberMarker: aws.String("1")})
	if err != nil || len(page2.Parts) != 1 || page2.Parts[0].PartNumber == nil || *page2.Parts[0].PartNumber != 2 {
		t.Fatal("part pagination", err)
	}
	if _, err := sdk.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("backups"), Key: aws.String(key), UploadId: id}); errorCode(err) != "NoSuchUpload" {
		t.Fatal("cross-bucket UploadId accepted", err)
	}
	p3 := uploadTestPart(t, sdk, "test", key, id, 3, partC)
	p4 := uploadTestPart(t, sdk, "test", key, id, 4, last)
	result, err := completeTestMultipart(sdk, "test", key, id, p1, p2, p3, p4)
	if err != nil {
		t.Fatal("complete", err)
	}
	for i := 0; i < 2; i++ {
		again, err := completeTestMultipart(sdk, "test", key, id, p1, p2, p3, p4)
		if err != nil || aws.ToString(again.ETag) != aws.ToString(result.ETag) {
			t.Fatal("completion retry", err)
		}
	}
	got, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("test"), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, got.Body)
	got.Body.Close()
	expected := sha256.New()
	for _, body := range [][]byte{partA, partB, partC, last} {
		expected.Write(body)
	}
	if err != nil || n != int64(19<<20)+17 || !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("multipart content mismatch", n, err)
	}
	file := fake.files["bucket-parent/test/"+key]
	if file.Size != n {
		t.Fatal("wrong same-name folder")
	}
	child, _ := newMappedBackend(be).directoryChild("test")
	if used, err := child.stagingUsage(); err != nil || used != 0 {
		t.Fatal("completed part bytes leaked", used, err)
	}
	other := initiateMultipart(t, sdk, "backups", "next.bin")
	if _, err := sdk.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("backups"), Key: aws.String("next.bin"), UploadId: other}); err != nil {
		t.Fatal(err)
	}
}

func TestMultipartValidationAbortAndBudget(t *testing.T) {
	_, be, sdk, _ := testMappedGateway(t, true, true, true)
	ctx := context.Background()
	key := "small.bin"
	id := initiateMultipart(t, sdk, "test", key)
	p1 := uploadTestPart(t, sdk, "test", key, id, 1, []byte("one"))
	p2 := uploadTestPart(t, sdk, "test", key, id, 2, []byte("two"))
	if _, err := completeTestMultipart(sdk, "test", key, id, p2, p1); errorCode(err) != "InvalidPartOrder" {
		t.Fatal("unordered complete", err)
	}
	bad := p1
	bad.ETag = aws.String(`"00000000000000000000000000000000"`)
	if _, err := completeTestMultipart(sdk, "test", key, id, bad); errorCode(err) != "InvalidPart" {
		t.Fatal("invalid ETag", err)
	}
	if _, err := completeTestMultipart(sdk, "test", key, id, p1, p2); errorCode(err) != "EntityTooSmall" {
		t.Fatal("small nonfinal part", err)
	}
	if _, err := sdk.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id, PartNumber: aws.Int32(3), Body: strings.NewReader("data"), ContentMD5: aws.String(base64.StdEncoding.EncodeToString(make([]byte, md5.Size)))}); errorCode(err) != "BadDigest" {
		t.Fatal("bad digest", err)
	}
	list, err := sdk.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("test")})
	if err != nil || len(list.Uploads) != 1 {
		t.Fatal("missing active task", err)
	}
	if _, err := sdk.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id}); errorCode(err) != "NoSuchUpload" {
		t.Fatal("abort retained task", err)
	}
	child, _ := newMappedBackend(be).directoryChild("test")
	child.Config.MaxMultipartBytes = 4
	child.Config.MaxStagingBytes = 5
	created, err := child.CreateMultipartUpload(ctx, makeMultipartInput("test", "bounded.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("test"), Key: aws.String("bounded.bin"), UploadId: aws.String(created.UploadId), PartNumber: aws.Int32(1), ContentLength: aws.Int64(5), Body: strings.NewReader("12345")}); directoryErrorCode(err) != "EntityTooLarge" {
		t.Fatal("object cap bypassed", err)
	}
	if _, err := child.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("test"), Key: aws.String("bounded.bin"), UploadId: aws.String(created.UploadId), PartNumber: aws.Int32(1), ContentLength: aws.Int64(4), Body: strings.NewReader("1234")}); err != nil {
		t.Fatal(err)
	}
	if _, err := child.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("test"), Key: aws.String("bounded.bin"), UploadId: aws.String(created.UploadId), PartNumber: aws.Int32(1), ContentLength: aws.Int64(4), Body: strings.NewReader("abcd")}); directoryErrorCode(err) != "InsufficientStorage" {
		t.Fatal("retry temporary disk budget bypassed", err)
	}
	parts, err := child.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("test"), Key: aws.String("bounded.bin"), UploadId: aws.String(created.UploadId)})
	if err != nil || len(parts.Parts) != 1 || parts.Parts[0].Size != 4 {
		t.Fatal("failed retry destroyed old part", err)
	}
	other, _ := newMappedBackend(be).directoryChild("backups")
	if err := os.WriteFile(filepath.Join(other.Config.UploadDir, "orphan-data"), []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	if used, err := child.stagingUsage(); err != nil || used != 7 {
		t.Fatal("disk accounting is not global", used, err)
	}
}

func TestMultipartFailurePreservesOldContentAndRecovery(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, true, true, true)
	ctx := context.Background()
	key := "shared.txt"
	id := initiateMultipart(t, sdk, "test", key)
	part := uploadTestPart(t, sdk, "test", key, id, 1, []byte("replacement"))
	fake.failChunk.Store(true)
	if _, err := completeTestMultipart(sdk, "test", key, id, part); err == nil {
		t.Fatal("upstream failure succeeded")
	}
	fake.failChunk.Store(false)
	got, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("test"), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil || string(body) != "content in test" {
		t.Fatal("old content lost on failed completion", err)
	}
	if err := be.RecoverWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id}); err != nil {
		t.Fatal(err)
	}
	id = initiateMultipart(t, sdk, "test", key)
	part = uploadTestPart(t, sdk, "test", key, id, 1, []byte("replacement"))
	child, _ := newMappedBackend(be).directoryChild("test")
	r, err := child.readMultipart(*id)
	if err != nil {
		t.Fatal(err)
	}
	r.State = "committing"
	if err := child.saveMultipart(r); err != nil {
		t.Fatal(err)
	}
	restarted := New(be.Config, be.Client, testSecret)
	if err := restarted.RecoverWrites(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = child.readMultipart(*id)
	if err != nil || r.State != "failed" {
		t.Fatal("uncertain completion not failed after restart", err)
	}
	if _, err := completeTestMultipart(sdk, "test", key, id, part); errorCode(err) != "OperationAborted" {
		t.Fatal("uncertain completion silently resumed", err)
	}
	r.Created = time.Now().Add(-25 * time.Hour)
	if err := child.saveMultipart(r); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child.recordDir(*id)); !os.IsNotExist(err) {
		t.Fatal("expired private upload data retained", err)
	}
}

func TestMultipartCorruptStagedPartNeverCommits(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, true, true, true)
	id := initiateMultipart(t, sdk, "test", "shared.txt")
	part := uploadTestPart(t, sdk, "test", "shared.txt", id, 1, []byte("replacement"))
	child, _ := newMappedBackend(be).directoryChild("test")
	r, err := child.readMultipart(*id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child.recordDir(*id), r.Parts[1].File), []byte("corruptdata"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := completeTestMultipart(sdk, "test", "shared.txt", id, part); errorCode(err) != "BadDigest" {
		t.Fatal("corrupt bytes accepted", err)
	}
	if file := fake.files["bucket-parent/test/shared.txt"]; string(fake.content[file.Primary]) != "content in test" {
		t.Fatal("corrupt data replaced old object")
	}
}
