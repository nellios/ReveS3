package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootAndListenMustBeExplicitAndScoped(t *testing.T) {
	good := Config{Site: "https://example.invalid", Root: "cloudreve://my/中文 空格", Bucket: "cloudreve", Region: "us-east-1", Listen: "127.0.0.1:7070", SessionFile: "state/session.json", TimeoutSeconds: 60, MaxEntries: 100, MaxConcurrent: 2}
	if e := good.Validate(); e != nil {
		t.Fatal(e)
	}
	if good.MaxUploadBytes != 32<<20 || good.UploadDir != filepath.Join("state", "uploads") || good.WriteEnabled {
		t.Fatal("write defaults are not bounded and read-only")
	}
	for _, limit := range []int64{-1, 33 << 20} {
		c := good
		c.MaxUploadBytes = limit
		if c.Validate() == nil {
			t.Fatalf("invalid upload cap accepted: %d", limit)
		}
	}
	for _, root := range []string{"", "cloudreve://my/", "cloudreve://my/a/../secret", "cloudreve://my/a/%2e%2e/secret", "cloudreve://my/a//b", "cloudreve://my/a\\b", "cloudreve://other/a", "cloudreve://my/a?search=1"} {
		c := good
		c.Root = root
		if c.Validate() == nil {
			t.Fatalf("unsafe root accepted: %s", root)
		}
	}
	c := good
	c.Listen = "0.0.0.0:7070"
	if c.Validate() == nil {
		t.Fatal("remote listener implicitly enabled")
	}
	c = good
	c.Site = "http://example.invalid"
	if c.Validate() == nil {
		t.Fatal("HTTP implicitly enabled")
	}
	for _, key := range []string{"a/../b", "../secret", "a//b", "/a", "a/", "a\\b", "a\x00b", string([]byte{0xff}), "a\ufffe"} {
		if ValidKey(key) {
			t.Fatal("unsafe key accepted")
		}
	}
	for _, key := range []string{"中文 空格.txt", "a/%2e%2e/b", "a%2Fb.txt", "x+?#%.bin"} {
		if !ValidKey(key) {
			t.Fatal("safe literal key rejected")
		}
	}
}

func TestExplicitBucketMappings(t *testing.T) {
	base := Config{Site: "https://example.invalid", Region: "us-east-1", Listen: "127.0.0.1:7070", SessionFile: "state/session.json", TimeoutSeconds: 60, MaxEntries: 100, MaxConcurrent: 2}
	mappings := []BucketMapping{
		{Name: "documents", Root: "cloudreve://my/S3/文档/"},
		{Name: "backups", Root: "cloudreve://my/S3/备份"},
	}
	c := base
	c.Buckets = mappings
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Root != "" || c.Bucket != "" || strings.HasSuffix(c.Buckets[0].Root, "/") {
		t.Fatal("multi-bucket configuration was not normalized independently")
	}
	if mappings[0].Root != "cloudreve://my/S3/文档/" {
		t.Fatal("validation mutated the caller's slice")
	}
	copy := c.BucketMappings()
	copy[0].Name = "changed"
	if c.Buckets[0].Name != "documents" {
		t.Fatal("mapping accessor returned mutable configuration")
	}
	legacy := base
	legacy.Bucket, legacy.Root = "legacy", "cloudreve://my/legacy"
	if err := legacy.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := legacy.BucketMappings(); len(got) != 1 || got[0].Name != "legacy" || got[0].Root != legacy.Root {
		t.Fatal("legacy mapping changed")
	}
	for _, tc := range []struct {
		name              string
		buckets           []BucketMapping
		legacy, multipart bool
	}{
		{name: "empty", buckets: []BucketMapping{}},
		{name: "mixed", buckets: mappings, legacy: true},
		{name: "duplicate name", buckets: []BucketMapping{{"same", "cloudreve://my/a"}, {"same", "cloudreve://my/b"}}},
		{name: "duplicate normalized root", buckets: []BucketMapping{{"one", "cloudreve://my/a/"}, {"two", "cloudreve://my/%61"}}},
		{name: "nested root", buckets: []BucketMapping{{"one", "cloudreve://my/a"}, {"two", "cloudreve://my/a/b"}}},
		{name: "nested root reversed", buckets: []BucketMapping{{"one", "cloudreve://my/a/b"}, {"two", "cloudreve://my/a"}}},
		{name: "account root", buckets: []BucketMapping{{"one", "cloudreve://my/"}}},
		{name: "traversal", buckets: []BucketMapping{{"one", "cloudreve://my/a/%2e%2e/b"}}},
		{name: "unsafe bucket", buckets: []BucketMapping{{"../bad", "cloudreve://my/a"}}},
		{name: "health route", buckets: []BucketMapping{{"healthz", "cloudreve://my/a"}}},
		{name: "missing root", buckets: []BucketMapping{{Name: "one"}}},
		{name: "too many", buckets: make([]BucketMapping, 101)},
		{name: "multipart prototype", buckets: mappings, multipart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Buckets, c.MultipartEnabled = tc.buckets, tc.multipart
			if tc.legacy {
				c.Root, c.Bucket = legacy.Root, legacy.Bucket
			}
			if c.Validate() == nil {
				t.Fatal("unsafe mapping accepted")
			}
		})
	}
	// A textual prefix is not a nested directory boundary.
	c = base
	c.Buckets = []BucketMapping{{"one", "cloudreve://my/a"}, {"two", "cloudreve://my/ab"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBucketMappingConfigFiles(t *testing.T) {
	for _, name := range []string{"gateway.example.json", "gateway.multibucket.example.json", "gateway.directories.example.json"} {
		if _, err := Load(filepath.Join("..", "..", name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	base := map[string]any{"site": "https://example.invalid", "buckets": []map[string]string{{"name": "documents", "root": "cloudreve://my/docs", "unknown": "typo"}}}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "gateway.json")
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(name); err == nil {
		t.Fatal("unknown mapping field accepted")
	}
}

func TestBucketDirectoriesConfiguration(t *testing.T) {
	base := Config{Site: "https://example.invalid", BucketsRoot: "cloudreve://my/S3/", Region: "us-east-1", Listen: "127.0.0.1:7070", SessionFile: "state/session.json", TimeoutSeconds: 60, MaxEntries: 100, MaxConcurrent: 2}
	c := base
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.BucketsRoot != "cloudreve://my/S3" || !c.MultiBucket() || len(c.BucketMappings()) != 0 {
		t.Fatal("directory mode must retain only a normalized parent, not a fixed mapping")
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Bucket = "test" },
		func(c *Config) { c.Root = "cloudreve://my/fixed" },
		func(c *Config) { c.Buckets = []BucketMapping{} },
		func(c *Config) { c.Buckets = []BucketMapping{{"test", "cloudreve://my/fixed"}} },
		func(c *Config) { c.BucketsRoot = "cloudreve://my/" },
		func(c *Config) { c.BucketsRoot = "cloudreve://my/S3/%2e%2e" },
		func(c *Config) { c.BucketsRoot = "cloudreve://my/S3//nested" },
		func(c *Config) { c.BucketsRoot = "cloudreve://other/S3" },
		func(c *Config) { c.BucketsRoot += "?search=1" },
	} {
		c := base
		change(&c)
		if c.Validate() == nil {
			t.Fatal("invalid directory configuration accepted")
		}
	}
	c = base
	c.MultipartEnabled = true
	if err := c.Validate(); err != nil {
		t.Fatal("directory multipart configuration rejected", err)
	}
}
