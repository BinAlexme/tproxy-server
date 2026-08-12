package server

import (
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/frame"
)

const testHost = "proxy.example.com"

func TestPublicFallbackAndCarrierRoundTrip(t *testing.T) {
	backend := startEchoBackend(t)
	application, index := newTestServer(t, backend)
	defer application.Shutdown()
	hosted := httptest.NewServer(application.Handler())
	defer hosted.Close()

	wrong := perform(t, hosted.Client(), request(t, http.MethodGet, hosted.URL+"/?bridge=wrong&extra=1", nil, ""))
	if wrong.StatusCode != http.StatusOK || !bytes.Equal(readResponse(t, wrong), index) {
		t.Fatal("wrong bridge query did not return the public index")
	}

	secret, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	capability := config.CapabilityString(config.DeriveCapability(testHost, secret))
	for _, query := range []string{
		"bridge=" + capability + "&extra=1",
		"bridge=" + capability + "&bridge=" + capability,
		"bridge=%" + hex.EncodeToString([]byte{capability[0]}) + capability[1:],
	} {
		fallback := perform(t, hosted.Client(), request(t, http.MethodGet, hosted.URL+"/?"+query, nil, ""))
		if fallback.StatusCode != http.StatusOK || !bytes.Equal(readResponse(t, fallback), index) {
			t.Fatal("augmented or duplicated valid bridge query did not return the public index")
		}
	}
	bridgeResponse := perform(t, hosted.Client(), request(t, http.MethodGet, hosted.URL+"/?bridge="+url.QueryEscape(capability), nil, ""))
	bridgeBody := readResponse(t, bridgeResponse)
	if bridgeResponse.StatusCode != http.StatusOK || !bytes.Contains(bridgeBody, []byte("tproxy-init")) {
		t.Fatalf("bridge response failed: status %d", bridgeResponse.StatusCode)
	}
	if bytes.Contains(bridgeBody, []byte(hex.EncodeToString(secret))) || bridgeResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("bridge response contained the profile secret or allowed caching")
	}
	if !strings.Contains(bridgeResponse.Header.Get("Content-Security-Policy"), "frame-ancestors http://127.0.0.1:*") {
		t.Fatal("bridge is missing its loopback framing policy")
	}
	match := regexp.MustCompile(`bootstrap="([A-Za-z0-9_-]{43})"`).FindSubmatch(bridgeBody)
	if len(match) != 2 {
		t.Fatal("could not locate embedded bootstrap token")
	}
	bootstrap := string(match[1])

	hello := frame.Encode(frame.Hello, 0, []byte{1})
	created := perform(t, hosted.Client(), apiRequest(t, http.MethodPost, hosted.URL+"/api/v1/session", bootstrap, hello))
	welcome := readResponse(t, created)
	if created.StatusCode != http.StatusOK || !bytes.Equal(welcome, frame.Encode(frame.Welcome, 0, nil)) {
		t.Fatalf("session creation failed: status %d", created.StatusCode)
	}
	sessionToken := created.Header.Get("X-Session-Token")
	if sessionToken == "" {
		t.Fatal("missing session token")
	}

	repeated := perform(t, hosted.Client(), apiRequest(t, http.MethodPost, hosted.URL+"/api/v1/session", bootstrap, hello))
	_ = readResponse(t, repeated)
	if repeated.StatusCode != http.StatusOK || repeated.Header.Get("X-Session-Token") != sessionToken {
		t.Fatal("bootstrap retry was not idempotent")
	}

	streamID := uint32(17)
	secondStreamID := uint32(18)
	uplink := append(frame.Encode(frame.Open, streamID, nil), frame.Encode(frame.Data, streamID, []byte("round trip"))...)
	uplink = append(uplink, frame.Encode(frame.Open, secondStreamID, nil)...)
	uplink = append(uplink, frame.Encode(frame.Data, secondStreamID, []byte("second stream"))...)
	upRequest := apiRequest(t, http.MethodPost, hosted.URL+"/api/v1/up", sessionToken, uplink)
	upRequest.Header.Set("X-Up-Seq", "1")
	up := perform(t, hosted.Client(), upRequest)
	_ = readResponse(t, up)
	if up.StatusCode != http.StatusNoContent || up.Header.Get("X-Up-Ack") != "1" {
		t.Fatalf("uplink failed: status %d", up.StatusCode)
	}

	firstBody, replayBase, firstCursor := pollForData(t, hosted.Client(), hosted.URL, sessionToken, "0", map[uint32][]byte{
		streamID:       []byte("round trip"),
		secondStreamID: []byte("second stream"),
	})
	replayRequest := apiRequest(t, http.MethodPost, hosted.URL+"/api/v1/down", sessionToken, nil)
	replayRequest.Header.Set("X-Down-Cursor", replayBase)
	replayed := perform(t, hosted.Client(), replayRequest)
	replayedBody := readResponse(t, replayed)
	if replayed.StatusCode != http.StatusOK || replayed.Header.Get("X-Down-Cursor") != firstCursor || !bytes.Equal(replayedBody, firstBody) {
		t.Fatal("lost downlink response was not replayed byte-for-byte")
	}

	closeRequest := apiRequest(t, http.MethodDelete, hosted.URL+"/api/v1/session", sessionToken, nil)
	closed := perform(t, hosted.Client(), closeRequest)
	_ = readResponse(t, closed)
	if closed.StatusCode != http.StatusNoContent {
		t.Fatalf("session close failed: status %d", closed.StatusCode)
	}
	closedAgain := perform(t, hosted.Client(), apiRequest(t, http.MethodDelete, hosted.URL+"/api/v1/session", sessionToken, nil))
	_ = readResponse(t, closedAgain)
	if closedAgain.StatusCode != http.StatusNoContent {
		t.Fatalf("session close retry was not idempotent: status %d", closedAgain.StatusCode)
	}
}

func TestAPIRejectsWrongOriginAsPublic404(t *testing.T) {
	backend := startEchoBackend(t)
	application, _ := newTestServer(t, backend)
	defer application.Shutdown()
	hosted := httptest.NewServer(application.Handler())
	defer hosted.Close()

	request := request(t, http.MethodPost, hosted.URL+"/api/v1/session", frame.Encode(frame.Hello, 0, []byte{1}), "application/octet-stream")
	request.Header.Set("Authorization", "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	request.Header.Set("Origin", "https://wrong.example")
	response := perform(t, hosted.Client(), request)
	body := readResponse(t, response)
	if response.StatusCode != http.StatusNotFound || !bytes.Contains(body, []byte("Quiet Systems")) {
		t.Fatal("invalid API request did not receive the ordinary public 404")
	}
}

func TestUplinkBackpressureIsRetryable(t *testing.T) {
	backend := startEchoBackend(t)
	application, _ := newConfiguredTestServer(t, backend, func(value *config.Config) {
		value.Limits.MaxStreamsPerSession = 1
		value.Limits.MaxPendingPerSession = 5500
		value.Limits.MaxPendingItemsPerSession = 64
	})
	defer application.Shutdown()
	hosted := httptest.NewServer(application.Handler())
	defer hosted.Close()

	clientIP := "198.51.100.7"
	bootstrap, err := application.manager.IssueBootstrap(
		&application.config.Profiles[0],
		clientIP)
	if err != nil {
		t.Fatal(err)
	}
	created, err := application.manager.Create(
		bootstrap,
		clientIP,
		frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}

	body := append(
		frame.Encode(frame.Open, 23, nil),
		frame.Encode(frame.Data, 23, bytes.Repeat([]byte{1}, 512))...)
	upRequest := apiRequest(
		t,
		http.MethodPost,
		hosted.URL+"/api/v1/up",
		created.Token,
		body)
	upRequest.Header.Set("X-Up-Seq", "1")
	response := perform(t, hosted.Client(), upRequest)
	_ = readResponse(t, response)
	if response.StatusCode != http.StatusServiceUnavailable ||
		response.Header.Get("Retry-After") != "1" ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("backpressure response was not retryable: status=%d", response.StatusCode)
	}
	if _, err := application.manager.Get(created.Token); err != nil {
		t.Fatal("backpressure invalidated the session token")
	}

	retry := apiRequest(
		t,
		http.MethodPost,
		hosted.URL+"/api/v1/up",
		created.Token,
		frame.Encode(frame.Open, 23, nil))
	retry.Header.Set("X-Up-Seq", "1")
	retried := perform(t, hosted.Client(), retry)
	_ = readResponse(t, retried)
	if retried.StatusCode != http.StatusNoContent ||
		retried.Header.Get("X-Up-Ack") != "1" {
		t.Fatalf("uncommitted sequence was not retryable: status=%d", retried.StatusCode)
	}
}

func TestSessionCapacityOverloadIsRetryable(t *testing.T) {
	backend := startEchoBackend(t)
	application, _ := newConfiguredTestServer(t, backend, func(value *config.Config) {
		value.Limits.MaxSessionsGlobal = 1
		value.Limits.NewSessionsBurst = 10
		value.Limits.NewSessionsPerMinute = 600
	})
	defer application.Shutdown()
	hosted := httptest.NewServer(application.Handler())
	defer hosted.Close()

	clientIP := "198.51.100.7"
	firstBootstrap, err := application.manager.IssueBootstrap(
		&application.config.Profiles[0],
		clientIP)
	if err != nil {
		t.Fatal(err)
	}
	first, err := application.manager.Create(
		firstBootstrap,
		clientIP,
		frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	secondBootstrap, err := application.manager.IssueBootstrap(
		&application.config.Profiles[0],
		clientIP)
	if err != nil {
		t.Fatal(err)
	}
	hello := frame.Encode(frame.Hello, 0, []byte{1})
	overloaded := perform(t, hosted.Client(), apiRequest(
		t,
		http.MethodPost,
		hosted.URL+"/api/v1/session",
		secondBootstrap,
		hello))
	_ = readResponse(t, overloaded)
	if overloaded.StatusCode != http.StatusServiceUnavailable ||
		overloaded.Header.Get("Retry-After") != "1" ||
		overloaded.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("session overload response was not retryable: status=%d", overloaded.StatusCode)
	}
	first.Session.Close()
	deadline := time.Now().Add(time.Second)
	for application.manager.Capacity().Sessions != 0 {
		if time.Now().After(deadline) {
			t.Fatal("closed session did not release capacity")
		}
		time.Sleep(time.Millisecond)
	}
	retried := perform(t, hosted.Client(), apiRequest(
		t,
		http.MethodPost,
		hosted.URL+"/api/v1/session",
		secondBootstrap,
		hello))
	_ = readResponse(t, retried)
	if retried.StatusCode != http.StatusOK {
		t.Fatalf("session overload consumed the bootstrap: status=%d", retried.StatusCode)
	}
}

func TestAdminSurfaceIsSeparate(t *testing.T) {
	backend := startEchoBackend(t)
	application, _ := newTestServer(t, backend)
	defer application.Shutdown()

	admin := httptest.NewRecorder()
	application.AdminHandler().ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/debug/pprof/", nil))
	if admin.Code != http.StatusNotFound {
		t.Fatal("profiling endpoint is enabled by default")
	}
	application.config.EnablePprof = true
	profiling := httptest.NewRecorder()
	application.AdminHandler().ServeHTTP(profiling, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/debug/pprof/", nil))
	if profiling.Code != http.StatusOK || !strings.Contains(profiling.Body.String(), "profile") {
		t.Fatal("explicitly enabled loopback profiling endpoint is unavailable")
	}
	public := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/debug/pprof/", nil)
	request.Host = testHost
	application.Handler().ServeHTTP(public, request)
	if public.Code != http.StatusNotFound {
		t.Fatal("profiling endpoint was available from the public handler")
	}
	metrics := httptest.NewRecorder()
	application.AdminHandler().ServeHTTP(metrics, httptest.NewRequest(
		http.MethodGet,
		"http://127.0.0.1/metrics",
		nil))
	for _, name := range []string{
		"tproxy_streams_live",
		"tproxy_backend_dials_in_flight",
		"tproxy_pending_bytes",
		"tproxy_streams_rejected_total",
		"tproxy_backend_dial_failures_total",
	} {
		if !strings.Contains(metrics.Body.String(), name+" ") {
			t.Fatalf("metrics output omitted %s", name)
		}
	}
}

func newTestServer(t *testing.T, backend string) (*Server, []byte) {
	return newConfiguredTestServer(t, backend, nil)
}

func newConfiguredTestServer(
	t *testing.T,
	backend string,
	configure func(*config.Config)) (*Server, []byte) {
	t.Helper()
	directory := t.TempDir()
	index := []byte("<!doctype html><title>Quiet Systems</title><p>ordinary site</p>")
	notFound := []byte("<!doctype html><title>Not found</title><p>Quiet Systems</p>")
	if err := os.WriteFile(filepath.Join(directory, "index.html"), index, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "404.html"), notFound, 0600); err != nil {
		t.Fatal(err)
	}
	value := config.Defaults()
	value.PublicHostname = testHost
	value.PublicDir = directory
	value.Timeouts.LongPoll = config.Duration(500 * time.Millisecond)
	value.Timeouts.BackendDial = config.Duration(time.Second)
	if configure != nil {
		configure(&value)
	}
	secret, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	value.Profiles = []config.Profile{{
		Name:       "default",
		Backend:    backend,
		Capability: config.DeriveCapability(testHost, secret),
	}}
	application, err := New(value)
	if err != nil {
		t.Fatal(err)
	}
	return application, index
}

func startEchoBackend(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	return listener.Addr().String()
}

func pollForData(t *testing.T, client *http.Client, baseURL, token, cursor string, expected map[uint32][]byte) ([]byte, string, string) {
	t.Helper()
	received := make(map[uint32][]byte)
	for attempt := 0; attempt != 8; attempt++ {
		poll := apiRequest(t, http.MethodPost, baseURL+"/api/v1/down", token, nil)
		poll.Header.Set("X-Down-Cursor", cursor)
		response := perform(t, client, poll)
		body := readResponse(t, response)
		if response.StatusCode == http.StatusNoContent {
			continue
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("downlink failed: status %d", response.StatusCode)
		}
		next := response.Header.Get("X-Down-Cursor")
		frames, err := frame.ParseAll(body, frame.MaxPayload)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range frames {
			if _, exists := expected[value.StreamID]; value.Type == frame.Data && exists {
				received[value.StreamID] = append(received[value.StreamID], value.Payload...)
			}
		}
		complete := true
		for id, payload := range expected {
			if !bytes.Equal(received[id], payload) {
				complete = false
			}
		}
		if complete {
			return body, cursor, next
		}
		cursor = next
	}
	t.Fatal("echo data did not arrive")
	return nil, "", ""
}

func request(t *testing.T, method, target string, body []byte, contentType string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = testHost
	request.Header.Set("X-Forwarded-For", "198.51.100.7")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return request
}

func apiRequest(t *testing.T, method, target, token string, body []byte) *http.Request {
	t.Helper()
	contentType := ""
	if body != nil {
		contentType = "application/octet-stream"
	}
	request := request(t, method, target, body, contentType)
	request.Header.Set("Origin", "https://"+testHost)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func perform(t *testing.T, client *http.Client, request *http.Request) *http.Response {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
