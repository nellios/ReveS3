package cloudreve

import (
	"net/http"
	"testing"
	"time"
)

func TestDirectClientDoesNotUseEnvironmentProxy(t *testing.T) {
	direct := NewDirect("https://example.invalid", "", time.Second)
	for _, client := range []*http.Client{direct.apiHTTP, direct.downloadHTTP} {
		transport, ok := client.Transport.(*http.Transport)
		if !ok || transport.Proxy != nil {
			t.Fatal("native direct client still consults environment proxies")
		}
	}
	ordinary := New("https://example.invalid", "", time.Second)
	if ordinary.apiHTTP.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("creating a direct client changed ordinary gateway transports")
	}
}
