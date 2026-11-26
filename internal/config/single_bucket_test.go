package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func singleBucketConfig() Config {
	return Config{Site: "https://example.invalid", SingleBucketMode: true,
		BucketsRoot: "cloudreve://my/test", Region: "us-east-1", Listen: "127.0.0.1:7070",
		SessionFile: "state/session.json", TimeoutSeconds: 60, MaxEntries: 100, MaxConcurrent: 2,
		WriteEnabled: true, MultipartEnabled: true}
}

func TestSingleBucketModeNormalization(t *testing.T) {
	for _, root := range []string{"cloudreve://my/test", "cloudreve://my/S3/test/", "cloudreve://my/S3/%74est"} {
		t.Run(root, func(t *testing.T) {
			c := singleBucketConfig()
			c.BucketsRoot = root
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if c.MultiBucket() || c.BucketsRoot != "" || c.Bucket != "test" || !c.SingleBucketMode || !c.MultipartEnabled {
				t.Fatal("switch did not resolve to the existing single-bucket backend")
			}
			want := []BucketMapping{{Name: "test", Root: c.Root}}
			if got := c.BucketMappings(); !reflect.DeepEqual(got, want) {
				t.Fatal("unexpected effective bucket mapping", got)
			}
			before := c
			if err := c.Validate(); err != nil || !reflect.DeepEqual(c, before) {
				t.Fatal("validation is not idempotent", err)
			}
		})
	}
	c := singleBucketConfig()
	c.SingleBucketMode = false
	if err := c.Validate(); err != nil || !c.MultiBucket() || c.Bucket != "" || c.Root != "" {
		t.Fatal("default switch changed directory discovery", err)
	}
}

func TestSingleBucketModeRejectsAmbiguityAndUnsafeNames(t *testing.T) {
	for _, root := range []string{"", "cloudreve://my/", "cloudreve://my/ab", "cloudreve://my/UPPER", "cloudreve://my/中文", "cloudreve://my/healthz", "cloudreve://my/a/../test", "cloudreve://my/a/%2e%2e/test", "cloudreve://my/a//test", "cloudreve://other/test"} {
		c := singleBucketConfig()
		c.BucketsRoot = root
		if c.Validate() == nil {
			t.Fatal("unsafe same-name bucket accepted", root)
		}
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Buckets = []BucketMapping{} },
		func(c *Config) { c.Buckets = []BucketMapping{{Name: "test", Root: c.BucketsRoot}} },
		func(c *Config) { c.Root = c.BucketsRoot },
		func(c *Config) { c.Bucket = "test" },
		func(c *Config) { c.BucketsRoot, c.Root, c.Bucket = "", c.BucketsRoot, "alias" },
	} {
		c := singleBucketConfig()
		change(&c)
		if c.Validate() == nil {
			t.Fatal("ambiguous single-bucket setting accepted")
		}
	}
}

func TestSingleBucketConfigCommentsAndRoundTrip(t *testing.T) {
	c := singleBucketConfig()
	c.Comments = map[string]string{"single_bucket_mode": "true: 此目录本身作为同名桶", "site": "只填写源站"}
	name := filepath.Join(t.TempDir(), "gateway.json")
	for range 2 {
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(name)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Bucket != "test" || loaded.Root != "cloudreve://my/test" || loaded.MultiBucket() || !reflect.DeepEqual(loaded.Comments, c.Comments) {
			t.Fatal("comments or canonical single-bucket round trip changed behavior")
		}
		c = loaded
	}
	valid, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct{ field, value string }{
		{"single_bucket_mode", `"true"`},
		{"single_bucket_mode", `1`},
		{"_comments", `{"site":true}`},
		{"unknown", `true`},
	} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(valid, &fields); err != nil {
			t.Fatal(err)
		}
		fields[invalid.field] = json.RawMessage(invalid.value)
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(name); err == nil {
			t.Fatal("invalid JSON setting type or unknown field accepted", invalid.field)
		}
	}
}
