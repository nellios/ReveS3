package gateway

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

const minMultipartPart = 5 << 20
const multipartLifetime = 24 * time.Hour

var multipartID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type storedPart struct {
	Number   int32     `json:"number"`
	File     string    `json:"file"`
	Size     int64     `json:"size"`
	ETag     string    `json:"etag"`
	Modified time.Time `json:"modified"`
}

type multipartRecord struct {
	ID, Site, Root, Bucket, Key, ContentType string
	Previous                                 string // Empty means the target did not exist at initiation.
	Created                                  time.Time
	State                                    string // receiving, committing, failed, completed
	Parts                                    map[int32]storedPart
	Completion                               string // Hash of the exact requested ordered part list.
	ETag                                     string // Gateway object ETag, consistent with subsequent HEAD/GET.
}

func randomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func (b *Backend) multipartDir() string       { return filepath.Join(b.Config.UploadDir, "multipart") }
func (b *Backend) recordDir(id string) string { return filepath.Join(b.multipartDir(), id) }

func (b *Backend) ValidateUploadID(id string) error {
	if !multipartID.MatchString(id) {
		return apierr(s3err.ErrNoSuchUpload)
	}
	return nil
}

func syncDirectory(path string) {
	// Windows does not support fsync on directories. File contents are always
	// synced; Linux also persists the rename before a successful response.
	if dir, err := os.Open(path); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
}

func (b *Backend) saveMultipart(record *multipartRecord) error {
	dir := b.recordDir(record.ID)
	tmp, err := os.CreateTemp(dir, ".record-")
	if err != nil {
		return unavailable("Cannot persist multipart state")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		err = json.NewEncoder(tmp).Encode(record)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, filepath.Join(dir, "record.json"))
	}
	if err != nil {
		return unavailable("Cannot persist multipart state")
	}
	syncDirectory(dir)
	return nil
}

func (b *Backend) readMultipart(id string) (*multipartRecord, error) {
	if err := b.ValidateUploadID(id); err != nil {
		return nil, err
	}
	name := filepath.Join(b.recordDir(id), "record.json")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, apierr(s3err.ErrNoSuchUpload)
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, unavailable("Invalid multipart state file")
	}
	file, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, apierr(s3err.ErrNoSuchUpload)
	}
	if err != nil {
		return nil, unavailable("Cannot read multipart state")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4<<20+1))
	var record multipartRecord
	if err != nil || len(data) > 4<<20 || json.Unmarshal(data, &record) != nil || record.ID != id ||
		record.Site != b.Config.Site || record.Root != b.Config.Root || record.Bucket != b.Config.Bucket ||
		!config.ValidKey(record.Key) || record.Created.IsZero() || len(record.Parts) > 10000 {
		return nil, unavailable("Invalid multipart state")
	}
	switch record.State {
	case "receiving", "committing", "failed", "completed":
	default:
		return nil, unavailable("Invalid multipart state")
	}
	for number, part := range record.Parts {
		if number != part.Number || number < 1 || number > 10000 || !multipartID.MatchString(part.File) || part.Size < 0 || part.Size > b.Config.MaxMultipartBytes || !validPartETag(part.ETag) {
			return nil, unavailable("Invalid multipart part record")
		}
	}
	if record.Parts == nil {
		record.Parts = map[int32]storedPart{}
	}
	return &record, nil
}

func validPartETag(tag string) bool {
	return len(tag) == 34 && tag[0] == '"' && tag[33] == '"' && multipartID.MatchString(tag[1:33])
}

// The multipart tree contains only generated hexadecimal directory/file names.
// Never follow symlinks during cleanup or while reading staged content.
func privateRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, unavailable("Invalid staged part file")
	}
	return info, nil
}

func (b *Backend) multipartRecords(recover bool) ([]*multipartRecord, error) {
	if err := b.writeDirectory(); err != nil {
		return nil, err
	}
	root := b.multipartDir()
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, unavailable("Cannot create multipart directory")
	}
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, unavailable("Invalid multipart directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) > 1000 {
		return nil, unavailable("Multipart state budget exceeded")
	}
	var records []*multipartRecord
	for _, entry := range entries {
		if !multipartID.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, unavailable("Unexpected multipart state entry")
		}
		dir := b.recordDir(entry.Name())
		r, err := b.readMultipart(entry.Name())
		if err != nil {
			if recover && errorCodeForMultipart(err) == "NoSuchUpload" {
				// A crash before the initial record was installed cannot have
				// returned an UploadId. Only remove this generated private tree.
				if err = os.RemoveAll(dir); err != nil {
					return nil, unavailable("Cannot clean multipart state")
				}
				continue
			}
			return nil, err
		}
		if recover && r.State == "committing" {
			r.State = "failed"
			if err = b.saveMultipart(r); err != nil {
				return nil, err
			}
		}
		if time.Since(r.Created) > multipartLifetime && r.State != "committing" {
			if err = os.RemoveAll(dir); err != nil {
				return nil, unavailable("Cannot expire multipart state")
			}
			continue
		}
		if recover {
			children, err := os.ReadDir(dir)
			if err != nil {
				return nil, unavailable("Cannot inspect multipart files")
			}
			referenced := map[string]bool{"record.json": true}
			if r.State != "completed" {
				for _, p := range r.Parts {
					referenced[p.File] = true
				}
			}
			for _, child := range children {
				if referenced[child.Name()] {
					if !child.Type().IsRegular() {
						return nil, unavailable("Invalid referenced multipart file")
					}
					continue
				}
				if child.Type().IsRegular() && (multipartID.MatchString(child.Name()) || strings.HasPrefix(child.Name(), ".record-")) {
					if err = os.Remove(filepath.Join(dir, child.Name())); err != nil {
						return nil, unavailable("Cannot clean orphan multipart data")
					}
				} else {
					return nil, unavailable("Unexpected multipart file")
				}
			}
		}
		records = append(records, r)
	}
	return records, nil
}

func errorCodeForMultipart(err error) string {
	var e s3err.S3Error
	if errors.As(err, &e) {
		return e.BaseError().Code
	}
	return ""
}

func (b *Backend) noMultipartTask() error {
	peers, err := b.multipartPeers()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if _, err := os.Lstat(peer.multipartDir()); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return unavailable("Cannot inspect multipart scope")
		}
		records, err := peer.multipartRecords(false)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.State != "completed" {
				return apierr(s3err.ErrSlowDown)
			}
		}
	}
	return nil
}

// Peers are derived from private state, not a remote folder snapshot, so an
// active upload in a removed/renamed folder cannot silently release the quota.
func (b *Backend) multipartPeers() ([]*Backend, error) {
	base := b.coordinator
	if base == nil || base.Config.BucketsRoot == "" {
		return []*Backend{b}, nil
	}
	entries, err := os.ReadDir(filepath.Join(base.Config.UploadDir, "directories"))
	if errors.Is(err, os.ErrNotExist) {
		return []*Backend{b}, nil
	}
	if err != nil {
		return nil, unavailable("Cannot inspect multipart buckets")
	}
	peers := []*Backend{b}
	for _, entry := range entries {
		name := strings.TrimPrefix(entry.Name(), "bucket-")
		if !strings.HasPrefix(entry.Name(), "bucket-") || !config.ValidBucketName(name) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, unavailable("Invalid multipart bucket state")
		}
		if name == b.Config.Bucket {
			continue
		}
		peer, err := newMappedBackend(base).directoryChild(name)
		if err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

func (b *Backend) multipartLock(ctx context.Context, bucket, key *string) (func(), error) {
	if !b.Config.WriteEnabled {
		return nil, apierr(s3err.ErrAccessDenied)
	}
	if !b.Config.MultipartEnabled {
		return nil, apierr(s3err.ErrNotImplemented)
	}
	if err := b.bucket(bucket); err != nil {
		return nil, err
	}
	if key != nil && !config.ValidKey(*key) {
		return nil, apierr(s3err.ErrInvalidURI)
	}
	slot, err := b.acquire()
	if err != nil {
		return nil, err
	}
	release, err := b.acquireWrite()
	if err != nil {
		slot()
		return nil, err
	}
	unlock := func() { release(); slot() }
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if _, err := b.checkRoot(ctx); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (b *Backend) matchMultipart(id, key *string) (*multipartRecord, error) {
	r, err := b.readMultipart(aws.ToString(id))
	if err != nil {
		return nil, err
	}
	if r.Key != aws.ToString(key) || time.Since(r.Created) > multipartLifetime {
		return nil, apierr(s3err.ErrNoSuchUpload)
	}
	return r, nil
}

func (b *Backend) CreateMultipartUpload(ctx context.Context, in s3response.CreateMultipartUploadInput) (s3response.InitiateMultipartUploadResult, error) {
	var out s3response.InitiateMultipartUploadResult
	release, err := b.multipartLock(ctx, in.Bucket, in.Key)
	if err != nil {
		return out, err
	}
	defer release()
	// Reuse the single-PUT feature allowlist. Multipart checksums require
	// composite checksum persistence and are explicitly unsupported for now.
	if unsupportedPut(s3response.PutObjectInput{ContentEncoding: in.ContentEncoding, ContentDisposition: in.ContentDisposition, ContentLanguage: in.ContentLanguage, CacheControl: in.CacheControl, Expires: in.Expires, Tagging: in.Tagging, ExpectedBucketOwner: in.ExpectedBucketOwner, GrantFullControl: in.GrantFullControl, GrantRead: in.GrantRead, GrantReadACP: in.GrantReadACP, GrantWriteACP: in.GrantWriteACP, SSECustomerAlgorithm: in.SSECustomerAlgorithm, SSECustomerKey: in.SSECustomerKey, SSECustomerKeyMD5: in.SSECustomerKeyMD5, SSEKMSEncryptionContext: in.SSEKMSEncryptionContext, SSEKMSKeyId: in.SSEKMSKeyId, WebsiteRedirectLocation: in.WebsiteRedirectLocation, Metadata: in.Metadata, ObjectLockMode: in.ObjectLockMode, ObjectLockLegalHoldStatus: in.ObjectLockLegalHoldStatus, ObjectLockRetainUntilDate: in.ObjectLockRetainUntilDate, StorageClass: in.StorageClass}) || in.ChecksumAlgorithm != "" || in.ChecksumType != "" || in.ServerSideEncryption != "" || aws.ToBool(in.BucketKeyEnabled) || (in.ACL != "" && in.ACL != types.ObjectCannedACLPrivate) {
		return out, apierr(s3err.ErrNotImplemented)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err = b.recoverWrites(ctx); err != nil {
		return out, mapError(err)
	}
	if err = b.noMultipartTask(); err != nil {
		return out, err
	}
	if _, err = b.multipartRecords(false); err != nil {
		return out, err
	}
	current, err := b.target(ctx, aws.ToString(in.Key), false)
	if err != nil {
		return out, err
	}
	id, err := randomID()
	if err != nil {
		return out, unavailable("Cannot create upload ID")
	}
	if err = os.Mkdir(b.recordDir(id), 0700); err != nil {
		return out, unavailable("Cannot create multipart state")
	}
	r := &multipartRecord{ID: id, Site: b.Config.Site, Root: b.Config.Root, Bucket: b.Config.Bucket, Key: aws.ToString(in.Key), ContentType: aws.ToString(in.ContentType), Created: time.Now().UTC(), State: "receiving", Parts: map[int32]storedPart{}}
	if current != nil {
		r.Previous = current.Primary
	}
	if err = b.saveMultipart(r); err != nil {
		_ = os.RemoveAll(b.recordDir(id))
		return out, err
	}
	syncDirectory(b.multipartDir())
	return s3response.InitiateMultipartUploadResult{Bucket: r.Bucket, Key: r.Key, UploadId: id}, nil
}

func (b *Backend) UploadPart(ctx context.Context, in *s3.UploadPartInput) (*s3.UploadPartOutput, error) {
	release, err := b.multipartLock(ctx, in.Bucket, in.Key)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := b.matchMultipart(in.UploadId, in.Key)
	if err != nil {
		return nil, err
	}
	if r.State != "receiving" {
		return nil, conflict()
	}
	if in.ChecksumAlgorithm != "" || anyString(in.ChecksumCRC32, in.ChecksumCRC32C, in.ChecksumSHA1, in.ChecksumSHA256, in.ChecksumCRC64NVME, in.ChecksumSHA512, in.ChecksumMD5, in.ChecksumXXHASH64, in.ChecksumXXHASH3, in.ChecksumXXHASH128, in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5) {
		return nil, apierr(s3err.ErrNotImplemented)
	}
	number, size := aws.ToInt32(in.PartNumber), aws.ToInt64(in.ContentLength)
	if in.ContentLength == nil || size < 0 || number < 1 || number > 10000 || in.Body == nil {
		return nil, apierr(s3err.ErrInvalidRequest)
	}
	var total int64
	for _, part := range r.Parts {
		total += part.Size
	}
	if size > b.Config.MaxMultipartBytes || total-r.Parts[number].Size+size > b.Config.MaxMultipartBytes {
		return nil, apierr(s3err.ErrEntityTooLarge)
	}
	// Retries keep the old part until the new durable record is installed.
	// Count both versions during staging, and count physical orphans too.
	used, err := b.stagingUsage()
	if err != nil {
		return nil, err
	}
	if size > b.Config.MaxStagingBytes-used {
		return nil, insufficientStorage()
	}
	name, err := randomID()
	if err != nil {
		return nil, unavailable("Cannot stage part")
	}
	path := filepath.Join(b.recordDir(r.ID), name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, insufficientStorage()
	}
	saved := false
	defer func() {
		if !saved {
			_ = os.Remove(path)
		}
	}()
	hash := md5.New() // S3 part ETag and optional Content-MD5, not authentication.
	_, err = io.CopyN(io.MultiWriter(file, hash), in.Body, size)
	if err == nil {
		var extra [1]byte
		n, readErr := in.Body.Read(extra[:])
		if n != 0 || readErr == nil {
			err = apierr(s3err.ErrInvalidRequest)
		} else if readErr != io.EOF {
			err = readErr
		}
	}
	if err == nil && aws.ToString(in.ContentMD5) != "" && base64.StdEncoding.EncodeToString(hash.Sum(nil)) != *in.ContentMD5 {
		err = apierr(s3err.ErrBadDigest)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		var se s3err.S3Error
		if errors.As(err, &se) {
			return nil, err
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, apierr(s3err.ErrIncompleteBody)
		}
		return nil, unavailable("Part transfer failed")
	}
	old := r.Parts[number]
	tag := `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
	r.Parts[number] = storedPart{Number: number, File: name, Size: size, ETag: tag, Modified: time.Now().UTC()}
	if err = b.saveMultipart(r); err != nil {
		return nil, err
	}
	saved = true
	if old.File != "" {
		_ = os.Remove(filepath.Join(b.recordDir(r.ID), old.File))
	}
	return &s3.UploadPartOutput{ETag: aws.String(tag)}, nil
}

func anyString(values ...*string) bool {
	for _, v := range values {
		if aws.ToString(v) != "" {
			return true
		}
	}
	return false
}

func (b *Backend) stagingUsage() (int64, error) {
	var size int64
	root := b.Config.UploadDir
	if b.coordinator != nil {
		root = b.coordinator.Config.UploadDir
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in state")
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid state file")
		}
		size += info.Size()
		return nil
	})
	if err != nil {
		return 0, unavailable("Cannot inspect staging usage")
	}
	return size, nil
}

// partSource presents selected immutable parts as a single seekable source.
// Each ReadAt opens at most one file at a time and crosses part boundaries.
type partSource struct {
	dir   string
	parts []storedPart
}

func (s *partSource) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative offset")
	}
	written := 0
	for _, part := range s.parts {
		if offset >= part.Size {
			offset -= part.Size
			continue
		}
		length := int64(len(p) - written)
		if length > part.Size-offset {
			length = part.Size - offset
		}
		file, err := os.Open(filepath.Join(s.dir, part.File))
		if err != nil {
			return written, err
		}
		n, readErr := file.ReadAt(p[written:written+int(length)], offset)
		file.Close()
		written += n
		if readErr != nil {
			return written, readErr
		}
		if written == len(p) {
			return written, nil
		}
		offset = 0
	}
	if written == len(p) {
		return written, nil
	}
	return written, io.EOF
}

func completionHash(parts []types.CompletedPart) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (b *Backend) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput) (s3response.CompleteMultipartUploadResult, string, error) {
	var out s3response.CompleteMultipartUploadResult
	release, err := b.multipartLock(ctx, in.Bucket, in.Key)
	if err != nil {
		return out, "", err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	r, err := b.matchMultipart(in.UploadId, in.Key)
	if err != nil {
		return out, "", err
	}
	if in.ChecksumType != "" || anyString(in.ChecksumCRC32, in.ChecksumCRC32C, in.ChecksumSHA1, in.ChecksumSHA256, in.ChecksumCRC64NVME, in.ChecksumSHA512, in.ChecksumMD5, in.ChecksumXXHASH64, in.ChecksumXXHASH3, in.ChecksumXXHASH128, in.SSECustomerAlgorithm, in.SSECustomerKey, in.SSECustomerKeyMD5) {
		return out, "", apierr(s3err.ErrNotImplemented)
	}
	if in.MultipartUpload == nil || len(in.MultipartUpload.Parts) == 0 || len(in.MultipartUpload.Parts) > 10000 {
		return out, "", apierr(s3err.ErrInvalidPart)
	}
	requested := in.MultipartUpload.Parts
	digest := completionHash(requested)
	out = s3response.CompleteMultipartUploadResult{Bucket: in.Bucket, Key: in.Key, ETag: aws.String(r.ETag)}
	if r.State == "completed" {
		if digest != r.Completion {
			return out, "", apierr(s3err.ErrInvalidPart)
		}
		return out, "", nil
	}
	if r.State != "receiving" {
		return out, "", conflict()
	}
	var lastNumber int32
	for _, p := range requested {
		number := aws.ToInt32(p.PartNumber)
		if number <= lastNumber {
			return out, "", apierr(s3err.ErrInvalidPartOrder)
		}
		lastNumber = number
	}
	var parts []storedPart
	var size int64
	var previous int32
	for i, p := range requested {
		if ctx.Err() != nil {
			return out, "", unavailable("Multipart completion timed out")
		}
		number := aws.ToInt32(p.PartNumber)
		if number <= previous {
			return out, "", apierr(s3err.ErrInvalidPartOrder)
		}
		previous = number
		part, ok := r.Parts[number]
		if !ok || strings.Trim(aws.ToString(p.ETag), `"`) != strings.Trim(part.ETag, `"`) {
			return out, "", apierr(s3err.ErrInvalidPart)
		}
		if anyString(p.ChecksumCRC32, p.ChecksumCRC32C, p.ChecksumSHA1, p.ChecksumSHA256, p.ChecksumCRC64NVME, p.ChecksumSHA512, p.ChecksumMD5, p.ChecksumXXHASH64, p.ChecksumXXHASH3, p.ChecksumXXHASH128) {
			return out, "", apierr(s3err.ErrNotImplemented)
		}
		if i < len(requested)-1 && part.Size < minMultipartPart {
			return out, "", apierr(s3err.ErrEntityTooSmall)
		}
		info, err := privateRegular(filepath.Join(b.recordDir(r.ID), part.File))
		if err != nil || info.Size() != part.Size {
			return out, "", unavailable("Staged part is missing or changed")
		}
		// Verify durable bytes before creating an upstream session, including
		// after process restart. Memory remains bounded by io.Copy's buffer.
		file, err := os.Open(filepath.Join(b.recordDir(r.ID), part.File))
		if err != nil {
			return out, "", unavailable("Cannot read staged part")
		}
		hash := md5.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil || `"`+hex.EncodeToString(hash.Sum(nil))+`"` != part.ETag {
			return out, "", apierr(s3err.ErrBadDigest)
		}
		size += part.Size
		parts = append(parts, part)
	}
	if size > b.Config.MaxMultipartBytes {
		return out, "", apierr(s3err.ErrEntityTooLarge)
	}
	if in.MpuObjectSize != nil && *in.MpuObjectSize != size {
		return out, "", apierr(s3err.ErrInvalidRequest)
	}
	if err = b.recoverWrites(ctx); err != nil {
		return out, "", mapError(err)
	}
	current, err := b.target(ctx, r.Key, true)
	if err != nil {
		return out, "", err
	}
	entity := ""
	if current != nil {
		entity = current.Primary
	}
	if entity != r.Previous {
		return out, "", conflict()
	}
	if err = writeCondition(current, in.IfMatch, in.IfNoneMatch); err != nil {
		return out, "", err
	}
	r.State = "committing"
	r.Completion = digest
	if err = b.saveMultipart(r); err != nil {
		return out, "", err
	}
	committed, err := b.commitUpload(ctx, r.Key, r.ContentType, size, &partSource{dir: b.recordDir(r.ID), parts: parts}, current)
	if err != nil {
		r.State = "failed"
		_ = b.saveMultipart(r)
		return out, "", err
	}
	r.State = "completed"
	r.ETag = etag(*committed)
	if err = b.saveMultipart(r); err != nil {
		return out, "", err
	}
	for _, part := range r.Parts {
		_ = os.Remove(filepath.Join(b.recordDir(r.ID), part.File))
	}
	out.ETag = aws.String(r.ETag)
	return out, "", nil
}

func (b *Backend) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput) error {
	release, err := b.multipartLock(ctx, in.Bucket, in.Key)
	if err != nil {
		return err
	}
	defer release()
	r, err := b.matchMultipart(in.UploadId, in.Key)
	if err != nil {
		return err
	}
	if r.State == "completed" {
		return apierr(s3err.ErrNoSuchUpload)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err = b.recoverWrites(ctx); err != nil {
		return mapError(err)
	}
	if err = os.RemoveAll(b.recordDir(r.ID)); err != nil {
		return unavailable("Cannot remove multipart state")
	}
	syncDirectory(b.multipartDir())
	return nil
}

func (b *Backend) ListParts(ctx context.Context, in *s3.ListPartsInput) (s3response.ListPartsResult, error) {
	var out s3response.ListPartsResult
	release, err := b.multipartLock(ctx, in.Bucket, in.Key)
	if err != nil {
		return out, err
	}
	defer release()
	r, err := b.matchMultipart(in.UploadId, in.Key)
	if err != nil {
		return out, err
	}
	if r.State == "completed" {
		return out, apierr(s3err.ErrNoSuchUpload)
	}
	marker := 0
	if aws.ToString(in.PartNumberMarker) != "" {
		marker, err = strconv.Atoi(*in.PartNumberMarker)
		if err != nil || marker < 0 {
			return out, invalidArgument("Invalid multipart pagination argument")
		}
	}
	max := int(aws.ToInt32(in.MaxParts))
	if in.MaxParts == nil {
		max = 1000
	}
	if max < 0 {
		return out, invalidArgument("Invalid multipart pagination argument")
	}
	if max > 1000 {
		max = 1000
	}
	out = s3response.ListPartsResult{Bucket: r.Bucket, Key: r.Key, UploadID: r.ID, PartNumberMarker: marker, MaxParts: max, StorageClass: types.StorageClassStandard, Owner: s3response.Owner{ID: "gateway", DisplayName: "gateway"}, Initiator: s3response.Initiator{ID: "gateway", DisplayName: "gateway"}}
	var numbers []int
	for n := range r.Parts {
		if int(n) > marker {
			numbers = append(numbers, int(n))
		}
	}
	sort.Ints(numbers)
	if len(numbers) > max {
		out.IsTruncated = true
		numbers = numbers[:max]
	}
	for _, n := range numbers {
		p := r.Parts[int32(n)]
		out.Parts = append(out.Parts, s3response.Part{PartNumber: n, ETag: p.ETag, Size: p.Size, LastModified: p.Modified})
		out.NextPartNumberMarker = n
	}
	return out, nil
}

func (b *Backend) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput) (s3response.ListMultipartUploadsResult, error) {
	var out s3response.ListMultipartUploadsResult
	release, err := b.multipartLock(ctx, in.Bucket, nil)
	if err != nil {
		return out, err
	}
	defer release()
	records, err := b.multipartRecords(false)
	if err != nil {
		return out, err
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	if prefix != "" && !config.ValidKey(strings.TrimSuffix(prefix, "/")) {
		return out, apierr(s3err.ErrInvalidURI)
	}
	if delimiter != "" && delimiter != "/" {
		return out, apierr(s3err.ErrNotImplemented)
	}
	max := int(aws.ToInt32(in.MaxUploads))
	if in.MaxUploads == nil {
		max = 1000
	}
	if max < 0 {
		return out, invalidArgument("Invalid multipart pagination argument")
	}
	if max > 1000 {
		max = 1000
	}
	out = s3response.ListMultipartUploadsResult{Bucket: b.Config.Bucket, Prefix: prefix, Delimiter: delimiter, KeyMarker: aws.ToString(in.KeyMarker), UploadIDMarker: aws.ToString(in.UploadIdMarker), MaxUploads: max}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Key == records[j].Key {
			return records[i].ID < records[j].ID
		}
		return records[i].Key < records[j].Key
	})
	count := 0
	seen := map[string]bool{}
	for _, r := range records {
		if r.State == "completed" || !strings.HasPrefix(r.Key, prefix) || r.Key < out.KeyMarker || (r.Key == out.KeyMarker && (out.UploadIDMarker == "" || r.ID <= out.UploadIDMarker)) {
			continue
		}
		cp := ""
		if delimiter != "" {
			if i := strings.Index(strings.TrimPrefix(r.Key, prefix), delimiter); i >= 0 {
				cp = prefix + strings.TrimPrefix(r.Key, prefix)[:i+1]
			}
		}
		if cp != "" && (seen[cp] || cp <= out.KeyMarker) {
			continue
		}
		if count == max {
			out.IsTruncated = true
			break
		}
		count++
		if cp != "" {
			out.CommonPrefixes = append(out.CommonPrefixes, s3response.CommonPrefix{Prefix: cp})
			seen[cp] = true
			out.NextKeyMarker = cp
			out.NextUploadIDMarker = ""
		} else {
			out.Uploads = append(out.Uploads, s3response.Upload{Key: r.Key, UploadID: r.ID, Initiated: r.Created, StorageClass: types.StorageClassStandard, Owner: s3response.Owner{ID: "gateway", DisplayName: "gateway"}, Initiator: s3response.Initiator{ID: "gateway", DisplayName: "gateway"}})
			out.NextKeyMarker = r.Key
			out.NextUploadIDMarker = r.ID
		}
	}
	if !out.IsTruncated {
		out.NextKeyMarker = ""
		out.NextUploadIDMarker = ""
	}
	return out, nil
}
