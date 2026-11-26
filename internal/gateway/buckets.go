package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

// mappedBackend dispatches only the operations already supported by the
// gateway. All children share the account client and global concurrency locks.
// Unsupported operations (including bucket creation/deletion)
// retain BackendUnsupported's explicit errors in multi-bucket mode.
type mappedBackend struct {
	backend.BackendUnsupported
	base    *Backend
	byName  map[string]*Backend
	ordered []*Backend
}

func newMappedBackend(base *Backend) *mappedBackend {
	m := &mappedBackend{base: base, byName: make(map[string]*Backend)}
	for _, mapping := range base.Config.BucketMappings() {
		child := base
		if base.Config.Buckets != nil {
			// Config was validated before construction; these names are path-safe.
			copy := *base
			copy.Config.Buckets = nil
			copy.Config.Bucket = mapping.Name
			copy.Config.Root = mapping.Root
			copy.Config.UploadDir = filepath.Join(base.Config.UploadDir, "buckets", bucketStateName(mapping.Name))
			child = &copy
		}
		m.byName[mapping.Name] = child
		m.ordered = append(m.ordered, child)
	}
	sort.Slice(m.ordered, func(i, j int) bool { return m.ordered[i].Config.Bucket < m.ordered[j].Config.Bucket })
	return m
}

func (*mappedBackend) String() string { return "Cloudreve" }

func bucketStateName(name string) string {
	// Valid S3 names such as con, aux and lpt1 are reserved Windows filenames.
	return "bucket-" + name
}

func (m *mappedBackend) selectBucket(name *string) (*Backend, error) {
	if m.base.Config.BucketsRoot != "" {
		return m.directoryChild(aws.ToString(name))
	}
	b := m.byName[aws.ToString(name)]
	if b == nil {
		return nil, apierr(s3err.ErrNoSuchBucket)
	}
	return b, nil
}

// CheckBuckets verifies every configured directory without creating anything.
func (b *Backend) CheckBuckets(ctx context.Context) error {
	if b.Config.BucketsRoot != "" {
		_, err := newMappedBackend(b).directorySnapshot(ctx)
		return err
	}
	for _, child := range newMappedBackend(b).ordered {
		if _, err := child.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(child.Config.Bucket)}); err != nil {
			return err
		}
	}
	return nil
}

func (m *mappedBackend) HeadBucket(ctx context.Context, in *s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.HeadBucket(ctx, in)
}

func (m *mappedBackend) GetBucketAcl(ctx context.Context, in *s3.GetBucketAclInput) ([]byte, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.GetBucketAcl(ctx, in)
}

func (m *mappedBackend) GetObjectLockConfiguration(ctx context.Context, name string) ([]byte, error) {
	b, err := m.selectBucket(&name)
	if err != nil {
		return nil, err
	}
	return b.GetObjectLockConfiguration(ctx, name)
}

func (m *mappedBackend) HeadObject(ctx context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.HeadObject(ctx, in)
}

func (m *mappedBackend) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.GetObject(ctx, in)
}

func (m *mappedBackend) ListObjects(ctx context.Context, in *s3.ListObjectsInput) (s3response.ListObjectsResult, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.ListObjectsResult{}, err
	}
	return b.ListObjects(ctx, in)
}

func (m *mappedBackend) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (s3response.ListObjectsV2Result, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.ListObjectsV2Result{}, err
	}
	return b.ListObjectsV2(ctx, in)
}

func (m *mappedBackend) PutObject(ctx context.Context, in s3response.PutObjectInput) (s3response.PutObjectOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.PutObjectOutput{}, err
	}
	return b.PutObject(ctx, in)
}

func (m *mappedBackend) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.DeleteObject(ctx, in)
}

func (m *mappedBackend) CreateMultipartUpload(ctx context.Context, in s3response.CreateMultipartUploadInput) (s3response.InitiateMultipartUploadResult, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.InitiateMultipartUploadResult{}, err
	}
	return b.CreateMultipartUpload(ctx, in)
}

func (m *mappedBackend) UploadPart(ctx context.Context, in *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	return b.UploadPart(ctx, in)
}

func (m *mappedBackend) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput) (s3response.CompleteMultipartUploadResult, string, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.CompleteMultipartUploadResult{}, "", err
	}
	return b.CompleteMultipartUpload(ctx, in)
}

func (m *mappedBackend) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput) error {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return err
	}
	return b.AbortMultipartUpload(ctx, in)
}

func (m *mappedBackend) ListParts(ctx context.Context, in *s3.ListPartsInput) (s3response.ListPartsResult, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.ListPartsResult{}, err
	}
	return b.ListParts(ctx, in)
}

func (m *mappedBackend) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput) (s3response.ListMultipartUploadsResult, error) {
	b, err := m.selectBucket(in.Bucket)
	if err != nil {
		return s3response.ListMultipartUploadsResult{}, err
	}
	return b.ListMultipartUploads(ctx, in)
}

func (m *mappedBackend) listingCursor(prefix string) cursor {
	mappings := m.base.Config.BucketMappings()
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Name < mappings[j].Name })
	scope, _ := json.Marshal(struct {
		Site, Region    string
		DirectoriesRoot string `json:",omitempty"`
		Mappings        []config.BucketMapping
	}{m.base.Config.Site, m.base.Config.Region, m.base.Config.BucketsRoot, mappings})
	sum := sha256.Sum256(scope)
	return cursor{Root: hex.EncodeToString(sum[:]), Bucket: "\x00list-buckets", Prefix: prefix}
}

func (m *mappedBackend) ListBuckets(ctx context.Context, in s3response.ListBucketsInput) (s3response.ListAllMyBucketsResult, error) {
	res := s3response.ListAllMyBucketsResult{Owner: s3response.CanonicalUser{ID: "gateway", DisplayName: "gateway"}, Prefix: in.Prefix}
	max := in.MaxBuckets
	if max == 0 {
		max = 10000
	}
	if max < 1 || max > 10000 {
		return res, invalidArgument("max-buckets must be between 1 and 10000")
	}
	c := m.listingCursor(in.Prefix)
	after := ""
	if in.ContinuationToken != "" {
		var err error
		after, err = m.base.decodeCursor(in.ContinuationToken, c)
		if err != nil {
			return res, err
		}
	}
	release, err := m.base.acquire()
	if err != nil {
		return res, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.base.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	children := m.ordered
	if m.base.Config.BucketsRoot != "" {
		children, err = m.discoverDirectories(ctx)
		if err != nil {
			return res, err
		}
	}
	for _, child := range children {
		name := child.Config.Bucket
		if !strings.HasPrefix(name, in.Prefix) || name <= after {
			continue
		}
		if len(res.Buckets.Bucket) == int(max) {
			c.Last = res.Buckets.Bucket[len(res.Buckets.Bucket)-1].Name
			res.ContinuationToken = m.base.encodeCursor(c)
			break
		}
		f, err := child.checkRoot(ctx)
		if err != nil {
			return s3response.ListAllMyBucketsResult{}, err
		}
		res.Buckets.Bucket = append(res.Buckets.Bucket, s3response.ListAllMyBucketsEntry{Name: name, BucketRegion: child.Config.Region, CreationDate: f.Created})
	}
	return res, nil
}

func stateHasFiles(root string) (bool, error) {
	found := errors.New("state present")
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return found
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, found) {
		return true, nil
	}
	return false, err
}

// Never silently skip recovery state when changing config format or removing a
// mapping. Empty directories left after completed uploads are harmless.
func (b *Backend) checkUploadLayout(discovered ...*Backend) error {
	if !b.Config.WriteEnabled {
		return nil
	}
	entries, err := os.ReadDir(b.Config.UploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect upload state layout")
	}
	layout := "buckets"
	known := make(map[string]bool)
	for _, mapping := range b.Config.BucketMappings() {
		known[bucketStateName(mapping.Name)] = true
	}
	if b.Config.BucketsRoot != "" {
		layout = "directories"
		for _, child := range discovered {
			known[bucketStateName(child.Config.Bucket)] = true
		}
	}
	for _, entry := range entries {
		if !b.Config.MultiBucket() && entry.Name() != "buckets" && entry.Name() != "directories" {
			continue
		}
		root := filepath.Join(b.Config.UploadDir, entry.Name())
		if b.Config.MultiBucket() && entry.Name() == layout && entry.IsDir() {
			children, err := os.ReadDir(root)
			if err != nil {
				return errors.New("cannot inspect bucket upload state")
			}
			for _, child := range children {
				if known[child.Name()] && child.IsDir() {
					continue
				}
				present, err := stateHasFiles(filepath.Join(root, child.Name()))
				if err != nil || present {
					return errors.New("unconfigured bucket has upload state; restore its mapping to recover")
				}
			}
			continue
		}
		present, err := stateHasFiles(root)
		if err != nil || present {
			return errors.New("upload state belongs to a different mapping layout; restore the previous configuration to recover")
		}
	}
	return nil
}
