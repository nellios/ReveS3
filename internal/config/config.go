package config

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Config struct {
	// Comments explains settings in strict JSON; it has no effect on runtime behavior.
	Comments          map[string]string `json:"_comments,omitempty"`
	Listen            string            `json:"listen"`
	AllowRemoteListen bool              `json:"allow_remote_listen"`
	Site              string            `json:"site"`
	AllowHTTP         bool              `json:"allow_http"`
	Root              string            `json:"root"`
	Bucket            string            `json:"bucket"`
	Buckets           []BucketMapping   `json:"buckets,omitempty"`
	BucketsRoot       string            `json:"buckets_root,omitempty"`
	// SingleBucketMode exposes BucketsRoot itself under its final directory name.
	SingleBucketMode  bool   `json:"single_bucket_mode,omitempty"`
	Region            string `json:"region"`
	SessionFile       string `json:"session_file"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	MaxEntries        int    `json:"max_entries"`
	MaxConcurrent     int    `json:"max_concurrent"`
	WriteEnabled      bool   `json:"write_enabled"`
	MaxUploadBytes    int64  `json:"max_upload_bytes"`
	UploadDir         string `json:"upload_dir"`
	MultipartEnabled  bool   `json:"multipart_enabled"`
	MaxMultipartBytes int64  `json:"max_multipart_bytes"`
	MaxStagingBytes   int64  `json:"max_staging_bytes"`
}

type BucketMapping struct {
	Name string `json:"name"`
	Root string `json:"root"`
}

// BucketMappings returns a copy so callers cannot mutate the configuration.
// The original single-bucket format keeps its state paths and behaviour.
func (c Config) BucketMappings() []BucketMapping {
	if c.BucketsRoot != "" {
		return nil // Directory discovery requires an authenticated upstream request.
	}
	if c.Buckets != nil {
		return append([]BucketMapping(nil), c.Buckets...)
	}
	return []BucketMapping{{Name: c.Bucket, Root: c.Root}}
}

func (c Config) MultiBucket() bool { return c.Buckets != nil || c.BucketsRoot != "" }

func Load(name string) (Config, error) {
	c := Config{Listen: "127.0.0.1:7070", Region: "us-east-1", SessionFile: ".state/session.json", TimeoutSeconds: 60, MaxEntries: 10000, MaxConcurrent: 16}
	f, err := os.Open(name)
	if err != nil {
		return c, errors.New("cannot open gateway configuration")
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid gateway configuration JSON")
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.MaxMultipartBytes == 0 {
		c.MaxMultipartBytes = 5 << 30
	}
	if c.MaxStagingBytes == 0 {
		c.MaxStagingBytes = 6 << 30
	}
	if c.MaxMultipartBytes < 1 || c.MaxMultipartBytes > 5<<30 || c.MaxStagingBytes < c.MaxMultipartBytes || c.MaxStagingBytes > 6<<30 {
		return errors.New("multipart limits require 0 < object <= staging, at most 5 GiB per object and 6 GiB staging")
	}
	if c.MaxUploadBytes == 0 {
		c.MaxUploadBytes = 32 << 20
	}
	if c.MaxUploadBytes < 1 || c.MaxUploadBytes > 32<<20 {
		return errors.New("max_upload_bytes must be between 1 and 33554432")
	}
	if c.UploadDir == "" {
		c.UploadDir = filepath.Join(filepath.Dir(c.SessionFile), "uploads")
	}
	u, e := url.Parse(c.Site)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http")) {
		return errors.New("site must be an HTTPS origin; HTTP requires allow_http")
	}
	c.Site = strings.TrimSuffix(c.Site, "/")
	if err := c.normalizeSingleBucket(); err != nil {
		return err
	}
	if c.BucketsRoot != "" {
		if c.Buckets != nil || c.Root != "" || c.Bucket != "" {
			return errors.New("buckets_root cannot be combined with bucket/root or explicit buckets")
		}
		root, err := validateRoot(c.BucketsRoot)
		if err != nil {
			return err
		}
		c.BucketsRoot = root
	} else if c.Buckets != nil {
		if c.Root != "" || c.Bucket != "" || len(c.Buckets) == 0 || len(c.Buckets) > 100 {
			return errors.New("use either bucket/root or 1-100 explicit buckets, not both")
		}
		if c.MultipartEnabled {
			return errors.New("multipart is not yet supported with multiple bucket mappings")
		}
		// Validate a copy: a failed validation must not partially normalize the caller's slice.
		mappings := c.BucketMappings()
		names := make(map[string]bool, len(mappings))
		for i := range mappings {
			if e := validateMapping(&mappings[i]); e != nil {
				return e
			}
			if names[mappings[i].Name] {
				return errors.New("bucket names must be unique")
			}
			names[mappings[i].Name] = true
			current, _ := url.Parse(mappings[i].Root)
			for _, previous := range mappings[:i] {
				other, _ := url.Parse(previous.Root)
				if current.Path == other.Path || strings.HasPrefix(current.Path, other.Path+"/") || strings.HasPrefix(other.Path, current.Path+"/") {
					return errors.New("bucket roots must not duplicate or overlap")
				}
			}
		}
		c.Buckets = mappings
	} else {
		mapping := BucketMapping{Name: c.Bucket, Root: c.Root}
		if e := validateMapping(&mapping); e != nil {
			return e
		}
		c.Root = mapping.Root
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(c.Region) {
		return errors.New("invalid region")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be host:port")
	}
	ip := net.ParseIP(host)
	if !c.AllowRemoteListen && (ip == nil || !ip.IsLoopback()) {
		return errors.New("remote listen requires allow_remote_listen")
	}
	if c.SessionFile == "" || c.TimeoutSeconds < 1 || c.TimeoutSeconds > 3600 || c.MaxEntries < 1 || c.MaxEntries > 100000 || c.MaxConcurrent < 1 || c.MaxConcurrent > 256 {
		return errors.New("invalid resource limits or session_file")
	}
	return nil
}

// Resolve the convenience switch into the existing single-bucket layout.
// Keep normalization idempotent, including after a normalized config is saved.
func (c *Config) normalizeSingleBucket() error {
	if !c.SingleBucketMode {
		return nil
	}
	if c.Buckets != nil {
		return errors.New("single_bucket_mode cannot be combined with explicit buckets")
	}
	root := c.BucketsRoot
	if root != "" {
		if c.Root != "" || c.Bucket != "" {
			return errors.New("single_bucket_mode uses buckets_root alone; remove bucket/root")
		}
	} else {
		// Also accept the canonical bucket/root form produced by normalization.
		if c.Root == "" || c.Bucket == "" {
			return errors.New("single_bucket_mode requires buckets_root pointing to the bucket directory")
		}
		root = c.Root
	}
	root, err := validateRoot(root)
	if err != nil {
		return err
	}
	u, _ := url.Parse(root)
	name := path.Base(u.Path)
	if !ValidBucketName(name) {
		return errors.New("single_bucket_mode requires a final directory name of 3-63 lowercase letters, digits or hyphens, excluding healthz")
	}
	if c.Bucket != "" && c.Bucket != name {
		return errors.New("single_bucket_mode bucket must match the final directory name")
	}
	c.Bucket, c.Root, c.BucketsRoot = name, root, ""
	return nil
}

func validateMapping(mapping *BucketMapping) error {
	root, e := validateRoot(mapping.Root)
	if e != nil {
		return e
	}
	mapping.Root = root
	if !ValidBucketName(mapping.Name) {
		return errors.New("bucket must contain 3-63 lowercase letters, digits or hyphens and cannot be healthz")
	}
	return nil
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// ValidBucketName is the gateway's existing, deliberately restricted subset.
func ValidBucketName(name string) bool {
	return name != "healthz" && bucketName.MatchString(name)
}

func validateRoot(root string) (string, error) {
	u, e := url.Parse(root)
	if e != nil || u.Scheme != "cloudreve" || u.Host != "my" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/") {
		return "", errors.New("root must be cloudreve://my/<explicit directory>")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	if !ValidKey(strings.TrimPrefix(u.Path, "/")) {
		return "", errors.New("account root and unsafe root paths are forbidden")
	}
	return u.String(), nil
}

// Keys are decoded once by the S3 layer. Never clean or unescape them again.
func ValidKey(key string) bool {
	if key == "" || !utf8.ValidString(key) || len(key) > 1024 || strings.ContainsAny(key, "\\\x00\r\n") {
		return false
	}
	for _, r := range key {
		if r < 32 || r == 127 || r == 0xfffe || r == 0xffff {
			return false
		}
	}
	for _, s := range strings.Split(key, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}
