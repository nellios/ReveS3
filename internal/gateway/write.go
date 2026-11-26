package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	cloud "cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

type uploadJournal struct {
	Site      string `json:"site"`
	Root      string `json:"root"`
	Key       string `json:"key"`
	SessionID string `json:"session_id"`
}

var errWriteParentMissing = errors.New("mapped parent directory is missing")

func (b *Backend) writeDirectory() error {
	if err := os.MkdirAll(b.Config.UploadDir, 0700); err != nil {
		return errors.New("cannot create private upload directory")
	}
	if err := os.Chmod(b.Config.UploadDir, 0700); err != nil {
		return errors.New("cannot secure private upload directory")
	}
	return nil
}

func (b *Backend) journalPath(key, id string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + id))
	return filepath.Join(b.Config.UploadDir, hex.EncodeToString(sum[:])+".json")
}

func saveJournal(name string, j uploadJournal) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".journal-*")
	if err != nil {
		return errors.New("cannot create upload recovery record")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		err = json.NewEncoder(tmp).Encode(j)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, name)
	}
	if err != nil {
		return errors.New("cannot persist upload recovery record")
	}
	return nil
}

func missingUpstream(err error) bool {
	var ce *cloud.Error
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Status == 404 || ce.Code == 404 || ce.Code == 40044 || ce.Code == 40016 || ce.Code == 40078 || ce.Code == 40077
}

func conflict() error {
	return s3err.APIError{Code: "OperationAborted", Description: "A conflicting Cloudreve operation is in progress", HTTPStatusCode: 409}
}

func (b *Backend) recoverJournal(ctx context.Context, name string) error {
	data, err := os.ReadFile(name)
	if err != nil {
		return errors.New("cannot read upload recovery record")
	}
	var j uploadJournal
	if json.Unmarshal(data, &j) != nil || j.Site != b.Config.Site || j.Root != b.Config.Root || j.SessionID == "" || !config.ValidKey(j.Key) {
		return errors.New("invalid upload recovery record")
	}
	if err = b.ensureParents(ctx, j.Key, false); err != nil {
		if errors.Is(err, errWriteParentMissing) {
			return conflict()
		}
		return err
	}
	file, err := b.Client.Info(ctx, b.uri(j.Key))
	if err != nil {
		if missingUpstream(err) {
			return conflict()
		}
		return mapError(err)
	}
	if !safeFile(file, b.uri(j.Key)) || file.Type != 0 {
		return apierr(s3err.ErrAccessDenied)
	}
	if file.Metadata["sys:upload_session_id"] == "" && file.Committed() {
		// The final chunk may have committed just before the process stopped.
		return os.Remove(name)
	}
	if file.Metadata["sys:upload_session_id"] != j.SessionID {
		return conflict()
	}
	if err = b.Client.CancelUpload(ctx, b.uri(j.Key), j.SessionID); err != nil {
		return mapError(err)
	}
	return os.Remove(name)
}

func (b *Backend) recoverWrites(ctx context.Context) error {
	if !b.Config.WriteEnabled {
		return nil
	}
	if err := b.writeDirectory(); err != nil {
		return err
	}
	entries, err := os.ReadDir(b.Config.UploadDir)
	if err != nil {
		return errors.New("cannot inspect upload recovery records")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".body-") || strings.HasPrefix(entry.Name(), ".journal-") {
			info, statErr := entry.Info()
			if statErr != nil {
				return errors.New("cannot inspect staged upload file")
			}
			if info.Mode().IsRegular() {
				if err := os.Remove(filepath.Join(b.Config.UploadDir, entry.Name())); err != nil {
					return errors.New("cannot clean interrupted staged upload")
				}
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := b.recoverJournal(ctx, filepath.Join(b.Config.UploadDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// RecoverWrites removes only upload sessions named by durable local records.
// Unknown, replaced, or inaccessible sessions fail closed for manual review.
func (b *Backend) RecoverWrites(ctx context.Context) error {
	if b.Config.BucketsRoot != "" {
		children, err := newMappedBackend(b).directorySnapshot(ctx)
		if err != nil {
			return err
		}
		if err := b.checkUploadLayout(children...); err != nil {
			return err
		}
		for _, child := range children {
			if err := child.RecoverWrites(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	if err := b.checkUploadLayout(); err != nil {
		return err
	}
	if b.Config.Buckets != nil {
		for _, child := range newMappedBackend(b).ordered {
			if err := child.RecoverWrites(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := b.recoverWrites(ctx); err != nil {
		return err
	}
	if b.Config.WriteEnabled && b.Config.MultipartEnabled {
		_, err := b.multipartRecords(true)
		return err
	}
	return nil
}

func (b *Backend) acquireWrite() (func(), error) {
	select {
	case b.writes <- struct{}{}:
		return func() { <-b.writes }, nil
	default:
		return nil, apierr(s3err.ErrSlowDown)
	}
}

func (b *Backend) ensureParents(ctx context.Context, key string, create bool) error {
	if _, err := b.checkRoot(ctx); err != nil {
		return err
	}
	parts := strings.Split(key, "/")
	for i := 1; i < len(parts); i++ {
		parentKey := strings.Join(parts[:i], "/")
		uri := b.uri(parentKey)
		file, err := b.Client.Info(ctx, uri)
		if err != nil && missingUpstream(err) {
			if !create {
				return errWriteParentMissing
			}
			// Re-read after either a successful create or conflict and validate
			// the resulting object rather than trusting the create response.
			_ = b.Client.CreateFolder(ctx, uri)
			file, err = b.Client.Info(ctx, uri)
			if err != nil {
				return mapError(err)
			}
		} else if err != nil {
			return mapError(err)
		}
		if !safeFile(file, uri) || file.Type != 1 {
			return apierr(s3err.ErrAccessDenied)
		}
	}
	return nil
}

func (b *Backend) target(ctx context.Context, key string, createParents bool) (*cloud.File, error) {
	if err := b.ensureParents(ctx, key, createParents); err != nil {
		if errors.Is(err, errWriteParentMissing) {
			return nil, nil
		}
		return nil, err
	}
	uri := b.uri(key)
	f, err := b.Client.Info(ctx, uri)
	if err != nil {
		if missingUpstream(err) {
			return nil, nil
		}
		return nil, mapError(err)
	}
	if !safeFile(f, uri) {
		return nil, apierr(s3err.ErrAccessDenied)
	}
	if f.Type != 0 {
		return nil, conflict()
	}
	if f.Metadata["sys:upload_session_id"] != "" || !f.Committed() || f.Updated.IsZero() {
		return nil, conflict()
	}
	return &f, nil
}

func writeCondition(current *cloud.File, match, none *string) error {
	if match != nil && *match != "" {
		if current == nil || !matchTags(*match, etag(*current), false) {
			return apierr(s3err.ErrPreconditionFailed)
		}
	}
	if none != nil && *none != "" && current != nil && matchTags(*none, etag(*current), false) {
		return apierr(s3err.ErrPreconditionFailed)
	}
	return nil
}

func (b *Backend) spool(body io.Reader, size int64) (string, error) {
	if err := b.writeDirectory(); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(b.Config.UploadDir, ".body-*")
	if err != nil {
		return "", errors.New("cannot stage upload")
	}
	name := f.Name()
	if err = f.Chmod(0600); err != nil {
		f.Close()
		os.Remove(name)
		return "", errors.New("cannot secure staged upload")
	}
	if _, err = io.CopyN(f, body, size); err == nil {
		var extra [1]byte
		var n int
		n, err = body.Read(extra[:])
		if n != 0 {
			err = apierr(s3err.ErrInvalidRequest)
		} else if err == io.EOF {
			err = nil
		} else if err == nil {
			err = apierr(s3err.ErrInvalidRequest)
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(name)
		var upstreamErr s3err.S3Error
		if errors.As(err, &upstreamErr) {
			return "", err
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", apierr(s3err.ErrIncompleteBody)
		}
		return "", apierr(s3err.ErrInvalidRequest)
	}
	return name, nil
}

func unsupportedPut(in s3response.PutObjectInput) bool {
	return aws.ToString(in.ContentEncoding) != "" || aws.ToString(in.ContentDisposition) != "" ||
		aws.ToString(in.ContentLanguage) != "" || aws.ToString(in.CacheControl) != "" ||
		aws.ToString(in.Expires) != "" || aws.ToString(in.Tagging) != "" ||
		aws.ToString(in.ExpectedBucketOwner) != "" || aws.ToString(in.GrantFullControl) != "" ||
		aws.ToString(in.GrantRead) != "" || aws.ToString(in.GrantReadACP) != "" ||
		aws.ToString(in.GrantWriteACP) != "" || aws.ToString(in.SSECustomerAlgorithm) != "" ||
		aws.ToString(in.SSECustomerKey) != "" || aws.ToString(in.SSECustomerKeyMD5) != "" ||
		aws.ToString(in.SSEKMSEncryptionContext) != "" || aws.ToString(in.SSEKMSKeyId) != "" ||
		aws.ToString(in.WebsiteRedirectLocation) != "" || len(in.Metadata) != 0 ||
		in.ObjectLockMode != "" || in.ObjectLockLegalHoldStatus != "" ||
		(in.ObjectLockRetainUntilDate != nil && !in.ObjectLockRetainUntilDate.IsZero()) ||
		(in.StorageClass != "" && in.StorageClass != "STANDARD")
}

func (b *Backend) cancelAfterFailure(name, uri, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.Client.CancelUpload(ctx, uri, id); err == nil {
		_ = os.Remove(name)
	}
}

func (b *Backend) PutObject(ctx context.Context, in s3response.PutObjectInput) (s3response.PutObjectOutput, error) {
	var out s3response.PutObjectOutput
	if !b.Config.WriteEnabled {
		return out, apierr(s3err.ErrAccessDenied)
	}
	if err := b.bucket(in.Bucket); err != nil {
		return out, err
	}
	key := aws.ToString(in.Key)
	if !config.ValidKey(key) {
		return out, apierr(s3err.ErrInvalidURI)
	}
	if unsupportedPut(in) {
		return out, apierr(s3err.ErrNotImplemented)
	}
	if in.ContentLength == nil || *in.ContentLength < 0 {
		return out, apierr(s3err.ErrInvalidRequest)
	}
	if *in.ContentLength > b.Config.MaxUploadBytes {
		return out, apierr(s3err.ErrEntityTooLarge)
	}
	release, err := b.acquire()
	if err != nil {
		return out, err
	}
	defer release()
	releaseWrite, err := b.acquireWrite()
	if err != nil {
		return out, err
	}
	defer releaseWrite()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err = b.recoverWrites(ctx); err != nil {
		return out, mapError(err)
	}
	if err = b.noMultipartTask(); err != nil {
		return out, err
	}
	if in.Body == nil {
		return out, apierr(s3err.ErrInvalidRequest)
	}
	spool, err := b.spool(in.Body, *in.ContentLength)
	if err != nil {
		return out, err
	}
	defer os.Remove(spool)
	current, err := b.target(ctx, key, true)
	if err != nil {
		return out, err
	}
	if err = writeCondition(current, in.IfMatch, in.IfNoneMatch); err != nil {
		return out, err
	}
	source, err := os.Open(spool)
	if err != nil {
		return out, unavailable("Could not read staged upload")
	}
	defer source.Close()
	committed, err := b.commitUpload(ctx, key, aws.ToString(in.ContentType), *in.ContentLength, source, current)
	if err != nil {
		return out, err
	}
	out.ETag = etag(*committed)
	out.Size = aws.Int64(committed.Size)
	out.ChecksumCRC32 = in.ChecksumCRC32
	out.ChecksumCRC32C = in.ChecksumCRC32C
	out.ChecksumSHA1 = in.ChecksumSHA1
	out.ChecksumSHA256 = in.ChecksumSHA256
	out.ChecksumCRC64NVME = in.ChecksumCRC64NVME
	out.ChecksumSHA512 = in.ChecksumSHA512
	out.ChecksumMD5 = in.ChecksumMD5
	out.ChecksumXXHASH64 = in.ChecksumXXHASH64
	out.ChecksumXXHASH3 = in.ChecksumXXHASH3
	out.ChecksumXXHASH128 = in.ChecksumXXHASH128
	return out, nil
}

// commitUpload transfers an already validated, bounded source. The caller holds
// the write lock and verifies the target before starting this operation.
func (b *Backend) commitUpload(ctx context.Context, key, mimeType string, size int64, source io.ReaderAt, current *cloud.File) (*cloud.File, error) {
	previous := ""
	if current != nil {
		previous = current.Primary
	}
	uri := b.uri(key)
	session, err := b.Client.StartUpload(ctx, uri, size, mimeType, previous)
	if err != nil {
		return nil, mapError(err)
	}
	if session.ID == "" || session.URI != uri {
		return nil, unavailable("Cloudreve returned an unsupported upload session")
	}
	journal := uploadJournal{Site: b.Config.Site, Root: b.Config.Root, Key: key, SessionID: session.ID}
	journalName := b.journalPath(key, session.ID)
	if err = saveJournal(journalName, journal); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = b.Client.CancelUpload(cancelCtx, uri, session.ID)
		cancel()
		return nil, unavailable("Could not record upload recovery state")
	}
	if session.StoragePolicy.Type != "local" || session.ChunkSize < 0 {
		b.cancelAfterFailure(journalName, uri, session.ID)
		return nil, apierr(s3err.ErrNotImplemented)
	}
	chunkSize := session.ChunkSize
	if chunkSize == 0 {
		chunkSize = size
	}
	if chunkSize <= 0 {
		chunkSize = 1
	}
	chunks := (size + chunkSize - 1) / chunkSize
	if chunks == 0 {
		chunks = 1
	}
	for index := int64(0); index < chunks; index++ {
		offset := index * chunkSize
		length := size - offset
		if length < 0 {
			length = 0
		}
		if length > chunkSize {
			length = chunkSize
		}
		reader := io.NewSectionReader(source, offset, length)
		err = b.Client.UploadChunk(ctx, session.ID, index, reader, length)
		if err != nil {
			b.cancelAfterFailure(journalName, uri, session.ID)
			return nil, mapError(err)
		}
	}
	committed, err := b.Client.Info(ctx, uri)
	if err != nil || !safeFile(committed, uri) || committed.Type != 0 ||
		committed.Size != size || committed.Metadata["sys:upload_session_id"] != "" || !committed.Committed() {
		return nil, unavailable("Cloudreve did not confirm the completed upload")
	}
	_ = os.Remove(journalName)
	return &committed, nil
}

func (b *Backend) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if !b.Config.WriteEnabled {
		return nil, apierr(s3err.ErrAccessDenied)
	}
	if err := b.bucket(in.Bucket); err != nil {
		return nil, err
	}
	if aws.ToString(in.VersionId) != "" {
		return nil, apierr(s3err.ErrNotImplemented)
	}
	key := aws.ToString(in.Key)
	if !config.ValidKey(key) {
		return nil, apierr(s3err.ErrInvalidURI)
	}
	release, err := b.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	releaseWrite, err := b.acquireWrite()
	if err != nil {
		return nil, err
	}
	defer releaseWrite()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err = b.recoverWrites(ctx); err != nil {
		return nil, mapError(err)
	}
	if err = b.noMultipartTask(); err != nil {
		return nil, err
	}
	current, err := b.target(ctx, key, false)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return &s3.DeleteObjectOutput{}, nil
	}
	if in.IfMatch != nil && *in.IfMatch != "" && !matchTags(*in.IfMatch, etag(*current), false) {
		return nil, apierr(s3err.ErrPreconditionFailed)
	}
	if in.IfMatchSize != nil && *in.IfMatchSize != current.Size {
		return nil, apierr(s3err.ErrPreconditionFailed)
	}
	if in.IfMatchLastModifiedTime != nil && current.Updated.UTC().Truncate(time.Second) != in.IfMatchLastModifiedTime.UTC().Truncate(time.Second) {
		return nil, apierr(s3err.ErrPreconditionFailed)
	}
	if err = b.Client.DeleteFile(ctx, b.uri(key)); err != nil {
		return nil, mapError(err)
	}
	return &s3.DeleteObjectOutput{}, nil
}

func (b *Backend) GetBucketVersioning(context.Context, string) (s3response.GetBucketVersioningOutput, error) {
	return s3response.GetBucketVersioningOutput{}, apierr(s3err.ErrNotImplemented)
}
