package server

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/frame"
	"github.com/telegramdesktop/tproxy-server/internal/session"
)

func TestAuthenticSecretsNeverReachPublicHandler(t *testing.T) {
	application, _ := newTestServer(t, "127.0.0.1:1")
	defer application.Shutdown()
	application.publicUpstream = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("authentic secret reached public handler")
	})
	bootstrap, err := application.manager.IssueBootstrap(&application.config.Profiles[0], "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	created, err := application.manager.Create(bootstrap, "127.0.0.1", frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	capability := config.CapabilityString(application.config.Profiles[0].Capability)
	for _, restart := range []bool{false, true} {
		if restart {
			application.manager.Shutdown()
			key, err := config.ReadTokenKey(application.config.TokenKeyFile)
			if err != nil {
				t.Fatal(err)
			}
			application.manager = session.NewManager(application.config, key)
		}
		for _, secret := range []string{capability, bootstrap, created.Token} {
			for _, target := range []string{
				"/wrong/" + secret,
				"/?bridge=" + secret + "&extra=1",
				"/?bridge=wrong&bridge=" + secret,
				"/?bridge=" + secret + ";bad=%zz",
				"/?bridge=%" + hex.EncodeToString([]byte{secret[0]}) + secret[1:],
				"/api/v1/up?bridge=" + secret,
			} {
				r := httptest.NewRequest(http.MethodHead, "http://"+testHost+target, nil)
				assertPrivateRejection(t, application, r)
			}
			for _, name := range []string{"Authorization", "Cookie", "Referer", "X-Unexpected"} {
				r := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/wrong", strings.NewReader("body"))
				r.Header.Add(name, "random")
				r.Header.Add(name, "Bearer "+secret+", malformed")
				assertPrivateRejection(t, application, r)
			}
			for _, protocol := range []string{"chat, tproxy-v1." + secret, "tproxy-lane-v1." + secret + ".bad"} {
				r := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/api/v1/ws", nil)
				r.Header.Set("Sec-WebSocket-Protocol", protocol)
				assertPrivateRejection(t, application, r)
			}
		}
		for _, target := range []string{"/api/v1/session", "/api/v1/up", "/api/v1/down", "/api/v1/ws"} {
			r := httptest.NewRequest(http.MethodPost, "http://"+testHost+target, nil)
			r.Header.Set("Authorization", "Bearer "+created.Token)
			r.Host = "wrong.example.com"
			assertPrivateRejection(t, application, r)
		}
	}
}

func assertPrivateRejection(t *testing.T, application *Server, r *http.Request) {
	t.Helper()
	w := httptest.NewRecorder()
	application.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("authentic malformed request did not fail locally: %d", w.Code)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines int
}

func (w *deadlineRecorder) SetReadDeadline(time.Time) error {
	w.deadlines++
	return nil
}

func TestOnlyInternalBodiesGetCarrierDeadline(t *testing.T) {
	application, _ := newTestServer(t, "127.0.0.1:1")
	defer application.Shutdown()
	application.publicUpstream = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	bootstrap, err := application.manager.IssueBootstrap(&application.config.Profiles[0], "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{strings.Repeat("A", 43), bootstrap} {
		for _, path := range []string{"/api/v1/session", "/random"} {
			r := httptest.NewRequest(http.MethodPost, "http://"+testHost+path, bytes.NewReader([]byte("body")))
			r.Header.Set("Authorization", "Bearer "+token)
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			application.Handler().ServeHTTP(w, r)
			if (w.deadlines != 0) != (token == bootstrap) {
				t.Fatal("body deadline does not follow secret provenance")
			}
		}
	}
}

func TestMalformedDeleteDoesNotCloseSession(t *testing.T) {
	application, _ := newTestServer(t, "127.0.0.1:1")
	defer application.Shutdown()
	bootstrap, err := application.manager.IssueBootstrap(&application.config.Profiles[0], "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	created, err := application.manager.Create(bootstrap, "127.0.0.1", frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodDelete, "http://"+testHost+"/api/v1/session", strings.NewReader("unexpected body"))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer "+created.Token)
	assertPrivateRejection(t, application, r)
	if _, err := application.manager.Get(created.Token); err != nil {
		t.Fatal("malformed DELETE closed the session")
	}
}

func TestLegacyDrainContainsOldTokensUntilClientsReload(t *testing.T) {
	application, _ := newTestServer(t, "127.0.0.1:1")
	defer application.Shutdown()
	forwarded := 0
	application.publicUpstream = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded++
		w.WriteHeader(http.StatusAccepted)
	})
	for _, name := range []string{"Authorization", "Sec-WebSocket-Protocol"} {
		r := httptest.NewRequest(http.MethodPost, "http://"+testHost+"/api/v1/up", strings.NewReader("legacy body"))
		value := "Bearer " + strings.Repeat("A", 43)
		if name == "Sec-WebSocket-Protocol" {
			value = "tproxy-v1." + strings.Repeat("A", 43)
		}
		r.Header.Set(name, value)
		application.config.LegacyTokenDrain = true
		assertPrivateRejection(t, application, r)
		application.config.LegacyTokenDrain = false
		application.Handler().ServeHTTP(httptest.NewRecorder(), r)
	}
	if forwarded != 2 {
		t.Fatal("drain switch did not restore public pass-through")
	}
}
