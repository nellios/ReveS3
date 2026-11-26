package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloud "cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
)

// Unlike the S3 integration fixtures, this starts only a simulated Cloudreve
// origin. There is no local or remote S3 service and no S3 credentials.
func testDirectBackend(t *testing.T) (*fakeCloud, *Backend) {
	t.Helper()
	fake := newFake(t)
	for _, name := range []string{"bucket-parent", "bucket-parent/test", "bucket-parent/backups"} {
		fake.add(name, nil, true)
	}
	fake.add("bucket-parent/test/shared.txt", []byte("original"), false)
	fake.add("bucket-parent/backups/shared.txt", []byte("sibling"), false)
	c := config.Config{
		Site: fake.server.URL, AllowHTTP: true, Listen: "127.0.0.1:0", Region: "us-east-1",
		BucketsRoot: "cloudreve://my/root/bucket-parent", WriteEnabled: true,
		SessionFile: filepath.Join(t.TempDir(), "session.json"), UploadDir: t.TempDir(),
		MaxConcurrent: 1, MaxEntries: 100, TimeoutSeconds: 30,
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	client := cloud.NewDirect(c.Site, "", 30*time.Second)
	if err := client.Login(context.Background(), "test", "test"); err != nil {
		t.Fatal(err)
	}
	return fake, New(c, client, "")
}

func sourceFile(t *testing.T, body string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func assertNoDirectPayload(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			t.Errorf("unexpected retained upload file: %s", filepath.Base(name))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDirectUploadLargeFromOriginalFile(t *testing.T) {
	fake, be := testDirectBackend(t)
	name := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	size := int64(33<<20) + 17
	if _, err := io.CopyN(io.MultiWriter(file, hash), strings.NewReader(strings.Repeat("x", int(size))), size); err != nil {
		t.Fatal(err)
	}
	file.Close()
	result, err := be.UploadLocalFile(context.Background(), "test", "nested/中文 空格.bin", name, false, true)
	if err != nil || result.Bytes != size || result.Verification != "sha256" || result.SHA256 != hex.EncodeToString(hash.Sum(nil)) {
		t.Fatal("native direct upload/verification", result, err)
	}
	got := fake.files["bucket-parent/test/nested/中文 空格.bin"]
	if got.Size != size || sha256.Sum256(fake.content[got.Primary]) != sha256.Sum256([]byte(strings.Repeat("x", int(size)))) {
		t.Fatal("native upload did not reach the same-name folder")
	}
	if len(fake.uploads) != 0 || fake.downloads.Load() != 1 {
		t.Fatal("unfinished session or missing direct verification download")
	}
	assertNoDirectPayload(t, be.Config.UploadDir)
}

func TestDirectUploadOverwriteAndFailureProtectExisting(t *testing.T) {
	fake, be := testDirectBackend(t)
	ctx := context.Background()
	name := sourceFile(t, "replacement")
	if _, err := be.UploadLocalFile(ctx, "test", "shared.txt", name, false, false); directoryErrorCode(err) != "PreconditionFailed" {
		t.Fatal("default overwrote existing content", err)
	}
	fake.failChunk.Store(true)
	if _, err := be.UploadLocalFile(ctx, "test", "shared.txt", name, true, false); err == nil {
		t.Fatal("upstream failure claimed success")
	}
	old := fake.files["bucket-parent/test/shared.txt"]
	if string(fake.content[old.Primary]) != "original" || len(fake.uploads) != 0 {
		t.Fatal("failed native upload damaged previous content or left a known session")
	}
	fake.failChunk.Store(false)
	if _, err := be.UploadLocalFile(ctx, "test", "shared.txt", name, true, true); err != nil {
		t.Fatal(err)
	}
	changed := fake.files["bucket-parent/test/shared.txt"]
	sibling := fake.files["bucket-parent/backups/shared.txt"]
	if string(fake.content[changed.Primary]) != "replacement" || string(fake.content[sibling.Primary]) != "sibling" {
		t.Fatal("native overwrite or cross-bucket isolation failed")
	}
	assertNoDirectPayload(t, be.Config.UploadDir)
}

func TestDirectUploadDefaultDoesNotDownloadAndSupportsFixedBucket(t *testing.T) {
	fake, be := testDirectBackend(t)
	be.Config.BucketsRoot = ""
	be.Config.Root = "cloudreve://my/root/bucket-parent/test"
	be.Config.Bucket = "test"
	result, err := be.UploadLocalFile(context.Background(), "test", "metadata-only.bin", sourceFile(t, "native"), false, false)
	if err != nil || result.Bytes != 6 || result.Verification != "metadata" || fake.downloads.Load() != 0 {
		t.Fatal("default native upload downloaded content or fixed bucket failed", result, err)
	}
	assertNoDirectPayload(t, be.Config.UploadDir)
}

func TestDirectUploadValidationAndZeroBytes(t *testing.T) {
	fake, be := testDirectBackend(t)
	ctx := context.Background()
	name := sourceFile(t, "abc")
	for _, tc := range []struct{ bucket, key, code string }{
		{"test", "../escape", "InvalidURI"},
		{"../escape", "key", "NoSuchBucket"},
		{"missing", "key", "NoSuchBucket"},
	} {
		if _, err := be.UploadLocalFile(ctx, tc.bucket, tc.key, name, false, false); directoryErrorCode(err) != tc.code {
			t.Errorf("%s/%s: %v", tc.bucket, tc.key, err)
		}
	}
	if _, ok := fake.files["bucket-parent/missing"]; ok {
		t.Fatal("missing bucket was created")
	}
	be.Config.WriteEnabled = false
	if _, err := be.UploadLocalFile(ctx, "test", "key", name, false, false); directoryErrorCode(err) != "AccessDenied" {
		t.Fatal("read-only bypass", err)
	}
	be.Config.WriteEnabled = true
	be.Config.MaxMultipartBytes = 2
	if _, err := be.UploadLocalFile(ctx, "test", "key", name, false, false); directoryErrorCode(err) != "EntityTooLarge" {
		t.Fatal("direct size limit bypass", err)
	}
	be.Config.MaxMultipartBytes = 5 << 30
	zero := sourceFile(t, "")
	result, err := be.UploadLocalFile(ctx, "test", "zero.bin", zero, false, true)
	if err != nil || result.Bytes != 0 || result.Verification != "sha256" {
		t.Fatal("zero-byte native upload failed", result, err)
	}
	fake.truncate.Store(true)
	if _, err := be.UploadLocalFile(ctx, "test", "verify-failure.bin", name, false, true); err == nil || !strings.Contains(err.Error(), "upload completed but") {
		t.Fatal("verification failure misreported upload state", err)
	}
	if _, ok := fake.files["bucket-parent/test/verify-failure.bin"]; !ok {
		t.Fatal("verification failure unexpectedly removed completed file")
	}
	assertNoDirectPayload(t, be.Config.UploadDir)
}

func TestDirectFileDetectsChangesAndCancellation(t *testing.T) {
	name := sourceFile(t, "original")
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	source := &unchangedFile{file: file, info: info, ctx: context.Background()}
	if err := source.check(); err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(name, changed, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("changed source was not detected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source.ctx = ctx
	if _, err := source.ReadAt(make([]byte, 1), 0); err != context.Canceled {
		t.Fatal("cancellation ignored", err)
	}
}
