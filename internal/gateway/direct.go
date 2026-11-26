package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"cloudreve-s3/internal/config"
	"github.com/versity/versitygw/s3err"
)

// DirectUploadResult does not contain local paths, upstream URLs or credentials.
type DirectUploadResult struct {
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
	Verification string `json:"verification"`
}

// unchangedFile reads the original file without staging a second copy. Stat
// checks detect ordinary edits, but are not an atomic filesystem snapshot.
type unchangedFile struct {
	file *os.File
	info os.FileInfo
	ctx  context.Context
}

func (s *unchangedFile) check() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	info, err := s.file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != s.info.Size() || !info.ModTime().Equal(s.info.ModTime()) {
		return errors.New("source file changed; do not modify it during upload")
	}
	return nil
}

func (s *unchangedFile) ReadAt(p []byte, offset int64) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	n, err := s.file.ReadAt(p, offset)
	if changed := s.check(); changed != nil {
		return n, changed
	}
	return n, err
}

// UploadLocalFile sends a local file through Cloudreve's native API. No S3
// request, local listener or staged payload is involved. The caller must use a
// private state/session directory separate from any running gateway process.
func (base *Backend) UploadLocalFile(ctx context.Context, bucket, key, filename string, overwrite, verify bool) (DirectUploadResult, error) {
	var result DirectUploadResult
	if !base.Config.WriteEnabled {
		return result, apierr(s3err.ErrAccessDenied)
	}
	if !config.ValidKey(key) {
		return result, apierr(s3err.ErrInvalidURI)
	}
	b, err := newMappedBackend(base).selectBucket(&bucket)
	if err != nil {
		return result, err
	}
	// Reject directories and special files before opening (e.g. named pipes).
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return result, errors.New("source must be a readable regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return result, errors.New("cannot open source file")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return result, errors.New("source must be a readable regular file")
	}
	if info.Size() > b.Config.MaxMultipartBytes {
		return result, apierr(s3err.ErrEntityTooLarge)
	}
	release, err := b.acquire()
	if err != nil {
		return result, err
	}
	defer release()
	releaseWrite, err := b.acquireWrite()
	if err != nil {
		return result, err
	}
	defer releaseWrite()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err = b.recoverWrites(ctx); err != nil {
		return result, err
	}
	if err = b.noMultipartTask(); err != nil {
		return result, err
	}
	current, err := b.target(ctx, key, true)
	if err != nil {
		return result, err
	}
	if current != nil && !overwrite {
		return result, apierr(s3err.ErrPreconditionFailed)
	}
	source := &unchangedFile{file: file, info: info, ctx: ctx}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.NewSectionReader(source, 0, info.Size())); err != nil {
		return result, errors.New("cannot hash stable source file")
	}
	if err = source.check(); err != nil {
		return result, err
	}
	committed, err := b.commitUpload(ctx, key, contentType(key), info.Size(), source, current)
	if err != nil {
		return result, err
	}
	// After the last chunk the remote object can already exist. Never claim
	// rollback if the source changes at this late stage or verification fails.
	if err = source.check(); err != nil {
		return result, errors.New("source changed; remote upload may already have completed, inspect the object")
	}
	result = DirectUploadResult{Bytes: committed.Size, SHA256: hex.EncodeToString(hash.Sum(nil)), Verification: "metadata"}
	if !verify {
		return result, nil
	}
	resp, err := b.Client.Open(ctx, b.uri(key), committed.Primary, "")
	if err != nil {
		return result, errors.New("upload completed but verification download failed; inspect the object")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != info.Size() || resp.Header.Get("Content-Encoding") != "" {
		return result, errors.New("upload completed but verification response was invalid; inspect the object")
	}
	downloaded := sha256.New()
	size, err := io.Copy(downloaded, io.LimitReader(resp.Body, info.Size()+1))
	if err != nil || size != info.Size() || hex.EncodeToString(downloaded.Sum(nil)) != result.SHA256 {
		return result, errors.New("upload completed but SHA-256 verification failed; inspect the object")
	}
	result.Verification = "sha256"
	return result, nil
}
