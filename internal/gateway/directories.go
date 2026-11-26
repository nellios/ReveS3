package gateway

import (
	"context"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cloudreve-s3/internal/config"
	"github.com/versity/versitygw/s3err"
)

// Directory buckets have no mutable routing cache. A valid bucket name always
// addresses exactly one immediate child of the explicitly configured parent.
// Authentication can therefore select a backend without touching upstream.
func (m *mappedBackend) directoryChild(name string) (*Backend, error) {
	if !config.ValidBucketName(name) {
		return nil, apierr(s3err.ErrNoSuchBucket)
	}
	u, err := url.Parse(m.base.Config.BucketsRoot)
	if err != nil || u == nil {
		return nil, apierr(s3err.ErrNoSuchBucket)
	}
	u.Path += "/" + name
	u.RawPath = ""
	if !config.ValidKey(strings.TrimPrefix(u.Path, "/")) {
		return nil, apierr(s3err.ErrNoSuchBucket)
	}
	child := *m.base
	child.Config.BucketsRoot = ""
	child.Config.Buckets = nil
	child.Config.Bucket = name
	child.Config.Root = u.String()
	child.Config.UploadDir = filepath.Join(m.base.Config.UploadDir, "directories", bucketStateName(name))
	child.directoryBucket = true
	return &child, nil
}

func (m *mappedBackend) directorySnapshot(ctx context.Context) ([]*Backend, error) {
	release, err := m.base.acquire()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.base.Config.TimeoutSeconds)*time.Second)
	defer cancel()
	children, err := m.discoverDirectories(ctx)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		if _, err := child.checkRoot(ctx); err != nil {
			return nil, err
		}
	}
	return children, nil
}

// The caller owns one global concurrency slot and a bounded context.
func (m *mappedBackend) discoverDirectories(ctx context.Context) ([]*Backend, error) {
	parent := *m.base
	parent.Config.Root = parent.Config.BucketsRoot
	if _, err := parent.checkRoot(ctx); err != nil {
		return nil, err
	}
	children := []*Backend{}
	seen, tokens := map[string]bool{}, map[string]bool{}
	next, count := "", 0
	for page := 0; ; page++ {
		if page > parent.Config.MaxEntries {
			return nil, unavailable("Bucket directory page budget exceeded")
		}
		listing, err := parent.Client.List(ctx, parent.Config.Root, next, page)
		if err != nil {
			return nil, mapError(err)
		}
		if listing.Parent.Type != 1 || !safeFile(listing.Parent, parent.Config.Root) {
			return nil, apierr(s3err.ErrAccessDenied)
		}
		for _, file := range listing.Files {
			count++
			if count > parent.Config.MaxEntries {
				return nil, unavailable("Bucket directory entry budget exceeded")
			}
			if seen[file.Name] {
				return nil, unavailable("Bucket directory changed during listing")
			}
			seen[file.Name] = true
			if file.Type != 1 || !config.ValidBucketName(file.Name) {
				continue
			}
			child, err := m.directoryChild(file.Name)
			if err != nil {
				return nil, err
			}
			if !safeFile(file, child.Config.Root) {
				return nil, apierr(s3err.ErrAccessDenied)
			}
			children = append(children, child)
		}
		if listing.Pagination.IsCursor {
			next = listing.Pagination.Next
			if next == "" {
				break
			}
			if tokens[next] {
				return nil, unavailable("Cloudreve repeated a bucket directory cursor")
			}
			tokens[next] = true
		} else if len(listing.Files) < 100 {
			break
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Config.Bucket < children[j].Config.Bucket })
	return children, nil
}
