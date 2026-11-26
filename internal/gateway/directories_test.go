package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cloud "cloudreve-s3/internal/cloudreve"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

func directoryErrorCode(err error) string {
	var api s3err.S3Error
	if errors.As(err, &api) {
		return api.BaseError().Code
	}
	return errorCode(err)
}

func directoryBucketNames(t *testing.T, sdk *s3.Client) []string {
	t.Helper()
	listing, err := sdk.ListBuckets(context.Background(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, bucket := range listing.Buckets {
		names = append(names, aws.ToString(bucket.Name))
	}
	return names
}

func TestDirectoryBucketsFollowSameNamedFoldersWithoutRestart(t *testing.T) {
	fake, _, sdk, _ := testMappedGateway(t, true, true)
	ctx := context.Background()
	for _, path := range []string{"bucket-parent/中文", "bucket-parent/space name", "bucket-parent/Upper", "bucket-parent/ab", "bucket-parent/healthz", "bucket-parent/test/inner-bucket"} {
		fake.add(path, nil, true)
	}
	fake.add("bucket-parent/plain-file", []byte("not a bucket"), false)
	if got := directoryBucketNames(t, sdk); !reflect.DeepEqual(got, []string{"backups", "test"}) {
		t.Fatalf("unexpected discovery: %v", got)
	}
	key := aws.String("nested/中文 空格.txt")
	for _, bucket := range []string{"test", "backups"} {
		if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: key, Body: strings.NewReader(bucket)}); err != nil {
			t.Fatal(err)
		}
		file, ok := fake.files["bucket-parent/"+bucket+"/"+*key]
		if !ok || string(fake.content[file.Primary]) != bucket {
			t.Fatal("write did not reach the identically named folder")
		}
	}
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("test"), Key: key, Body: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("test"), Key: key}); err != nil {
		t.Fatal(err)
	}
	object, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("backups"), Key: key})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(object.Body)
	object.Body.Close()
	if err != nil || string(body) != "backups" {
		t.Fatal("cross-bucket overwrite/delete", err)
	}
	fake.add("bucket-parent/new-bucket", nil, true)
	if _, err := sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("new-bucket")}); err != nil {
		t.Fatal("new folder requires restart", err)
	}
	if got := directoryBucketNames(t, sdk); !reflect.DeepEqual(got, []string{"backups", "new-bucket", "test"}) {
		t.Fatal("new folder not discovered", got)
	}
	delete(fake.files, "bucket-parent/new-bucket")
	fake.add("bucket-parent/renamed", nil, true)
	if _, err := sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("new-bucket")}); err == nil {
		t.Fatal("removed folder remained a bucket")
	}
	if got := directoryBucketNames(t, sdk); !reflect.DeepEqual(got, []string{"backups", "renamed", "test"}) {
		t.Fatal("rename not discovered", got)
	}
	for _, bucket := range []string{"missing", "plain-file", "inner-bucket"} {
		if _, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("shared.txt")}); errorCode(err) != "NoSuchBucket" {
			t.Fatalf("%s is not a bucket: %v", bucket, err)
		}
		if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("a.txt"), Body: strings.NewReader("x")}); errorCode(err) != "NoSuchBucket" {
			t.Fatalf("missing bucket was writable: %v", err)
		}
	}
	if _, ok := fake.files["bucket-parent/missing"]; ok {
		t.Fatal("PUT created a bucket directory")
	}
	if _, err := sdk.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("new-bucket")}); err == nil {
		t.Fatal("S3 bucket creation was enabled")
	}
	if _, err := sdk.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("backups")}); err == nil {
		t.Fatal("S3 bucket deletion was enabled")
	}
}

func TestDirectoryBucketListingTokens(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, false, true)
	ctx := context.Background()
	first, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{MaxBuckets: aws.Int32(1)})
	if err != nil || len(first.Buckets) != 1 || aws.ToString(first.Buckets[0].Name) != "backups" || aws.ToString(first.ContinuationToken) == "" {
		t.Fatal("missing first page", err)
	}
	// Directory changes do not invalidate a cursor scoped to the same parent.
	fake.add("bucket-parent/fresh", nil, true)
	second, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{ContinuationToken: first.ContinuationToken})
	if err != nil || len(second.Buckets) != 2 || aws.ToString(second.Buckets[0].Name) != "fresh" {
		t.Fatal("directory changes invalidated cursor", err)
	}
	filtered, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{Prefix: aws.String("te")})
	if err != nil || len(filtered.Buckets) != 1 || aws.ToString(filtered.Buckets[0].Name) != "test" {
		t.Fatal("wrong prefix results", err)
	}
	before := fake.calls.Load()
	if _, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{ContinuationToken: first.ContinuationToken, Prefix: aws.String("te")}); err == nil {
		t.Fatal("cross-prefix token accepted")
	}
	copy := *be
	copy.Config.BucketsRoot += "-other"
	if _, err := newMappedBackend(&copy).ListBuckets(ctx, s3response.ListBucketsInput{ContinuationToken: *first.ContinuationToken}); err == nil {
		t.Fatal("cross-parent token accepted")
	}
	if fake.calls.Load() != before {
		t.Fatal("invalid cursor reached upstream")
	}
	delete(fake.files, "bucket-parent/backups")
	delete(fake.files, "bucket-parent/fresh")
	delete(fake.files, "bucket-parent/test")
	if got := directoryBucketNames(t, sdk); len(got) != 0 {
		t.Fatal("empty parent should be valid", got)
	}
}

func TestDirectoryBucketsRespectAuthenticationAndGlobalLimits(t *testing.T) {
	fake, be, sdk, endpoint := testMappedGateway(t, false, true)
	ctx := context.Background()
	before := fake.calls.Load()
	bad := s3.New(s3.Options{Region: be.Config.Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(testAccess, strings.Repeat("wrong", 8), ""), RetryMaxAttempts: 1})
	if _, err := bad.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("test")}); err == nil {
		t.Fatal("invalid signature accepted")
	}
	if fake.calls.Load() != before {
		t.Fatal("invalid authentication reached upstream")
	}
	for _, name := range []string{"Upper", "ab", "a..b", "../test"} {
		if _, err := newMappedBackend(be).HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)}); directoryErrorCode(err) != "NoSuchBucket" {
			t.Fatal("invalid bucket name accepted", err)
		}
	}
	if fake.calls.Load() != before {
		t.Fatal("invalid bucket name reached upstream")
	}
	if _, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("test"), Key: aws.String("no"), Body: strings.NewReader("x")}); errorCode(err) != "AccessDenied" {
		t.Fatal("read-only directory was writable", err)
	}
	be.slots = make(chan struct{}, 1)
	router := newMappedBackend(be)
	object, err := router.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("test"), Key: aws.String("shared.txt")})
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	if _, err := router.ListBuckets(ctx, s3response.ListBucketsInput{}); directoryErrorCode(err) != "SlowDown" {
		t.Fatal("discovery bypassed global concurrency limit", err)
	}
	if _, err := router.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("backups")}); directoryErrorCode(err) != "SlowDown" {
		t.Fatal("bucket bypassed shared limit", err)
	}
	object.Body.Close()
	if _, err := router.ListBuckets(ctx, s3response.ListBucketsInput{}); err != nil {
		t.Fatal("download slot not released", err)
	}
}

func TestDirectoryBucketRecoveryDoesNotLoseRenamedOrLegacyState(t *testing.T) {
	fake, be, _, _ := testMappedGateway(t, true, true)
	ctx := context.Background()
	router := newMappedBackend(be)
	for _, name := range []string{"test", "backups"} {
		child, err := router.directoryChild(name)
		if err != nil {
			t.Fatal(err)
		}
		if child.slots != be.slots || child.writes != be.writes || child.Config.UploadDir != filepath.Join(be.Config.UploadDir, "directories", bucketStateName(name)) {
			t.Fatal("state/locks not scoped correctly")
		}
		if err := saveJournal(child.journalPath("shared.txt", "completed"), uploadJournal{Site: child.Config.Site, Root: child.Config.Root, Key: "shared.txt", SessionID: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := be.RecoverWrites(ctx); err != nil {
		t.Fatal("completed records did not recover independently", err)
	}
	child, _ := router.directoryChild("test")
	old := filepath.Join(child.Config.UploadDir, ".body-pending")
	if err := os.WriteFile(old, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := fake.files["bucket-parent/test"]
	delete(fake.files, "bucket-parent/test")
	if err := be.RecoverWrites(ctx); err == nil {
		t.Fatal("removed bucket's pending state was ignored")
	}
	if body, err := os.ReadFile(old); err != nil || string(body) != "preserve" {
		t.Fatal("orphaned state was deleted", err)
	}
	fake.files["bucket-parent/test"] = dir
	if err := be.RecoverWrites(ctx); err != nil {
		t.Fatal("restored bucket could not recover", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("interrupted spool not cleaned after restoration")
	}
	for _, prefix := range []string{"", "buckets/bucket-test"} {
		oldDir := filepath.Join(be.Config.UploadDir, prefix)
		if err := os.MkdirAll(oldDir, 0700); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(oldDir, "pending.json")
		if err := os.WriteFile(name, []byte("old-layout"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := be.RecoverWrites(ctx); err == nil {
			t.Fatal("old layout was ignored")
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectoryDiscoveryRejectsSharedAndInconsistentPaths(t *testing.T) {
	fake, be, sdk, _ := testMappedGateway(t, false, true)
	ctx := context.Background()
	file := fake.files["bucket-parent/test"]
	file.Metadata["sys:shared_redirect"] = "outside"
	if _, err := sdk.ListBuckets(ctx, &s3.ListBucketsInput{}); errorCode(err) != "AccessDenied" {
		t.Fatal("shared folder appeared as a bucket", err)
	}
	if _, err := sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("test")}); err == nil {
		t.Fatal("shared bucket was accessible")
	}
	delete(file.Metadata, "sys:shared_redirect")
	file.Path = "cloudreve://my/root/outside"
	fake.files["bucket-parent/test"] = file
	if err := be.CheckBuckets(ctx); directoryErrorCode(err) != "AccessDenied" {
		t.Fatal("inconsistent directory path accepted", err)
	}
}

func TestDirectoryDiscoveryBudgetsAndPaginationFailures(t *testing.T) {
	for _, mode := range []string{"entry-budget", "duplicate", "cursor-loop", "page-budget", "bad-parent"} {
		t.Run(mode, func(t *testing.T) {
			fake, be, _, _ := testMappedGateway(t, false, true)
			if mode == "entry-budget" {
				fake.add("bucket-parent/ignored file", nil, false)
				be.Config.MaxEntries = 2
				if err := be.CheckBuckets(context.Background()); err == nil {
					t.Fatal("ignored files escaped the resource budget")
				}
				return
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/file" || r.Method != "GET" {
					fake.handle(w, r)
					return
				}
				parent := fake.files["bucket-parent"]
				files, next := []cloud.File{}, "loop"
				switch mode {
				case "duplicate":
					files = []cloud.File{fake.files["bucket-parent/test"], fake.files["bucket-parent/test"]}
					next = ""
				case "page-budget":
					next = "page-" + r.URL.Query().Get("next_page_token")
				case "bad-parent":
					parent.Path += "-outside"
					next = ""
				}
				json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"files": files, "parent": parent, "pagination": map[string]any{"is_cursor": true, "next_token": next}}})
			}))
			defer server.Close()
			be.Client = cloud.New(server.URL, "", time.Second*10)
			if err := be.Client.Login(context.Background(), "test", "test"); err != nil {
				t.Fatal(err)
			}
			be.Config.MaxEntries = 2
			if err := be.CheckBuckets(context.Background()); err == nil {
				t.Fatal("unsafe or endless upstream directory accepted")
			}
		})
	}
}
