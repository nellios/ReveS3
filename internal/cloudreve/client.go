package cloudreve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Error deliberately excludes upstream messages, URLs and credentials.
type Error struct{ Code, Status int }

func (e *Error) Error() string {
	return fmt.Sprintf("cloudreve error (HTTP %d, code %d)", e.Status, e.Code)
}

var ErrLogin = errors.New("Cloudreve session unavailable; run login again")
var ErrUpstream = errors.New("Cloudreve response unavailable or invalid")

type Session struct {
	Site           string    `json:"site,omitempty"`
	Access         string    `json:"access_token"`
	Refresh        string    `json:"refresh_token"`
	AccessExpires  time.Time `json:"access_expires"`
	RefreshExpires time.Time `json:"refresh_expires"`
}

type Client struct {
	base, sessionFile     string
	apiHTTP, downloadHTTP *http.Client
	mu                    sync.Mutex
	session               Session
	failed                bool
}

func New(base, sessionFile string, timeout time.Duration) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = timeout
	return &Client{base: strings.TrimSuffix(base, "/"), sessionFile: sessionFile,
		apiHTTP: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		downloadHTTP: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.User != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") || (via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https") {
				return errors.New("unsafe download redirect")
			}
			return nil
		}}}
}

// NewDirect connects to Cloudreve without consulting HTTP proxy environment
// variables. It neither changes system proxy settings nor starts a local server.
func NewDirect(base, sessionFile string, timeout time.Duration) *Client {
	c := New(base, sessionFile, timeout)
	// New gives both clients the same freshly cloned transport.
	c.apiHTTP.Transport.(*http.Transport).Proxy = nil
	return c
}

func (c *Client) Load() error {
	data, e := os.ReadFile(c.sessionFile)
	if e != nil {
		return ErrLogin
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if json.Unmarshal(data, &c.session) != nil || c.session.Access == "" || c.session.Refresh == "" || c.session.Site != c.base {
		return ErrLogin
	}
	return nil
}

func (c *Client) save(s Session) error {
	s.Site = c.base
	if s.Access == "" || s.Refresh == "" || s.AccessExpires.IsZero() {
		return ErrLogin
	}
	if c.sessionFile == "" {
		c.session = s
		return nil
	} // In-memory test clients.
	if e := os.MkdirAll(filepath.Dir(c.sessionFile), 0700); e != nil {
		return errors.New("cannot create session directory")
	}
	f, e := os.CreateTemp(filepath.Dir(c.sessionFile), ".session-*")
	if e != nil {
		return errors.New("cannot create session file")
	}
	name := f.Name()
	defer os.Remove(name)
	e = f.Chmod(0600)
	if e == nil {
		e = json.NewEncoder(f).Encode(s)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e == nil {
		e = os.Rename(name, c.sessionFile)
	}
	if e != nil {
		return errors.New("cannot persist session")
	}
	c.session = s
	return nil
}

func (c *Client) Login(ctx context.Context, email, password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var res struct {
		Token Session `json:"token"`
	}
	if e := c.call(ctx, "POST", "/session/token", map[string]string{"email": email, "password": password}, "", &res); e != nil {
		return e
	}
	if e := c.save(res.Token); e != nil {
		return e
	}
	c.failed = false
	return nil
}

func (c *Client) token(ctx context.Context, rejected string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed || c.session.Access == "" {
		return "", ErrLogin
	}
	// A request rejected with an older token can reuse another goroutine's refresh.
	if rejected != "" && c.session.Access != rejected {
		return c.session.Access, nil
	}
	if rejected == "" && time.Until(c.session.AccessExpires) > 30*time.Second {
		return c.session.Access, nil
	}
	if !c.session.RefreshExpires.IsZero() && time.Now().After(c.session.RefreshExpires) {
		c.failed = true
		return "", ErrLogin
	}
	var s Session
	if e := c.call(ctx, "POST", "/session/token/refresh", map[string]string{"refresh_token": c.session.Refresh}, "", &s); e != nil {
		c.failed = true
		return "", ErrLogin
	}
	if e := c.save(s); e != nil {
		c.failed = true
		return "", ErrLogin
	}
	return s.Access, nil
}

func (c *Client) call(ctx context.Context, method, path string, input any, token string, out any) error {
	var body io.Reader
	if input != nil {
		data, e := json.Marshal(input)
		if e != nil {
			return ErrUpstream
		}
		body = bytes.NewReader(data)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+"/api/v4"+path, body)
	if e != nil {
		return ErrUpstream
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cloudreve-s3/0.1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := c.apiHTTP.Do(req)
	if e != nil {
		return ErrUpstream
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if e != nil || len(data) > 4*1024*1024 {
		return ErrUpstream
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
		return ErrUpstream
	}
	if resp.StatusCode >= 300 || *envelope.Code != 0 {
		return &Error{Code: *envelope.Code, Status: resp.StatusCode}
	}
	if out != nil && json.Unmarshal(envelope.Data, out) != nil {
		return ErrUpstream
	}
	return nil
}

func (c *Client) API(ctx context.Context, method, path string, input, out any) error {
	t, e := c.token(ctx, "")
	if e != nil {
		return e
	}
	e = c.call(ctx, method, path, input, t, out)
	var ce *Error
	if errors.As(e, &ce) && (ce.Status == 401 || ce.Code == 401 || ce.Code == 40020) {
		t, e = c.token(ctx, t)
		if e != nil {
			return e
		}
		e = c.call(ctx, method, path, input, t, out)
		if errors.As(e, &ce) && (ce.Status == 401 || ce.Code == 401 || ce.Code == 40020) {
			c.mu.Lock()
			c.failed = true
			c.mu.Unlock()
			return ErrLogin
		}
	}
	return e
}

// UploadChunk sends one bounded Cloudreve local-policy chunk. The body must be
// seekable so an authentication refresh can safely retry the request once.
func (c *Client) UploadChunk(ctx context.Context, id string, index int64, body io.ReadSeeker, size int64) error {
	path := "/file/upload/" + url.PathEscape(id) + "/" + fmt.Sprint(index)
	for attempt := 0; attempt < 2; attempt++ {
		t, e := c.token(ctx, "")
		if e != nil {
			return e
		}
		if _, e = body.Seek(0, io.SeekStart); e != nil {
			return ErrUpstream
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v4"+path, body)
		if e != nil {
			return ErrUpstream
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("User-Agent", "cloudreve-s3/0.1")
		req.Header.Set("Authorization", "Bearer "+t)
		resp, err := c.apiHTTP.Do(req)
		if err != nil {
			return ErrUpstream
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
		resp.Body.Close()
		if readErr != nil || len(data) > 4*1024*1024 {
			return ErrUpstream
		}
		var envelope struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal(data, &envelope) != nil || envelope.Code == nil {
			return ErrUpstream
		}
		if resp.StatusCode == http.StatusUnauthorized || *envelope.Code == 401 || *envelope.Code == 40020 {
			if attempt == 0 {
				if _, err = c.token(ctx, t); err != nil {
					return err
				}
				continue
			}
			c.mu.Lock()
			c.failed = true
			c.mu.Unlock()
			return ErrLogin
		}
		if resp.StatusCode >= 300 || *envelope.Code != 0 {
			return &Error{Code: *envelope.Code, Status: resp.StatusCode}
		}
		return nil
	}
	return ErrUpstream
}

type Entity struct {
	ID   string `json:"id"`
	Type int    `json:"type"`
	Size int64  `json:"size"`
}
type File struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Path     string            `json:"path"`
	Type     int               `json:"type"`
	Size     int64             `json:"size"`
	Updated  time.Time         `json:"updated_at"`
	Created  time.Time         `json:"created_at"`
	Primary  string            `json:"primary_entity"`
	Metadata map[string]string `json:"metadata"`
	Extended struct {
		Entities []Entity `json:"entities"`
	} `json:"extended_info"`
}

func (f File) Committed() bool {
	for _, e := range f.Extended.Entities {
		if e.ID == f.Primary && e.Type == 0 && e.Size == f.Size {
			return true
		}
	}
	return false
}
func (c *Client) Info(ctx context.Context, uri string) (File, error) {
	var f File
	e := c.API(ctx, "GET", "/file/info?"+url.Values{"uri": {uri}, "extended": {"true"}}.Encode(), nil, &f)
	return f, e
}

type Listing struct {
	Files      []File `json:"files"`
	Parent     File   `json:"parent"`
	Pagination struct {
		IsCursor                   bool   `json:"is_cursor"`
		Next                       string `json:"next_token"`
		Page, PageSize, TotalItems int
	} `json:"pagination"`
}

func (c *Client) List(ctx context.Context, uri, next string, page int) (Listing, error) {
	var l Listing
	q := url.Values{"uri": {uri}, "page_size": {"100"}, "page": {fmt.Sprint(page)}, "order_by": {"name"}, "order_direction": {"asc"}}
	if next != "" {
		q.Set("next_page_token", next)
	}
	e := c.API(ctx, "GET", "/file?"+q.Encode(), nil, &l)
	return l, e
}

func (c *Client) Open(ctx context.Context, uri, entity, byteRange string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var res struct {
			URLs []struct {
				URL string `json:"url"`
			} `json:"urls"`
		}
		if e := c.API(ctx, "POST", "/file/url", map[string]any{"uris": []string{uri}, "entity": entity, "no_cache": true}, &res); e != nil {
			return nil, e
		}
		if len(res.URLs) != 1 {
			return nil, ErrUpstream
		}
		u, e := url.Parse(res.URLs[0].URL)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || (strings.HasPrefix(c.base, "https:") && u.Scheme != "https") {
			return nil, ErrUpstream
		}
		req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if e != nil {
			return nil, ErrUpstream
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		resp, e := c.downloadHTTP.Do(req)
		if e != nil {
			return nil, ErrUpstream
		}
		if attempt == 0 && (resp.StatusCode == 401 || resp.StatusCode == 403) {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, ErrUpstream
}
