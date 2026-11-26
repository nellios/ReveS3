package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloud "cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/s3api"
	"github.com/versity/versitygw/s3response"
)

func testMappedGateway(t *testing.T, writes bool, directoryMode ...bool) (*fakeCloud, *Backend, *s3.Client, string) {
	t.Helper()
	return testMappedGatewayConfigured(t, writes, nil, directoryMode...)
}

func testMappedGatewayConfigured(t *testing.T, writes bool, configure func(*config.Config), directoryMode ...bool) (*fakeCloud, *Backend, *s3.Client, string) {
	t.Helper()
	fake := newFake(t)
	for _, dir := range []string{"documents", "备份 中文"} {
		fake.add(dir, nil, true)
		fake.add(dir+"/shared.txt", []byte("content in "+dir), false)
		fake.add(dir+"/second.txt", []byte("second in "+dir), false)
	}
	c := config.Config{Site: fake.server.URL, AllowHTTP: true, Region: "us-east-1", Listen: "127.0.0.1:0", SessionFile: filepath.Join(t.TempDir(), "session.json"), TimeoutSeconds: 10, MaxEntries: 100, MaxConcurrent: 8, WriteEnabled: writes, UploadDir: t.TempDir(), Buckets: []config.BucketMapping{
		{Name: "documents", Root: "cloudreve://my/root/documents"},
		{Name: "backups", Root: "cloudreve://my/root/备份 中文"},
	}}
	if len(directoryMode) > 0 && directoryMode[0] {
		c.Buckets = nil
		c.MultipartEnabled = len(directoryMode) > 1 && directoryMode[1]
		c.BucketsRoot = "cloudreve://my/root/bucket-parent"
		fake.add("bucket-parent", nil, true)
		for _, name := range []string{"test", "backups"} {
			fake.add("bucket-parent/"+name, nil, true)
			fake.add("bucket-parent/"+name+"/shared.txt", []byte("content in "+name), false)
		}
	}
	if configure != nil {
		configure(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	client := cloud.New(c.Site, "", time.Second*10)
	if err := client.Login(context.Background(), "test", "test"); err != nil {
		t.Fatal(err)
	}
	be := New(c, client, testSecret)
	if err := be.CheckBuckets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := be.RecoverWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	addresses := make(chan []net.Addr, 1)
	server, err := NewServer(be, testAccess, testSecret, s3api.WithOnListenAddrs(func(a []net.Addr) { addresses <- a }))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeMultiPort([]string{c.Listen}) }()
	endpoint := ""
	select {
	case a := <-addresses:
		endpoint = "http://" + a[0].String()
	case err := <-done:
		t.Fatalf("server failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		server.ShutDown()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	sdk := s3.New(s3.Options{Region: c.Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(testAccess, testSecret, ""), RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired})
	return fake, be, sdk, endpoint
}

func TestMappedBucketsReadAndList(t *testing.T) {
	fake, _, sdk, endpoint := testMappedGateway(t, false)
	ctx := context.Background()
	list, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil || len(list.Buckets) != 2 || aws.ToString(list.Buckets[0].Name) != "backups" || aws.ToString(list.Buckets[1].Name) != "documents" {
		t.Fatalf("configured sorted buckets: %#v, %v", list, err)
	}
	for name, dir := range map[string]string{"documents": "documents", "backups": "备份 中文"} {
		bucket := aws.String(name)
		if _, err := sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket}); err != nil {
			t.Fatal(err)
		}
		if _, err := sdk.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: bucket}); err != nil {
			t.Fatal(err)
		}
		objects, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
		if err != nil || len(objects.Contents) != 2 || aws.ToString(objects.Contents[0].Key) != "second.txt" || aws.ToString(objects.Contents[1].Key) != "shared.txt" {
			t.Fatalf("bucket %s leaked objects: %#v, %v", name, objects, err)
		}
		legacy, err := sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, Prefix: aws.String("shared")})
		if err != nil || len(legacy.Contents) != 1 {
			t.Fatalf("v1 listing: %#v, %v", legacy, err)
		}
		head, err := sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("shared.txt")})
		if err != nil || aws.ToInt64(head.ContentLength) != int64(len("content in "+dir)) {
			t.Fatalf("head: %#v, %v", head, err)
		}
		object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("shared.txt")})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(object.Body)
		object.Body.Close()
		if readErr != nil || string(body) != "content in "+dir {
			t.Fatalf("wrong root: %q, %v", body, readErr)
		}
	}
	before := fake.calls.Load()
	if _, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("unconfigured"), Key: aws.String("a.bin")}); errorCode(err) != "NoSuchBucket" {
		t.Fatalf("unconfigured bucket: %v", err)
	}
	if fake.calls.Load() != before {
		t.Fatal("unknown bucket reached upstream")
	}
	if _, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("documents"), Key: aws.String("../备份 中文/shared.txt")}); err == nil {
		t.Fatal("cross-bucket traversal accepted")
	}
	if fake.calls.Load() != before {
		t.Fatal("traversal reached upstream")
	}
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("documents"), Key: aws.String("blocked"), Body: strings.NewReader("x")}); errorCode(err) != "AccessDenied" {
		t.Fatalf("read-only mapping allowed upload: %v", err)
	}
	// WinSCP's login probe must expose both buckets and retain SigV4 coverage.
	request, _ := http.NewRequest(http.MethodGet, endpoint+"/?max-keys=1", nil)
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
	if err != nil || response.StatusCode != 200 || len(probe.Buckets.Bucket) != 2 {
		t.Fatalf("login probe: %#v, %v", probe, err)
	}
}

func TestMappedBucketPaginationTokens(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, false)
	ctx := context.Background()
	first, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1)})
	if err != nil || len(first.Buckets) != 1 || aws.ToString(first.Buckets[0].Name) != "backups" || aws.ToString(first.ContinuationToken) == "" {
		t.Fatalf("first bucket page: %#v, %v", first, err)
	}
	second, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1), ContinuationToken: first.ContinuationToken})
	if err != nil || len(second.Buckets) != 1 || aws.ToString(second.Buckets[0].Name) != "documents" || aws.ToString(second.ContinuationToken) != "" {
		t.Fatalf("second bucket page: %#v, %v", second, err)
	}
	filtered, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: aws.String("doc"), BucketRegion: aws.String("us-east-1")})
	if err != nil || len(filtered.Buckets) != 1 || aws.ToString(filtered.Buckets[0].Name) != "documents" {
		t.Fatalf("prefix: %#v, %v", filtered, err)
	}
	empty, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: aws.String("missing")})
	if err != nil || len(empty.Buckets) != 0 {
		t.Fatalf("prefix should be empty: %#v, %v", empty, err)
	}
	objects, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("documents"), MaxKeys: aws.Int32(1)})
	if err != nil || aws.ToString(objects.NextContinuationToken) == "" {
		t.Fatal("missing object cursor", err)
	}
	before := fake.calls.Load()
	for _, input := range []*s3.ListBucketsInput{
		{MaxBuckets: aws.Int32(0)},
		{MaxBuckets: aws.Int32(-1)},
		{MaxBuckets: aws.Int32(10001)},
		{ContinuationToken: aws.String("tampered")},
		{ContinuationToken: first.ContinuationToken, Prefix: aws.String("doc")},
		{ContinuationToken: objects.NextContinuationToken},
		{BucketRegion: aws.String("eu-west-1")},
	} {
		if _, err := sdk.ListBuckets(ctx, input); err == nil {
			t.Fatalf("invalid listing accepted: %#v", input)
		}
	}
	if _, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("backups"), ContinuationToken: objects.NextContinuationToken}); err == nil {
		t.Fatal("cross-bucket object cursor accepted")
	}
	if _, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("documents"), ContinuationToken: first.ContinuationToken}); err == nil {
		t.Fatal("bucket cursor accepted for objects")
	}
	if fake.calls.Load() != before {
		t.Fatal("invalid listing reached upstream")
	}
	// The same token survives configuration ordering, but not remapping a root.
	copy := *be
	copy.Config.Buckets = be.Config.BucketMappings()
	copy.Config.Buckets[0], copy.Config.Buckets[1] = copy.Config.Buckets[1], copy.Config.Buckets[0]
	if _, err := newMappedBackend(&copy).ListBuckets(ctx, s3response.ListBucketsInput{ContinuationToken: *first.ContinuationToken}); err != nil {
		t.Fatal(err)
	}
	copy.Config.Buckets[0].Root += "-changed"
	if _, err := newMappedBackend(&copy).ListBuckets(ctx, s3response.ListBucketsInput{ContinuationToken: *first.ContinuationToken}); err == nil {
		t.Fatal("token survived a changed mapping")
	}
}

func TestMappedBucketWritesAndRecoveryIsolation(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, true)
	ctx := context.Background()
	key := aws.String("nested/shared.txt")
	for _, name := range []string{"documents", "backups"} {
		if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(name), Key: key, Body: strings.NewReader(name)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("documents"), Key: key, Body: strings.NewReader("replacement")}); err != nil {
		t.Fatal(err)
	}
	updated, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("documents"), Key: key})
	if err != nil {
		t.Fatal(err)
	}
	updatedBody, readErr := io.ReadAll(updated.Body)
	updated.Body.Close()
	if readErr != nil || string(updatedBody) != "replacement" {
		t.Fatalf("overwrite was not visible: %q, %v", updatedBody, readErr)
	}
	if _, err := sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("documents"), Key: key}); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("documents"), Key: key}); errorCode(err) != "NoSuchKey" {
		t.Fatalf("deleted object remained visible: %v", err)
	}
	object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("backups"), Key: key})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || string(body) != "backups" {
		t.Fatalf("other bucket was overwritten/deleted: %q, %v", body, err)
	}
	if _, exists := fake.files["nested/shared.txt"]; exists {
		t.Fatal("write escaped into the common ancestor")
	}
	router := newMappedBackend(be)
	one, two := router.byName["documents"], router.byName["backups"]
	if one.Config.UploadDir == two.Config.UploadDir || one.slots != two.slots || one.writes != two.writes {
		t.Fatal("state must be separate and account resource limits shared")
	}
	if one.journalPath("shared.txt", "same-session") == two.journalPath("shared.txt", "same-session") {
		t.Fatal("journal collision")
	}
	// Completed uploads with the same key and session ID recover independently.
	for _, child := range []*Backend{one, two} {
		if err := child.writeDirectory(); err != nil {
			t.Fatal(err)
		}
		name := child.journalPath("shared.txt", "same-session")
		if err := saveJournal(name, uploadJournal{Site: child.Config.Site, Root: child.Config.Root, Key: "shared.txt", SessionID: "same-session"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := be.RecoverWrites(ctx); err != nil {
		t.Fatal(err)
	}
	for _, child := range []*Backend{one, two} {
		if _, err := os.Stat(child.journalPath("shared.txt", "same-session")); !os.IsNotExist(err) {
			t.Fatal("journal not recovered", err)
		}
	}
	if _, err := sdk.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("new-bucket")}); err == nil {
		t.Fatal("bucket creation unexpectedly supported")
	}
	if _, err := sdk.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("backups")}); err == nil {
		t.Fatal("bucket deletion unexpectedly supported")
	}
	if _, err := sdk.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("backups"), Key: aws.String("not-supported")}); err == nil {
		t.Fatal("multi-bucket multipart unexpectedly supported")
	}
}

func TestMappedStartupAndStateLayout(t *testing.T) {
	fake, be, _, _ := testMappedGateway(t, true)
	ctx := context.Background()
	before := fake.calls.Load()
	if err := be.CheckBuckets(ctx); err != nil || fake.calls.Load()-before != 4 {
		t.Fatalf("not all roots checked: %v", err)
	}
	file := fake.files["documents"]
	delete(fake.files, "documents")
	if err := be.CheckBuckets(ctx); err == nil {
		t.Fatal("missing directory accepted")
	}
	fake.files["documents"] = file
	file.Metadata["sys:shared_redirect"] = "outside"
	if err := be.CheckBuckets(ctx); err == nil {
		t.Fatal("shared redirect accepted")
	}
	delete(file.Metadata, "sys:shared_redirect")
	old := filepath.Join(be.Config.UploadDir, "old.json")
	if err := os.WriteFile(old, []byte(`{"session_id":"do-not-ignore"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := be.RecoverWrites(ctx); err == nil {
		t.Fatal("legacy upload state silently skipped")
	}
	data, err := os.ReadFile(old)
	if err != nil || string(data) != `{"session_id":"do-not-ignore"}` {
		t.Fatal("old state was changed")
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(be.Config.UploadDir, "buckets", "removed-bucket")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	orphanFile := filepath.Join(orphan, ".body-still-pending")
	if err := os.WriteFile(orphanFile, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := be.RecoverWrites(ctx); err == nil {
		t.Fatal("removed bucket state silently skipped")
	}
	if err := os.Remove(orphanFile); err != nil {
		t.Fatal(err)
	}
	if err := be.RecoverWrites(ctx); err != nil {
		t.Fatal("empty old state directory blocked startup", err)
	}
	legacy := *be
	legacy.Config.Buckets = nil
	legacy.Config.Root, legacy.Config.Bucket = "cloudreve://my/root/documents", "documents"
	pending := filepath.Join(newMappedBackend(be).byName["documents"].Config.UploadDir, "pending.json")
	if err := os.WriteFile(pending, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := legacy.RecoverWrites(ctx); err == nil {
		t.Fatal("multi-bucket state silently skipped by legacy mode")
	}
	// Unknown extra files were not deleted during any failed recovery.
	got, err := os.ReadFile(pending)
	if err != nil || string(got) != "pending" {
		t.Fatal("pending state changed", err)
	}
}

func TestMappedBucketsShareDownloadLimit(t *testing.T) {
	_, be, _, _ := testMappedGateway(t, false)
	be.slots = make(chan struct{}, 1)
	mapped := newMappedBackend(be)
	ctx := context.Background()
	object, err := mapped.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("documents"), Key: aws.String("shared.txt")})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	if _, err := mapped.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("backups")}); err == nil {
		t.Fatal("another bucket bypassed the occupied download limit")
	}
	if err := object.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mapped.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("backups")}); err != nil {
		t.Fatalf("closed download did not release the shared limit: %v", err)
	}
}

func TestMappedStateNamesArePortable(t *testing.T) {
	c := config.Config{UploadDir: t.TempDir(), WriteEnabled: true, MaxConcurrent: 1}
	for _, name := range []string{"con", "aux", "prn", "com1", "lpt1"} {
		c.Buckets = append(c.Buckets, config.BucketMapping{Name: name, Root: "cloudreve://my/" + name})
	}
	be := New(c, nil, testSecret)
	for _, child := range newMappedBackend(be).ordered {
		if err := child.writeDirectory(); err != nil {
			t.Fatalf("valid S3 name cannot create private state: %s: %v", child.Config.Bucket, err)
		}
		if err := os.WriteFile(filepath.Join(child.Config.UploadDir, ".body-pending"), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := be.checkUploadLayout(); err != nil {
		t.Fatalf("known bucket state rejected: %v", err)
	}
}
