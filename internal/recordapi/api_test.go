package recordapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/schema"
)

func TestInternalRequestPreservesResolvedClientIP(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	app.Settings().TrustedProxy.Headers = []string{"X-Forwarded-For"}
	request := httptest.NewRequest("GET", "http://example.test/api/functions/notes", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.12")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		event := &core.RequestEvent{App: app, Request: r}
		if event.RealIP() != "198.51.100.12" {
			t.Errorf("client IP changed to %s", event.RealIP())
		}
		if r.Header.Get("Authorization") != "caller-token" {
			t.Error("caller auth was lost")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	client := New(handler, context.Background(), "caller-token", PeerAddress(app, request))
	if response, err := client.Fetch("GET", "/api/collections/notes/records", nil); err != nil || response.Status != 200 {
		t.Fatal(response, err)
	}
	app.Settings().TrustedProxy.Headers = nil
	if PeerAddress(app, request) != "192.0.2.1:0" {
		t.Fatal("unconfigured proxy header was trusted")
	}
}
