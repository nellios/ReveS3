package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/versity/versitygw/backend"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

type Backend struct {
	backend.BackendUnsupported
	Config config.Config
	Client *cloudreve.Client
	secret []byte
	slots  chan struct{}
	writes chan struct{}
	// directoryBucket distinguishes a discovered bucket from a fixed mapping.
	directoryBucket bool
	coordinator     *Backend // Shared account-wide multipart task and staging scope.
}

func New(c config.Config, client *cloudreve.Client, secret string) *Backend {
	if c.MaxMultipartBytes == 0 {
		c.MaxMultipartBytes = 5 << 30
	}
	if c.MaxStagingBytes == 0 {
		c.MaxStagingBytes = 6 << 30
	}
	b := &Backend{Config: c, Client: client, secret: []byte(secret), slots: make(chan struct{}, c.MaxConcurrent), writes: make(chan struct{}, 1)}
	b.coordinator = b
	return b
}
func (b *Backend) acquire() (func(), error) {
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, nil
	default:
		return nil, apierr(s3err.ErrSlowDown)
	}
}
func (*Backend) String() string         { return "Cloudreve" }
func apierr(code s3err.ErrorCode) error { return s3err.GetAPIError(code) }
func unavailable(message string) error {
	return s3err.APIError{Code: "ServiceUnavailable", Description: message, HTTPStatusCode: 503}
}
func mapError(e error) error {
	if e == nil {
		return nil
	}
	var se s3err.S3Error
	if errors.As(e, &se) {
		return e
	}
	if errors.Is(e, cloudreve.ErrLogin) {
		return unavailable("Cloudreve session requires login")
	}
	var ce *cloudreve.Error
	if errors.As(e, &ce) {
		switch {
		case ce.Code == 40004 || ce.Code == 40076:
			return conflict()
		case ce.Status == 404 || ce.Code == 404 || ce.Code == 40044 || ce.Code == 40016 || ce.Code == 40078 || ce.Code == 40077:
			return apierr(s3err.ErrNoSuchKey)
		case ce.Status == 403 || ce.Code == 403 || ce.Code == 40007 || ce.Code == 40017 || ce.Code == 40018 || ce.Code == 40089:
			return apierr(s3err.ErrAccessDenied)
		case ce.Status == 429:
			return apierr(s3err.ErrSlowDown)
		}
	}
	return unavailable("Cloudreve request failed")
}
func (b *Backend) bucket(bucket *string) error {
	if aws.ToString(bucket) != b.Config.Bucket {
		return apierr(s3err.ErrNoSuchBucket)
	}
	return nil
}

// Object Lock is not exposed by this gateway. The S3 protocol middleware
// treats this specific response as an unconfigured bucket and permits ordinary
// object operations to reach our backend.
func (b *Backend) GetObjectLockConfiguration(_ context.Context, bucket string) ([]byte, error) {
	if bucket != b.Config.Bucket {
		return nil, apierr(s3err.ErrNoSuchBucket)
	}
	return nil, apierr(s3err.ErrObjectLockConfigurationNotFound)
}

// ACL metadata is local so unsigned/invalid requests cannot trigger Cloudreve calls.
func (b *Backend) GetBucketAcl(_ context.Context, in *s3.GetBucketAclInput) ([]byte, error) {
	if e := b.bucket(in.Bucket); e != nil {
		return nil, e
	}
	return []byte(`{}`), nil
}
func (b *Backend) uri(key string) string {
	u, _ := url.Parse(b.Config.Root)
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = ""
	return u.String()
}
func sameURI(a, b string) bool {
	u, e := url.Parse(a)
	if e != nil {
		return false
	}
	v, e := url.Parse(b)
	return e == nil && u.Scheme == v.Scheme && u.Host == v.Host && u.Path == v.Path && u.RawQuery == "" && u.Fragment == ""
}
func safeFile(f cloudreve.File, uri string) bool {
	return sameURI(f.Path, uri) && f.Metadata["sys:shared_redirect"] == "" && f.Metadata["sys:restore_uri"] == ""
}

func (b *Backend) checkRoot(ctx context.Context) (cloudreve.File, error) {
	// Check each configured ancestor: Cloudreve supports symbolic shared folders.
	u, _ := url.Parse(b.Config.Root)
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	var f cloudreve.File
	for i := range segments {
		v := url.URL{Scheme: "cloudreve", Host: "my", Path: "/" + strings.Join(segments[:i+1], "/")}
		uri := v.String()
		var e error
		f, e = b.Client.Info(ctx, uri)
		if e != nil {
			if b.directoryBucket && missingUpstream(e) {
				return f, apierr(s3err.ErrNoSuchBucket)
			}
			return f, mapError(e)
		}
		if b.directoryBucket && i == len(segments)-1 && f.Type != 1 && safeFile(f, uri) {
			return f, apierr(s3err.ErrNoSuchBucket)
		}
		if f.Type != 1 || !safeFile(f, uri) {
			return f, apierr(s3err.ErrAccessDenied)
		}
	}
	return f, nil
}
func (b *Backend) HeadBucket(ctx context.Context, in *s3.HeadBucketInput) (*s3.HeadBucketOutput, error) {
	release, e := b.acquire()
	if e != nil {
		return nil, e
	}
	defer release()
	if e := b.bucket(in.Bucket); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	_, e = b.checkRoot(ctx)
	if e != nil {
		return nil, e
	}
	return &s3.HeadBucketOutput{BucketRegion: aws.String(b.Config.Region)}, nil
}
func (b *Backend) ListBuckets(ctx context.Context, in s3response.ListBucketsInput) (s3response.ListAllMyBucketsResult, error) {
	return newMappedBackend(b).ListBuckets(ctx, in)
}
func (b *Backend) object(ctx context.Context, bucket, key *string) (cloudreve.File, error) {
	var f cloudreve.File
	if e := b.bucket(bucket); e != nil {
		return f, e
	}
	k := aws.ToString(key)
	if !config.ValidKey(k) {
		return f, apierr(s3err.ErrInvalidURI)
	}
	if _, e := b.checkRoot(ctx); e != nil {
		return f, e
	}
	parts := strings.Split(k, "/")
	for i := range parts {
		uri := b.uri(strings.Join(parts[:i+1], "/"))
		var e error
		f, e = b.Client.Info(ctx, uri)
		if e != nil {
			return f, mapError(e)
		}
		if !safeFile(f, uri) {
			return f, apierr(s3err.ErrAccessDenied)
		}
		if i < len(parts)-1 && f.Type != 1 {
			return f, apierr(s3err.ErrNoSuchKey)
		}
	}
	if f.Type != 0 || f.Size < 0 || f.Updated.IsZero() || !f.Committed() {
		return f, apierr(s3err.ErrNoSuchKey)
	}
	return f, nil
}
func etag(f cloudreve.File) string {
	sum := sha256.Sum256([]byte(f.ID + "\x00" + f.Primary + "\x00" + strconv.FormatInt(f.Size, 10)))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
func contentType(key string) string {
	t := mime.TypeByExtension(path.Ext(key))
	if t == "" {
		return "application/octet-stream"
	}
	return t
}
func matchTags(value, tag string, weak bool) bool {
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		if v == "*" {
			return true
		}
		if weak {
			v = strings.TrimPrefix(v, "W/")
		}
		// Versity removes the outer quotes of the complete header before
		// passing it to a backend; normalize each item to support lists too.
		if !strings.HasPrefix(v, "W/") && strings.Trim(v, `"`) == strings.Trim(tag, `"`) {
			return true
		}
	}
	return false
}
func condition(f cloudreve.File, match, none *string, modified, unmodified *time.Time) error {
	t := f.Updated.UTC().Truncate(time.Second)
	tag := etag(f)
	if aws.ToString(match) != "" {
		if !matchTags(*match, tag, false) {
			return apierr(s3err.ErrPreconditionFailed)
		}
	} else if unmodified != nil && t.After(*unmodified) {
		return apierr(s3err.ErrPreconditionFailed)
	}
	if aws.ToString(none) != "" {
		if matchTags(*none, tag, true) {
			return apierr(s3err.ErrNotModified)
		}
	} else if modified != nil && !t.After(*modified) {
		return apierr(s3err.ErrNotModified)
	}
	return nil
}

func byteRange(value string, size int64) (start, length int64, cr string, err error) {
	if value == "" {
		return 0, size, "", nil
	}
	err = apierr(s3err.ErrInvalidRange)
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return
	}
	p := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(p) != 2 {
		return
	}
	parse := func(s string) (int64, error) {
		if s == "" {
			return 0, errors.New("empty")
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0, errors.New("invalid")
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	end := size - 1
	if p[0] == "" {
		n, e := parse(p[1])
		if e != nil || n <= 0 {
			return
		}
		if n > size {
			n = size
		}
		start = size - n
	} else {
		var e error
		start, e = parse(p[0])
		if e != nil || start >= size {
			return
		}
		if p[1] != "" {
			end, e = parse(p[1])
			if e != nil || end < start {
				return
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	length = end - start + 1
	cr = fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	err = nil
	return
}
func unsupported(version *string, part *int32, sse *string) bool {
	return aws.ToString(version) != "" || part != nil || aws.ToString(sse) != ""
}
func (b *Backend) HeadObject(ctx context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	release, e := b.acquire()
	if e != nil {
		return nil, e
	}
	defer release()
	if unsupported(in.VersionId, in.PartNumber, in.SSECustomerAlgorithm) {
		return nil, apierr(s3err.ErrNotImplemented)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	f, e := b.object(ctx, in.Bucket, in.Key)
	if e != nil {
		return nil, e
	}
	res := &s3.HeadObjectOutput{ContentLength: aws.Int64(f.Size), ETag: aws.String(etag(f)), LastModified: &f.Updated, ContentType: aws.String(contentType(aws.ToString(in.Key))), AcceptRanges: aws.String("bytes")}
	if e = condition(f, in.IfMatch, in.IfNoneMatch, in.IfModifiedSince, in.IfUnmodifiedSince); e != nil {
		return res, e
	}
	_, n, _, e := byteRange(aws.ToString(in.Range), f.Size)
	if e != nil {
		return nil, e
	}
	res.ContentLength = &n
	return res, nil
}

type stream struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (s *stream) Close() error {
	var e error
	s.once.Do(func() { e = s.ReadCloser.Close(); s.cancel() })
	return e
}
func (b *Backend) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	release, e := b.acquire()
	if e != nil {
		return nil, e
	}
	if unsupported(in.VersionId, in.PartNumber, in.SSECustomerAlgorithm) {
		release()
		return nil, apierr(s3err.ErrNotImplemented)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	success := false
	defer func() {
		if !success {
			cancel()
			release()
		}
	}()
	f, e := b.object(ctx, in.Bucket, in.Key)
	if e != nil {
		return nil, e
	}
	res := &s3.GetObjectOutput{ContentLength: aws.Int64(f.Size), ETag: aws.String(etag(f)), LastModified: &f.Updated, ContentType: aws.String(contentType(aws.ToString(in.Key))), AcceptRanges: aws.String("bytes")}
	if e = condition(f, in.IfMatch, in.IfNoneMatch, in.IfModifiedSince, in.IfUnmodifiedSince); e != nil {
		return res, e
	}
	start, n, cr, e := byteRange(aws.ToString(in.Range), f.Size)
	if e != nil {
		return nil, e
	}
	requested := ""
	if cr != "" {
		requested = fmt.Sprintf("bytes=%d-%d", start, start+n-1)
	}
	resp, e := b.Client.Open(ctx, b.uri(aws.ToString(in.Key)), f.Primary, requested)
	if e != nil {
		return nil, mapError(e)
	}
	expected := http.StatusOK
	if cr != "" {
		expected = http.StatusPartialContent
	}
	if resp.StatusCode != expected || resp.ContentLength != n || (cr != "" && resp.Header.Get("Content-Range") != cr) || resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close()
		return nil, unavailable("Cloudreve download metadata mismatch")
	}
	res.ContentLength = &n
	if cr != "" {
		res.ContentRange = &cr
	}
	res.Body = &stream{ReadCloser: resp.Body, cancel: func() { cancel(); release() }}
	success = true
	return res, nil
}

type entry struct {
	key       string
	file      *cloudreve.File
	directory bool
}
type cursor struct{ Root, Bucket, Prefix, Delimiter, Last string }

func (b *Backend) encodeCursor(c cursor) string {
	data, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, b.secret)
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(append(mac.Sum(nil), data...))
}
func (b *Backend) decodeCursor(token string, c cursor) (string, error) {
	if len(token) > 8192 {
		return "", apierr(s3err.ErrInvalidToken)
	}
	data, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(data) < 33 {
		return "", apierr(s3err.ErrInvalidToken)
	}
	mac := hmac.New(sha256.New, b.secret)
	mac.Write(data[32:])
	var got cursor
	if !hmac.Equal(data[:32], mac.Sum(nil)) || json.Unmarshal(data[32:], &got) != nil {
		return "", apierr(s3err.ErrInvalidToken)
	}
	c.Last = got.Last
	if c != got {
		return "", apierr(s3err.ErrInvalidToken)
	}
	return got.Last, nil
}

// ListObjects uses the same isolated traversal as V2, with a plain key marker
// and owner metadata as required by the older API used by WinSCP.
func (b *Backend) ListObjects(ctx context.Context, in *s3.ListObjectsInput) (s3response.ListObjectsResult, error) {
	page, err := b.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: in.Bucket, Prefix: in.Prefix, Delimiter: in.Delimiter,
		MaxKeys: in.MaxKeys, StartAfter: in.Marker, FetchOwner: aws.Bool(true),
	})
	if err != nil {
		return s3response.ListObjectsResult{}, err
	}
	result := s3response.ListObjectsResult{
		Name: page.Name, Prefix: page.Prefix, Delimiter: page.Delimiter,
		Marker: in.Marker, MaxKeys: page.MaxKeys, IsTruncated: page.IsTruncated,
		Contents: page.Contents, CommonPrefixes: page.CommonPrefixes,
	}
	if aws.ToBool(page.IsTruncated) && aws.ToString(in.Delimiter) != "" {
		// Prefixes count towards MaxKeys too, and can be the last item of a
		// page with no objects. Return the last key across both collections.
		last := ""
		for _, object := range page.Contents {
			if key := aws.ToString(object.Key); key > last {
				last = key
			}
		}
		for _, prefix := range page.CommonPrefixes {
			if key := aws.ToString(prefix.Prefix); key > last {
				last = key
			}
		}
		result.NextMarker = aws.String(last)
	}
	return result, nil
}

func (b *Backend) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (s3response.ListObjectsV2Result, error) {
	var res s3response.ListObjectsV2Result
	release, e := b.acquire()
	if e != nil {
		return res, e
	}
	defer release()
	if e := b.bucket(in.Bucket); e != nil {
		return res, e
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	if prefix != "" && !config.ValidKey(strings.TrimSuffix(prefix, "/")) {
		return res, apierr(s3err.ErrInvalidURI)
	}
	if delimiter != "" && delimiter != "/" {
		return res, apierr(s3err.ErrNotImplemented)
	}
	max := int32(1000)
	if in.MaxKeys != nil {
		max = *in.MaxKeys
	}
	if max < 0 {
		return res, s3err.APIError{Code: "InvalidArgument", Description: "max-keys must be nonnegative", HTTPStatusCode: 400}
	}
	if max > 1000 {
		max = 1000
	}
	c := cursor{Root: b.Config.Root, Bucket: b.Config.Bucket, Prefix: prefix, Delimiter: delimiter}
	after := aws.ToString(in.StartAfter)
	if token := aws.ToString(in.ContinuationToken); token != "" {
		var e error
		after, e = b.decodeCursor(token, c)
		if e != nil {
			return res, e
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if _, e := b.checkRoot(ctx); e != nil {
		return res, e
	}
	res = s3response.ListObjectsV2Result{Name: in.Bucket, Prefix: in.Prefix, Delimiter: in.Delimiter, StartAfter: in.StartAfter, ContinuationToken: in.ContinuationToken, MaxKeys: &max, IsTruncated: aws.Bool(false), KeyCount: aws.Int32(0)}
	if max == 0 {
		return res, nil
	}
	entries := map[string]entry{}
	seen := map[string]bool{}
	count := 0
	pages := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > 64 {
			return unavailable("Directory traversal depth exceeded")
		}
		next := ""
		tokens := map[string]bool{}
		for page := 0; ; page++ {
			pages++
			if pages > b.Config.MaxEntries+1 {
				return unavailable("Directory traversal page budget exceeded")
			}
			uri := b.uri(dir)
			l, e := b.Client.List(ctx, uri, next, page)
			if e != nil {
				return mapError(e)
			}
			if !safeFile(l.Parent, uri) {
				return apierr(s3err.ErrAccessDenied)
			}
			for _, f := range l.Files {
				count++
				if count > b.Config.MaxEntries {
					return unavailable("Directory traversal entry budget exceeded")
				}
				if !config.ValidKey(f.Name) || strings.Contains(f.Name, "/") {
					return unavailable("Unsupported Cloudreve filename")
				}
				key := f.Name
				if dir != "" {
					key = dir + "/" + key
				}
				if !config.ValidKey(key) {
					return unavailable("Cloudreve path cannot be represented by an S3 key")
				}
				if seen[key] {
					return unavailable("Cloudreve directory changed during listing")
				}
				seen[key] = true
				if !safeFile(f, b.uri(key)) {
					return apierr(s3err.ErrAccessDenied)
				}
				if f.Type == 1 {
					folder := key + "/"
					if delimiter == "/" && strings.HasPrefix(folder, prefix) {
						if i := strings.Index(strings.TrimPrefix(folder, prefix), "/"); i >= 0 {
							cp := prefix + strings.TrimPrefix(folder, prefix)[:i+1]
							entries[cp] = entry{key: cp, directory: true}
							continue
						}
					}
					if strings.HasPrefix(folder, prefix) || strings.HasPrefix(prefix, folder) {
						if e := walk(key, depth+1); e != nil {
							return e
						}
					}
					continue
				}
				if f.Type != 0 || !strings.HasPrefix(key, prefix) {
					continue
				}
				if f.Metadata["sys:upload_session_id"] != "" {
					f, e = b.Client.Info(ctx, b.uri(key))
					if e != nil {
						return mapError(e)
					}
					if !safeFile(f, b.uri(key)) {
						return apierr(s3err.ErrAccessDenied)
					}
					if !f.Committed() {
						continue
					}
				}
				if f.Size < 0 || f.Primary == "" || f.Updated.IsZero() {
					return unavailable("Invalid Cloudreve file metadata")
				}
				if delimiter != "" {
					if i := strings.Index(strings.TrimPrefix(key, prefix), delimiter); i >= 0 {
						cp := prefix + strings.TrimPrefix(key, prefix)[:i+1]
						entries[cp] = entry{key: cp, directory: true}
						continue
					}
				}
				copy := f
				entries[key] = entry{key: key, file: &copy}
			}
			if l.Pagination.IsCursor {
				next = l.Pagination.Next
				if next == "" {
					break
				}
				if tokens[next] {
					return unavailable("Cloudreve repeated a directory cursor")
				}
				tokens[next] = true
			} else if len(l.Files) < 100 {
				break
			}
		}
		return nil
	}
	if e := walk("", 0); e != nil {
		return res, e
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		if key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > int(max) {
		res.IsTruncated = aws.Bool(true)
		keys = keys[:max]
		c.Last = keys[len(keys)-1]
		res.NextContinuationToken = aws.String(b.encodeCursor(c))
	}
	for _, key := range keys {
		en := entries[key]
		if en.directory {
			res.CommonPrefixes = append(res.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(key)})
			continue
		}
		f := *en.file
		obj := s3response.Object{Key: aws.String(key), Size: &f.Size, LastModified: &f.Updated, ETag: aws.String(etag(f)), StorageClass: types.ObjectStorageClassStandard}
		if aws.ToBool(in.FetchOwner) {
			obj.Owner = &types.Owner{ID: aws.String("gateway"), DisplayName: aws.String("gateway")}
		}
		res.Contents = append(res.Contents, obj)
	}
	res.KeyCount = aws.Int32(int32(len(keys)))
	return res, nil
}
