package cloudreve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentRefreshAndFailedSession(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var refreshes atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v4/session/token/refresh" {
					refreshes.Add(1)
					if failed {
						fmt.Fprint(w, `{"code":40020}`)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": Session{Access: "new-access", Refresh: "new-refresh", AccessExpires: time.Now().Add(time.Hour)}})
					return
				}
				if r.Header.Get("Authorization") != "Bearer new-access" {
					t.Error("stale token used")
				}
				fmt.Fprint(w, `{"code":0,"data":{}}`)
			}))
			defer s.Close()
			c := New(s.URL, filepath.Join(t.TempDir(), "session.json"), time.Second*3)
			c.session = Session{Access: "expired", Refresh: "refresh", AccessExpires: time.Now().Add(-time.Hour)}
			var wg sync.WaitGroup
			for range 16 {
				wg.Go(func() {
					e := c.API(context.Background(), "GET", "/file", nil, nil)
					if failed && !errors.Is(e, ErrLogin) {
						t.Error("failed refresh did not require login")
					}
					if !failed && e != nil {
						t.Error(e)
					}
				})
			}
			wg.Wait()
			if refreshes.Load() != 1 {
				t.Fatalf("refresh called %d times", refreshes.Load())
			}
			if !failed {
				restored := New(s.URL, c.sessionFile, time.Second)
				if e := restored.Load(); e != nil {
					t.Fatal(e)
				}
				if restored.session.Access != "new-access" {
					t.Fatal("rotated session was not persisted")
				}
			}
		})
	}
}

func TestReactiveRefreshAndNoRetryForParameterError(t *testing.T) {
	var refreshes atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/session/token/refresh" {
			refreshes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": Session{Access: "new", Refresh: "refresh2", AccessExpires: time.Now().Add(time.Hour)}})
			return
		}
		if r.URL.Path == "/api/v4/bad" {
			fmt.Fprint(w, `{"code":40001}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			fmt.Fprint(w, `{"code":401}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	}))
	defer s.Close()
	c := New(s.URL, "", time.Second*3)
	c.session = Session{Access: "old", Refresh: "refresh", AccessExpires: time.Now().Add(time.Hour)}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if e := c.API(context.Background(), "GET", "/file", nil, nil); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("reactive refresh count %d", refreshes.Load())
	}
	if c.API(context.Background(), "GET", "/bad", nil, nil) == nil {
		t.Fatal("business error ignored")
	}
	if refreshes.Load() != 1 {
		t.Fatal("parameter failure caused refresh")
	}
}

func TestDownloadCredentialsAndAPIResultLimits(t *testing.T) {
	var destination string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("API authorization leaked to storage")
		}
		fmt.Fprint(w, "data")
	}))
	defer storage.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization attached to signed URL")
		}
		http.Redirect(w, r, storage.URL, 302)
	}))
	defer redirect.Close()
	destination = redirect.URL
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/file/url" {
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"urls": []map[string]string{{"url": destination}}}})
			return
		}
		if r.URL.Path == "/api/v4/redirect" {
			http.Redirect(w, r, storage.URL, 302)
			return
		}
		if r.URL.Path == "/api/v4/large" {
			w.Write(make([]byte, 4*1024*1024+1))
			return
		}
		fmt.Fprint(w, `{"code":40044}`)
	}))
	defer api.Close()
	c := New(api.URL, "", time.Second*3)
	c.session = Session{Access: "api-token", Refresh: "refresh", AccessExpires: time.Now().Add(time.Hour)}
	r, e := c.Open(context.Background(), "cloudreve://my/root/file", "entity", "")
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	for _, p := range []string{"/redirect", "/large", "/missing"} {
		if e = c.API(context.Background(), "GET", p, nil, nil); e == nil {
			t.Fatalf("%s accepted", p)
		}
	}
}
