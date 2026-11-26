package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/versity/versitygw/s3api"
	"github.com/versity/versitygw/s3api/utils"
	"github.com/versity/versitygw/s3err"
	"github.com/versity/versitygw/s3response"
)

const testAccess = "gateway-test"
const testSecret = "test-only-secret-012345678901234567890123456789"

type fakeCloud struct {
	files       map[string]cloudreve.File
	content     map[string][]byte
	uploads     map[string]*fakeUpload
	entitySeq   atomic.Int32
	calls       atomic.Int32
	downloads   atomic.Int32
	server      *httptest.Server
	truncate    atomic.Bool
	ignoreRange atomic.Bool
	failChunk   atomic.Bool
}

type fakeUpload struct {
	key         string
	size        int64
	data        []byte
	previous    cloudreve.File
	hadPrevious bool
}

func newFake(t *testing.T) *fakeCloud {
	t.Helper()
	f := &fakeCloud{files: map[string]cloudreve.File{}, content: map[string][]byte{}, uploads: map[string]*fakeUpload{}}
	for _, key := range []string{"", "dir", "empty"} {
		f.add(key, nil, true)
	}
	for _, key := range []string{"a.bin", "中文 空格.txt", "dir/nested.txt", "literal%2Fname.txt", "special +?#%.txt", "old-upload.bin", "new-upload.bin"} {
		f.add(key, []byte("content:"+key+":0123456789"), false)
	}
	f.add("zero", []byte{}, false)
	for _, key := range []string{"old-upload.bin", "new-upload.bin"} {
		v := f.files[key]
		v.Metadata["sys:upload_session_id"] = "pending"
		if key == "new-upload.bin" {
			v.Primary = "uncommitted-zero"
			v.Size = 0
		}
		f.files[key] = v
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fakeCloud) add(key string, body []byte, dir bool) {
	u := url.URL{Scheme: "cloudreve", Host: "my", Path: "/root"}
	if key != "" {
		u.Path += "/" + key
	}
	entity := fmt.Sprintf("entity:%s:%d", key, f.entitySeq.Add(1))
	v := cloudreve.File{ID: "id:" + key, Name: key[strings.LastIndex(key, "/")+1:], Path: u.String(), Size: int64(len(body)), Updated: time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC), Created: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), Primary: entity, Metadata: map[string]string{}}
	if dir {
		v.Type = 1
	} else {
		v.Extended.Entities = []cloudreve.Entity{{ID: v.Primary, Type: 0, Size: v.Size}}
		f.content[v.Primary] = body
	}
	f.files[key] = v
}
func (f *fakeCloud) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/download" {
		f.downloads.Add(1)
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "leaked authorization", 500)
			return
		}
		body, ok := f.content[r.URL.Query().Get("entity")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if f.truncate.Load() {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(200)
			w.Write(body[:len(body)/2])
			return
		}
		if f.ignoreRange.Load() {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Write(body)
			return
		}
		http.ServeContent(w, r, "object", time.Time{}, bytes.NewReader(body))
		return
	}
	f.calls.Add(1)
	respond := func(code int, data any) { json.NewEncoder(w).Encode(map[string]any{"code": code, "data": data}) }
	if r.URL.Path == "/api/v4/session/token" {
		respond(0, map[string]any{"token": cloudreve.Session{Access: "test-token", Refresh: "test-refresh", AccessExpires: time.Now().Add(time.Hour)}})
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		respond(401, nil)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v4/file/upload/") && r.Method == http.MethodPost {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v4/file/upload/"), "/")
		if len(parts) != 2 {
			respond(40001, nil)
			return
		}
		session := f.uploads[parts[0]]
		if session == nil {
			respond(40044, nil)
			return
		}
		if f.failChunk.Load() {
			respond(50001, nil)
			return
		}
		chunk, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			respond(40001, nil)
			return
		}
		session.data = append(session.data, chunk...)
		if int64(len(session.data)) == session.size {
			f.add(session.key, append([]byte(nil), session.data...), false)
			delete(f.uploads, parts[0])
		}
		respond(0, map[string]any{})
		return
	}
	if r.URL.Path == "/api/v4/file/create" && r.Method == http.MethodPost {
		var in struct {
			URI  string `json:"uri"`
			Type string `json:"type"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Type != "folder" {
			respond(40001, nil)
			return
		}
		created, _ := url.Parse(in.URI)
		createdKey := strings.TrimPrefix(strings.TrimPrefix(created.Path, "/root"), "/")
		if _, exists := f.files[createdKey]; exists {
			respond(40004, nil)
			return
		}
		f.add(createdKey, nil, true)
		respond(0, f.files[createdKey])
		return
	}
	if r.URL.Path == "/api/v4/file/upload" && r.Method == http.MethodPut {
		var in struct {
			URI        string `json:"uri"`
			Size       int64  `json:"size"`
			EntityType string `json:"entity_type"`
			Previous   string `json:"previous"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			respond(40001, nil)
			return
		}
		targetURI, _ := url.Parse(in.URI)
		target := strings.TrimPrefix(strings.TrimPrefix(targetURI.Path, "/root"), "/")
		old, exists := f.files[target]
		if exists && (old.Type != 0 || old.Metadata["sys:upload_session_id"] != "" || in.EntityType != "version" || in.Previous != old.Primary) {
			respond(40076, nil)
			return
		}
		id := "session-" + strconv.Itoa(len(f.uploads)+1)
		session := &fakeUpload{key: target, size: in.Size, previous: old, hadPrevious: exists}
		f.uploads[id] = session
		pending := old
		if !exists {
			f.add(target, nil, false)
			pending = f.files[target]
		}
		pending.Metadata = map[string]string{}
		for name, value := range old.Metadata {
			pending.Metadata[name] = value
		}
		pending.Metadata["sys:upload_session_id"] = id
		f.files[target] = pending
		respond(0, map[string]any{"session_id": id, "uri": in.URI, "chunk_size": 1 << 20, "storage_policy": map[string]any{"type": "local"}})
		return
	}
	if r.URL.Path == "/api/v4/file/upload" && r.Method == http.MethodDelete {
		var in struct {
			ID  string `json:"id"`
			URI string `json:"uri"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		session := f.uploads[in.ID]
		if session == nil {
			respond(40044, nil)
			return
		}
		if session.hadPrevious {
			f.files[session.key] = session.previous
		} else {
			delete(f.files, session.key)
		}
		delete(f.uploads, in.ID)
		respond(0, map[string]any{})
		return
	}
	if r.URL.Path == "/api/v4/file" && r.Method == http.MethodDelete {
		var in struct {
			URIs []string `json:"uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, uri := range in.URIs {
			parsed, _ := url.Parse(uri)
			deleted := strings.TrimPrefix(strings.TrimPrefix(parsed.Path, "/root"), "/")
			if file, exists := f.files[deleted]; exists && file.Type == 0 {
				delete(f.content, file.Primary)
				delete(f.files, deleted)
			}
		}
		respond(0, map[string]any{})
		return
	}
	u, _ := url.Parse(r.URL.Query().Get("uri"))
	key := strings.TrimPrefix(u.Path, "/root")
	key = strings.TrimPrefix(key, "/")
	if r.URL.Query().Get("uri") != "" && !(u.Path == "/root" || strings.HasPrefix(u.Path, "/root/")) {
		respond(403, nil)
		return
	}
	switch r.URL.Path {
	case "/api/v4/file/info":
		v, ok := f.files[key]
		if !ok {
			respond(40044, nil)
			return
		}
		respond(0, v)
	case "/api/v4/file":
		keys := []string{}
		prefix := ""
		if key != "" {
			prefix = key + "/"
		}
		for k := range f.files {
			if k != key && strings.HasPrefix(k, prefix) && !strings.Contains(strings.TrimPrefix(k, prefix), "/") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		offset, _ := strconv.Atoi(r.URL.Query().Get("next_page_token"))
		if offset > len(keys) {
			respond(40001, nil)
			return
		}
		end := offset + 2
		if end > len(keys) {
			end = len(keys)
		}
		files := []cloudreve.File{}
		for _, k := range keys[offset:end] {
			files = append(files, f.files[k])
		}
		next := ""
		if end < len(keys) {
			next = strconv.Itoa(end)
		}
		respond(0, map[string]any{"files": files, "parent": f.files[key], "pagination": map[string]any{"is_cursor": true, "next_token": next}})
	case "/api/v4/file/url":
		var in struct {
			Entity string `json:"entity"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		respond(0, map[string]any{"urls": []map[string]string{{"url": f.server.URL + "/download?entity=" + url.QueryEscape(in.Entity)}}})
	default:
		respond(404, nil)
	}
}

func testGateway(t *testing.T) (*fakeCloud, *Backend, *s3.Client, string) {
	return testGatewayMode(t, false, 32<<20)
}

func testGatewayMode(t *testing.T, writes bool, maxUpload int64) (*fakeCloud, *Backend, *s3.Client, string) {
	t.Helper()
	fake := newFake(t)
	c := config.Config{Site: fake.server.URL, AllowHTTP: true, Root: "cloudreve://my/root", Bucket: "cloudreve", Region: "us-east-1", Listen: "127.0.0.1:0", SessionFile: "", TimeoutSeconds: 10, MaxEntries: 100, MaxConcurrent: 8, WriteEnabled: writes, MaxUploadBytes: maxUpload, UploadDir: t.TempDir()}
	client := cloudreve.New(c.Site, "", time.Second*10)
	if e := client.Login(context.Background(), "test", "test"); e != nil {
		t.Fatal(e)
	}
	be := New(c, client, testSecret)
	addresses := make(chan []net.Addr, 1)
	server, e := NewServer(be, testAccess, testSecret, s3api.WithOnListenAddrs(func(a []net.Addr) { addresses <- a }))
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeMultiPort([]string{c.Listen}) }()
	var endpoint string
	select {
	case a := <-addresses:
		endpoint = "http://" + a[0].String()
	case e := <-done:
		t.Fatalf("server failed: %v", e)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}
	t.Cleanup(func() {
		server.ShutDown()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	sdk := s3.New(s3.Options{Region: c.Region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(testAccess, testSecret, ""), RetryMaxAttempts: 1})
	return fake, be, sdk, endpoint
}
func errorCode(e error) string {
	var api smithy.APIError
	if errors.As(e, &api) {
		return api.ErrorCode()
	}
	if e != nil {
		return "other"
	}
	return ""
}

func TestCompletedClientsReleaseConnectionSlots(t *testing.T) {
	_, _, sdk, endpoint := testGateway(t)
	address := strings.TrimPrefix(endpoint, "http://")
	// Keep more client handles open than the server's 16 connection slots.
	// Each completed response must close its socket rather than leave the
	// connection idle indefinitely and deny the next client (including health).
	for i := 0; i < 20; i++ {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err = fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", address); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || !response.Close {
			t.Fatalf("client %d: status=%d close=%v read=%v", i, response.StatusCode, response.Close, readErr)
		}
		if _, err = reader.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("client %d: completed socket not closed: %v", i, err)
		}
	}
	if _, err := sdk.HeadBucket(context.Background(), &s3.HeadBucketInput{Bucket: aws.String("cloudreve")}); err != nil {
		t.Fatalf("signed request after completed clients: %v", err)
	}
}

func TestWinSCPLoginQueryRemainsSigned(t *testing.T) {
	fake, _, _, endpoint := testGateway(t)
	get := func(query, secret, changedQuery string) (int, []byte) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, endpoint+"/"+query, nil)
		sum := sha256.Sum256(nil)
		hash := hex.EncodeToString(sum[:])
		request.Header.Set("X-Amz-Content-Sha256", hash)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: testAccess, SecretAccessKey: secret}, request, hash, "s3", "us-east-1", time.Now()); err != nil {
			t.Fatal(err)
		}
		if changedQuery != "" {
			request.URL.RawQuery = changedQuery
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	for _, query := range []string{"?max-keys=1", "?max-keys=1000"} {
		status, body := get(query, testSecret, "")
		var response s3response.ListAllMyBucketsResult
		if err := xml.Unmarshal(body, &response); err != nil || status != 200 || len(response.Buckets.Bucket) != 1 || response.Buckets.Bucket[0].Name != "cloudreve" {
			t.Fatalf("WinSCP login: status=%d parse=%v", status, err)
		}
	}
	before := fake.calls.Load()
	for _, tc := range []struct {
		query, secret, changed string
		status                 int
	}{
		{"?max-keys=0", testSecret, "", 400},
		{"?max-keys=-1", testSecret, "", 400},
		{"?max-keys=invalid", testSecret, "", 400},
		{"?max-keys=2147483648", testSecret, "", 400},
		{"?max-keys=1&unsupported=other", testSecret, "", 501},
		{"?max-keys=1", "wrong-secret", "", 403},
		{"?max-keys=1", testSecret, "max-keys=2", 403},
	} {
		if status, _ := get(tc.query, tc.secret, tc.changed); status != tc.status {
			t.Fatalf("query %s: status=%d expected=%d", tc.query, status, tc.status)
		}
	}
	if fake.calls.Load() != before {
		t.Fatal("rejected login query reached Cloudreve")
	}
}

func TestListObjectsV1Markers(t *testing.T) {
	_, _, sdk, _ := testGateway(t)
	ctx := context.Background()
	bucket := aws.String("cloudreve")
	for _, delimiter := range []string{"", "/"} {
		var marker *string
		var actual []string
		pages := 0
		for {
			page, err := sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, Delimiter: &delimiter, MaxKeys: aws.Int32(1), Marker: marker})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if pages > 20 || aws.ToString(page.Marker) != aws.ToString(marker) || len(page.Contents)+len(page.CommonPrefixes) > 1 {
				t.Fatal("invalid V1 pagination")
			}
			last := ""
			for _, object := range page.Contents {
				last = aws.ToString(object.Key)
				if object.Owner == nil || aws.ToString(object.Owner.ID) != "gateway" {
					t.Fatal("V1 object owner missing")
				}
				actual = append(actual, last)
			}
			for _, prefix := range page.CommonPrefixes {
				last = aws.ToString(prefix.Prefix)
				actual = append(actual, last)
			}
			if !aws.ToBool(page.IsTruncated) {
				break
			}
			if last <= aws.ToString(marker) {
				t.Fatal("V1 marker did not advance")
			}
			if delimiter == "" {
				if page.NextMarker != nil {
					t.Fatal("V1 without delimiter must omit NextMarker")
				}
				marker = aws.String(last)
			} else {
				if aws.ToString(page.NextMarker) != last {
					t.Fatal("V1 delimiter page must return the last key or prefix")
				}
				marker = page.NextMarker
			}
		}
		reference, err := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, Delimiter: &delimiter})
		if err != nil {
			t.Fatal(err)
		}
		var expected []string
		for _, object := range reference.Contents {
			expected = append(expected, aws.ToString(object.Key))
		}
		for _, prefix := range reference.CommonPrefixes {
			expected = append(expected, aws.ToString(prefix.Prefix))
		}
		sort.Strings(expected)
		if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
			t.Fatalf("V1 omitted or repeated an item: %v != %v", actual, expected)
		}
	}
	page, err := sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, Prefix: aws.String("dir/ne")})
	if err != nil || len(page.Contents) != 1 || aws.ToString(page.Contents[0].Key) != "dir/nested.txt" {
		t.Fatalf("V1 partial prefix: %v", err)
	}
	page, err = sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, MaxKeys: aws.Int32(0)})
	if err != nil || len(page.Contents)+len(page.CommonPrefixes) != 0 || aws.ToBool(page.IsTruncated) {
		t.Fatalf("V1 zero max keys: %v", err)
	}
	if _, err = sdk.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, Prefix: aws.String("../")}); err == nil {
		t.Fatal("V1 unsafe prefix accepted")
	}
}

func TestS3ReadsAndPagination(t *testing.T) {
	fake, _, sdk, _ := testGateway(t)
	ctx := context.Background()
	bucket := aws.String("cloudreve")
	bs, e := sdk.ListBuckets(ctx, &s3.ListBucketsInput{})
	if e != nil || len(bs.Buckets) != 1 {
		t.Fatalf("ListBuckets: %v", e)
	}
	if _, e = sdk.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: bucket}); e != nil {
		t.Fatal(e)
	}
	loc, e := sdk.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: bucket})
	if e != nil || loc.LocationConstraint != "" {
		t.Fatalf("location: %v %v", loc, e)
	}
	for _, key := range []string{"a.bin", "中文 空格.txt", "dir/nested.txt", "literal%2Fname.txt", "special +?#%.txt", "old-upload.bin", "zero"} {
		h, e := sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: &key})
		if e != nil {
			t.Fatalf("HEAD %q: %v", key, e)
		}
		if aws.ToInt64(h.ContentLength) != fake.files[key].Size {
			t.Fatal("wrong HEAD size")
		}
		r, e := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: &key})
		if e != nil {
			t.Fatalf("GET %q: %v", key, e)
		}
		data, e := io.ReadAll(r.Body)
		r.Body.Close()
		if e != nil || !bytes.Equal(data, fake.content[fake.files[key].Primary]) {
			t.Fatalf("GET content: %v", e)
		}
		_, e = sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: &key, IfNoneMatch: h.ETag})
		if errorCode(e) != "NotModified" {
			t.Fatalf("IfNoneMatch: %v", e)
		}
		_, e = sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: &key, IfMatch: aws.String(`"wrong"`)})
		if errorCode(e) != "PreconditionFailed" {
			t.Fatalf("IfMatch: %v", e)
		}
	}
	_, e = sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("new-upload.bin")})
	if errorCode(e) != "NotFound" && errorCode(e) != "NoSuchKey" {
		t.Fatalf("placeholder HEAD: %v", e)
	}
	var token *string
	keys := map[string]bool{}
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatal("endless pagination")
		}
		l, e := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, MaxKeys: aws.Int32(2), ContinuationToken: token})
		if e != nil {
			t.Fatal(e)
		}
		for _, o := range l.Contents {
			if keys[*o.Key] {
				t.Fatal("duplicate object")
			}
			keys[*o.Key] = true
		}
		if !aws.ToBool(l.IsTruncated) {
			break
		}
		token = l.NextContinuationToken
	}
	if len(keys) != 7 || keys["new-upload.bin"] {
		t.Fatalf("wrong visible count %d", len(keys))
	}
	l, e := sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, Delimiter: aws.String("/"), MaxKeys: aws.Int32(100)})
	if e != nil {
		t.Fatal(e)
	}
	if len(l.CommonPrefixes) != 2 {
		t.Fatalf("directory prefixes %d", len(l.CommonPrefixes))
	}
	l, e = sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String("dir/ne")})
	if e != nil || len(l.Contents) != 1 || *l.Contents[0].Key != "dir/nested.txt" {
		t.Fatalf("partial prefix: %v", e)
	}
	if token == nil {
		t.Fatal("no cursor created")
	}
	bad := *token + "x"
	_, e = sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, ContinuationToken: &bad})
	if errorCode(e) != "InvalidToken" {
		t.Fatalf("tampered cursor: %v", e)
	}
	_, e = sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, ContinuationToken: token, Prefix: aws.String("dir/")})
	if errorCode(e) != "InvalidToken" {
		t.Fatalf("cursor scope: %v", e)
	}
}

func TestS3RangeAndBrokenStream(t *testing.T) {
	fake, be, sdk, _ := testGateway(t)
	ctx := context.Background()
	bucket, key := aws.String("cloudreve"), aws.String("a.bin")
	content := fake.content[fake.files["a.bin"].Primary]
	for _, tc := range []struct {
		r    string
		want []byte
	}{{"bytes=0-3", content[:4]}, {"bytes=4-8", content[4:9]}, {"bytes=-4", content[len(content)-4:]}, {"bytes=4-", content[4:]}, {"bytes=0-999", content}} {
		r, e := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key, Range: &tc.r})
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r.Body)
		r.Body.Close()
		if e != nil || !bytes.Equal(data, tc.want) || r.ContentRange == nil {
			t.Fatalf("range %s: %v", tc.r, e)
		}
	}
	for _, r := range []string{"bytes=999-", "bytes=-0", "bytes=3-1", "bytes=1-2,4-5"} {
		_, e := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key, Range: &r})
		if errorCode(e) != "InvalidRange" {
			t.Fatalf("invalid range: %v", e)
		}
	}
	fake.ignoreRange.Store(true)
	_, e := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key, Range: aws.String("bytes=0-3")})
	if errorCode(e) != "ServiceUnavailable" {
		t.Fatalf("ignored range accepted: %v", e)
	}
	fake.ignoreRange.Store(false)
	fake.truncate.Store(true)
	r, e := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	if e == nil {
		_, e = io.ReadAll(r.Body)
		r.Body.Close()
	}
	if e == nil {
		t.Fatal("truncated stream reported success")
	}
	fake.truncate.Store(false)
	be.Config.MaxEntries = 1
	_, e = sdk.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if errorCode(e) != "ServiceUnavailable" {
		t.Fatalf("budget silently truncated: %v", e)
	}
}

func TestS3AuthIsolationAndReadOnly(t *testing.T) {
	fake, _, sdk, endpoint := testGateway(t)
	ctx := context.Background()
	before := fake.calls.Load()
	signed := func(target, secret, region string, change bool) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", endpoint+target, nil)
		sum := sha256.Sum256(nil)
		hash := hex.EncodeToString(sum[:])
		req.Header.Set("X-Amz-Content-Sha256", hash)
		e := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: testAccess, SecretAccessKey: secret}, req, hash, "s3", region, time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		if e != nil {
			t.Fatal(e)
		}
		if change {
			req.URL.RawQuery = "prefix=changed"
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	for _, tc := range []struct {
		target, secret, region string
		change                 bool
	}{{"/cloudreve/a.bin", "wrong-secret", "us-east-1", false}, {"/cloudreve/a.bin", testSecret, "wrong-region", false}, {"/cloudreve?list-type=2", testSecret, "us-east-1", true}, {"/cloudreve/../outside", testSecret, "us-east-1", false}, {"/cloudreve/a//b", testSecret, "us-east-1", false}, {"/cloudreve/a%5Cb", testSecret, "us-east-1", false}, {"/cloudreve/%2e%2e/outside", testSecret, "us-east-1", false}} {
		if r := signed(tc.target, tc.secret, tc.region, tc.change); r.StatusCode < 400 {
			t.Fatalf("unsafe request accepted: %d", r.StatusCode)
		}
	}
	wrongAccess := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("unknown-access", testSecret, ""), RetryMaxAttempts: 1})
	if _, e := wrongAccess.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("a.bin")}); e == nil {
		t.Fatal("unknown access key accepted")
	}
	res, e := http.Get(endpoint + "/cloudreve/a.bin")
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode < 400 {
		t.Fatal("anonymous read accepted")
	}
	if fake.calls.Load() != before {
		t.Fatal("rejected request reached Cloudreve")
	}
	for _, key := range []string{"%2e%2e/outside", "missing", "dir/"} {
		_, e = sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("cloudreve"), Key: &key})
		if e == nil {
			t.Fatal("unexpected object escaped scope")
		}
	}
	_, e = sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("write"), Body: strings.NewReader("new")})
	if e == nil {
		t.Fatal("read-only PUT succeeded")
	}
	_, e = sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("a.bin")})
	if e == nil {
		t.Fatal("read-only DELETE succeeded")
	}
	if r := signed("/cloudreve?acl", testSecret, "us-east-1", false); r.StatusCode != 501 {
		t.Fatalf("unsupported ACL returned %d", r.StatusCode)
	}
	if r := signed("/other-bucket/a.bin", testSecret, "us-east-1", false); r.StatusCode != 404 {
		t.Fatalf("other bucket returned %d", r.StatusCode)
	}
}

func TestS3WritesOverwriteDeleteAndRecovery(t *testing.T) {
	fake, be, sdk, _ := testGatewayMode(t, true, 1<<20)
	ctx := context.Background()
	bucket := aws.String("cloudreve")
	key := aws.String("netcatty/deep/sync.json")
	first := []byte(`{"text":"a short Cloudreve sync object"}`)
	put := func(body []byte) {
		t.Helper()
		_, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: bytes.NewReader(body), ContentType: aws.String("application/json")})
		if err != nil {
			t.Fatalf("PutObject: %v", err)
		}
	}
	put(first)
	got, err := sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil || !bytes.Equal(data, first) {
		t.Fatalf("first uploaded value mismatch: %v", err)
	}
	long := []byte(`{"text":"this is a longer replacement value to exercise versioned overwrite"}`)
	put(long)
	short := []byte(`{"text":"short"}`)
	put(short)
	got, err = sdk.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(got.Body)
	got.Body.Close()
	if err != nil || !bytes.Equal(data, short) {
		t.Fatalf("short overwrite content mismatch: %v", err)
	}
	_, err = sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: bytes.NewReader(first), IfNoneMatch: aws.String("*"), ContentType: aws.String("application/json")})
	if errorCode(err) != "PreconditionFailed" {
		t.Fatalf("If-None-Match did not protect current object: %v", err)
	}
	if _, err = sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err = sdk.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key}); err != nil {
		t.Fatalf("idempotent DeleteObject: %v", err)
	}
	if _, err = sdk.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key}); errorCode(err) != "NotFound" && errorCode(err) != "NoSuchKey" {
		t.Fatalf("deleted object remains visible: %v", err)
	}

	oldContent := append([]byte(nil), fake.content[fake.files["a.bin"].Primary]...)
	fake.failChunk.Store(true)
	_, err = sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String("a.bin"), Body: strings.NewReader("failed replacement"), ContentType: aws.String("application/octet-stream")})
	fake.failChunk.Store(false)
	if err == nil {
		t.Fatal("failed chunk unexpectedly completed an overwrite")
	}
	oldAfterFailure := fake.files["a.bin"]
	if oldAfterFailure.Metadata["sys:upload_session_id"] != "" || !bytes.Equal(fake.content[oldAfterFailure.Primary], oldContent) {
		t.Fatal("failed overwrite did not preserve the previous object")
	}

	old := fake.files["a.bin"]
	pending := old
	pending.Metadata = map[string]string{"sys:upload_session_id": "recover-me"}
	fake.files["a.bin"] = pending
	fake.uploads["recover-me"] = &fakeUpload{key: "a.bin", size: 12, previous: old, hadPrevious: true}
	journal := be.journalPath("a.bin", "recover-me")
	if err = saveJournal(journal, uploadJournal{Site: be.Config.Site, Root: be.Config.Root, Key: "a.bin", SessionID: "recover-me"}); err != nil {
		t.Fatal(err)
	}
	if err = be.RecoverWrites(ctx); err != nil {
		t.Fatalf("RecoverWrites: %v", err)
	}
	if _, exists := fake.uploads["recover-me"]; exists || fake.files["a.bin"].Metadata["sys:upload_session_id"] != "" {
		t.Fatal("recovery did not cancel the recorded pending upload")
	}
}

func TestS3WriteRejectsOversizeAndBadDigestBeforeUpstream(t *testing.T) {
	fake, be, sdk, endpoint := testGatewayMode(t, true, 8)
	ctx := context.Background()
	before := len(fake.uploads)
	_, err := sdk.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("oversize"), Body: strings.NewReader("0123456789")})
	if errorCode(err) != "EntityTooLarge" {
		t.Fatalf("oversize upload accepted: %v", err)
	}
	badMD5 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	payload := []byte("12345678")
	req, err := http.NewRequest(http.MethodPut, endpoint+"/cloudreve/bad-digest", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-MD5", badMD5)
	payloadHash := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(payloadHash[:])
	req.Header.Set("X-Amz-Content-Sha256", hashHex)
	if err = v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: testAccess, SecretAccessKey: testSecret}, req, hashHex, "s3", "us-east-1", time.Now(), func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true }); err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	errorBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(errorBody), "BadDigest") {
		t.Fatalf("bad digest was not rejected: status=%d", response.StatusCode)
	}
	hashReader, err := utils.NewHashReader(strings.NewReader("12345678"), badMD5, utils.HashTypeContentMD5)
	if err != nil {
		t.Fatal(err)
	}
	_, err = be.PutObject(ctx, s3response.PutObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("bad-digest-direct"), ContentLength: aws.Int64(8), ContentType: aws.String("application/octet-stream"), Body: hashReader})
	var s3Error s3err.S3Error
	if !errors.As(err, &s3Error) || s3Error.BaseError().Code != "BadDigest" {
		t.Fatalf("streaming checksum failure was not preserved: %v", err)
	}
	_, err = be.PutObject(ctx, s3response.PutObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("short-body"), ContentLength: aws.Int64(5), ContentType: aws.String("application/octet-stream"), Body: strings.NewReader("four")})
	if err == nil {
		t.Fatal("short body was not rejected")
	}
	if len(fake.uploads) != before {
		t.Fatalf("rejected payload created an upstream upload session (%d -> %d)", before, len(fake.uploads))
	}
}

func TestConditionsFollowS3Precedence(t *testing.T) {
	f := cloudreve.File{ID: "a", Primary: "b", Updated: time.Unix(100, 999), Size: 1}
	old, future := time.Unix(99, 0), time.Unix(101, 0)
	tag := etag(f)
	if e := condition(f, &tag, nil, nil, &old); e != nil {
		t.Fatal("IfMatch must take precedence over IfUnmodifiedSince")
	}
	none := `"different"`
	if e := condition(f, nil, &none, &future, nil); e != nil {
		t.Fatal("IfNoneMatch must take precedence over IfModifiedSince")
	}
	if e := condition(f, nil, nil, &future, nil); e == nil {
		t.Fatal("modified-since accepted")
	}
	if e := condition(f, nil, nil, nil, &old); e == nil {
		t.Fatal("unmodified-since accepted")
	}
	weak := "W/" + tag
	if condition(f, &weak, nil, nil, nil) == nil {
		t.Fatal("weak IfMatch accepted")
	}
	if condition(f, nil, &weak, nil, nil) == nil {
		t.Fatal("weak IfNoneMatch missed")
	}
}

func TestDownloadSlotsRemainHeldUntilBodyClosed(t *testing.T) {
	_, be, _, _ := testGateway(t)
	be.slots = make(chan struct{}, 1)
	ctx := context.Background()
	in := &s3.GetObjectInput{Bucket: aws.String("cloudreve"), Key: aws.String("a.bin")}
	r, e := be.GetObject(ctx, in)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = be.HeadObject(ctx, &s3.HeadObjectInput{Bucket: in.Bucket, Key: in.Key}); e == nil {
		t.Fatal("download did not retain its concurrency slot")
	}
	r.Body.Close()
	r.Body.Close()
	if _, e = be.HeadObject(ctx, &s3.HeadObjectInput{Bucket: in.Bucket, Key: in.Key}); e != nil {
		t.Fatalf("closed download did not release slot: %v", e)
	}
}

func Example_byteRange() {
	_, n, cr, e := byteRange("bytes=-4", 10)
	fmt.Println(n, cr, e) // Output: 4 bytes 6-9/10 <nil>
}
