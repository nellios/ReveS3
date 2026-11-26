package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/s3response"
)

func testSingleBucketGateway(t *testing.T, writes bool) (*fakeCloud, *Backend, *s3.Client, string) {
	t.Helper()
	return testMappedGatewayConfigured(t, writes, func(c *config.Config) {
		c.SingleBucketMode = true
		c.BucketsRoot += "/test"
	}, true, true)
}

func TestSingleBucketModeSDKReadWriteAndLoginProbe(t *testing.T) {
	fake, be, sdk, endpoint := testSingleBucketGateway(t, true)
	ctx := context.Background()
	bucket := aws.String("test")
	if be.Config.MultiBucket() || be.Config.Root != "cloudreve://my/root/bucket-parent/test" {
		t.Fatal("single bucket did not select the directory itself")
	}
	list, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(list.Buckets) != 1 || aws.ToString(list.Buckets[0].Name) != "test" {
		t.Fatal("same-name bucket listing", list, err)
	}
	if _, err := sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket}); err != nil {
		t.Fatal(err)
	}
	legacy, err := sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket})
	if err != nil || len(legacy.Contents) != 1 || aws.ToString(legacy.Contents[0].Key) != "shared.txt" {
		t.Fatal("legacy listing did not use the directory itself", legacy, err)
	}
	modern, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if err != nil || len(modern.Contents) != 1 || aws.ToString(modern.Contents[0].Key) != "shared.txt" {
		t.Fatal("v2 listing did not use the directory itself", modern, err)
	}
	// WinSCP probes the service root even when a remote bucket is configured.
	request, err := http.NewRequest(http.MethodGet, endpoint+"/?max-keys=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nil)
	hash := hex.EncodeToString(sum[:])
	request.Header.Set("X-Amz-Content-Sha256", hash)
	if err := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: testAccess, SecretAccessKey: testSecret}, request, hash, "s3", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var probe s3response.ListAllMyBucketsResult
	err = xml.NewDecoder(response.Body).Decode(&probe)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || len(probe.Buckets.Bucket) != 1 {
		t.Fatal("WinSCP login probe", probe, err)
	}
	// Backup clients such as Netcatty upload and overwrite JSON objects.
	key := "同步/settings.json"
	for _, body := range []string{`{"revision":1,"content":"initial"}`, `{"revision":2}`} {
		if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String(key), ContentType: aws.String("application/json"), Body: strings.NewReader(body)}); err != nil {
			t.Fatal("JSON upload", err)
		}
		object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := io.ReadAll(object.Body)
		object.Body.Close()
		if readErr != nil || string(got) != body {
			t.Fatal("read after overwrite", readErr)
		}
		file, ok := fake.files["bucket-parent/test/"+key]
		if !ok || !bytes.Equal(fake.content[file.Primary], []byte(body)) {
			t.Fatal("upload did not land directly inside the bucket directory")
		}
	}
	if _, err := sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: aws.String(key)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.files["bucket-parent/test/"+key]; ok {
		t.Fatal("delete used the wrong directory")
	}
	before := fake.calls.Load()
	if _, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("backups"), Key: aws.String("shared.txt")}); errorCode(err) != "NoSuchBucket" || fake.calls.Load() != before {
		t.Fatal("sibling bucket was exposed", err)
	}
	for path := range fake.files {
		if strings.HasPrefix(path, "bucket-parent/test/test/") {
			t.Fatal("bucket name was appended twice")
		}
	}
}

func TestSingleBucketModeMultipartRecovery(t *testing.T) {
	fake, be, sdk, _ := testSingleBucketGateway(t, true)
	ctx := context.Background()
	key := "nested/large.bin"
	id := initiateMultipart(t, sdk, "test", key)
	first := bytes.Repeat([]byte{0x51}, 6<<20)
	last := []byte("final part")
	p1 := uploadTestPart(t, sdk, "test", key, id, 1, first)
	restarted := New(be.Config, be.Client, testSecret)
	if err := restarted.CheckBuckets(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverWrites(ctx); err != nil {
		t.Fatal("single-bucket state recovery", err)
	}
	parts, err := restarted.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("test"), Key: aws.String(key), UploadId: id})
	if err != nil || len(parts.Parts) != 1 || parts.Parts[0].PartNumber != 1 {
		t.Fatal("persisted part was lost", err)
	}
	p2 := uploadTestPart(t, sdk, "test", key, id, 2, last)
	if _, err := completeTestMultipart(sdk, "test", key, id, p1, p2); err != nil {
		t.Fatal("complete", err)
	}
	object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("test"), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(object.Body)
	object.Body.Close()
	want := append(first, last...)
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatal("multipart readback", readErr)
	}
	file, ok := fake.files["bucket-parent/test/"+key]
	if !ok || !bytes.Equal(fake.content[file.Primary], want) {
		t.Fatal("multipart used an extra bucket directory")
	}
}

func TestSingleBucketModeSwitchPreservesPendingState(t *testing.T) {
	for _, single := range []bool{false, true} {
		name, oldLayout := "to-multiple", ""
		if single {
			name, oldLayout = "to-single", "directories/bucket-test"
		}
		t.Run(name, func(t *testing.T) {
			_, be, _, _ := testMappedGatewayConfigured(t, true, func(c *config.Config) {
				c.SingleBucketMode = single
				if single {
					c.BucketsRoot += "/test"
				}
			}, true, true)
			stateDir := filepath.Join(be.Config.UploadDir, oldLayout)
			if err := os.MkdirAll(stateDir, 0700); err != nil {
				t.Fatal(err)
			}
			stateFile := filepath.Join(stateDir, ".body-pending")
			if err := os.WriteFile(stateFile, []byte("preserve pending body"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := be.RecoverWrites(context.Background()); err == nil {
				t.Fatal("old mode upload state was silently skipped")
			}
			got, err := os.ReadFile(stateFile)
			if err != nil || string(got) != "preserve pending body" {
				t.Fatal("mode switch changed the old upload state", err)
			}
		})
	}
}
