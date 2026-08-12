package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/telegramdesktop/tproxy-server/internal/bridge"
	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/session"
)

type Server struct {
	config   config.Config
	manager  *session.Manager
	index    []byte
	notFound []byte
}

func New(value config.Config) (*Server, error) {
	index, err := os.ReadFile(filepath.Join(value.PublicDir, "index.html"))
	if err != nil {
		return nil, err
	}
	notFound, err := os.ReadFile(filepath.Join(value.PublicDir, "404.html"))
	if err != nil {
		notFound = index
	}
	return &Server{
		config:   value,
		manager:  session.NewManager(value),
		index:    index,
		notFound: notFound,
	}, nil
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/readyz", s.serveReady)
	mux.HandleFunc("/metrics", s.serveMetrics)
	if s.config.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

func (s *Server) Shutdown() {
	s.manager.Shutdown()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != s.config.PublicHostname {
		s.serveNotFound(w)
		return
	}
	if r.URL.Path == "/" {
		s.serveRoot(w, r)
		return
	}
	if r.URL.Path == "/api/v1/session" || r.URL.Path == "/api/v1/up" || r.URL.Path == "/api/v1/down" {
		s.serveAPI(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.serveNotFound(w)
		return
	}
	s.serveStatic(w, r)
}

func (s *Server) serveRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.servePublicIndex(w, r.Method == http.MethodHead)
		return
	}
	profile := s.bridgeProfile(r)
	if profile == nil {
		s.servePublicIndex(w, false)
		return
	}
	clientIP, err := s.clientIP(r)
	if err != nil {
		s.servePublicIndex(w, false)
		return
	}
	token, err := s.manager.IssueBootstrap(profile, clientIP)
	if err != nil {
		s.servePublicIndex(w, false)
		return
	}
	page, err := bridge.Render(
		s.config.PublicHostname,
		token,
		s.config.Limits.CarrierBatchBytes)
	if err != nil {
		s.servePublicIndex(w, false)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", page.CSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Length", strconv.Itoa(len(page.Body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page.Body)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.Header.Get("Origin") != "https://"+s.config.PublicHostname {
		s.serveNotFound(w)
		return
	}
	clientIP, err := s.clientIP(r)
	if err != nil {
		s.serveNotFound(w)
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		s.serveNotFound(w)
		return
	}
	switch r.URL.Path {
	case "/api/v1/session":
		s.serveSession(w, r, token, clientIP)
	case "/api/v1/up":
		s.serveUp(w, r, token)
	case "/api/v1/down":
		s.serveDown(w, r, token)
	default:
		s.serveNotFound(w)
	}
}

func (s *Server) serveSession(w http.ResponseWriter, r *http.Request, token, clientIP string) {
	if r.Method == http.MethodDelete {
		if !emptyBody(r) || r.Header.Get("Content-Type") != "" {
			s.serveNotFound(w)
			return
		}
		if err := s.manager.CloseToken(token); err != nil {
			s.serveNotFound(w)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost || !binaryContentType(r.Header.Get("Content-Type")) {
		s.serveNotFound(w)
		return
	}
	body, err := readBody(w, r, s.config.Limits.MaxBodyBytes)
	if err != nil {
		s.serveNotFound(w)
		return
	}
	result, err := s.manager.Create(token, clientIP, body)
	if err != nil {
		if errors.Is(err, session.ErrLimit) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.serveNotFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Session-Token", result.Token)
	w.Header().Set("X-Down-Cursor", "0")
	w.Header().Set("Content-Length", strconv.Itoa(len(result.Welcome)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Welcome)
}

func (s *Server) serveUp(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost || !binaryContentType(r.Header.Get("Content-Type")) {
		s.serveNotFound(w)
		return
	}
	sequence, ok := canonicalUint(r.Header.Get("X-Up-Seq"))
	if !ok || sequence == 0 {
		s.serveNotFound(w)
		return
	}
	value, err := s.manager.Get(token)
	if err != nil {
		s.serveNotFound(w)
		return
	}
	body, err := readBody(w, r, s.config.Limits.MaxBodyBytes)
	if err != nil {
		s.serveNotFound(w)
		return
	}
	ack, err := value.ProcessUp(sequence, body)
	if err != nil {
		if errors.Is(err, session.ErrBackpressure) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.serveNotFound(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Up-Ack", strconv.FormatUint(ack, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveDown(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodPost || !emptyBody(r) || r.Header.Get("Content-Type") != "" {
		s.serveNotFound(w)
		return
	}
	cursor, ok := canonicalUint(r.Header.Get("X-Down-Cursor"))
	if !ok {
		s.serveNotFound(w)
		return
	}
	value, err := s.manager.Get(token)
	if err != nil {
		s.serveNotFound(w)
		return
	}
	body, next, err := value.Poll(r.Context(), cursor)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.serveNotFound(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Down-Cursor", strconv.FormatUint(next, 10))
	if len(body) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) bridgeProfile(r *http.Request) *config.Profile {
	query := r.URL.Query()
	values, exists := query["bridge"]
	if !exists || len(query) != 1 || len(values) != 1 || len(values[0]) != 43 {
		return nil
	}
	if r.URL.RawQuery != "bridge="+values[0] {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(values[0])
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != values[0] {
		return nil
	}
	return s.manager.MatchCapability(decoded)
}

func (s *Server) clientIP(r *http.Request) (string, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "", err
	}
	peer := net.ParseIP(host)
	if peer == nil || !peer.IsLoopback() {
		return "", errors.New("request did not arrive through loopback proxy")
	}
	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return peer.String(), nil
	}
	if strings.Contains(forwarded, ",") || strings.TrimSpace(forwarded) != forwarded {
		return "", errors.New("invalid forwarding address")
	}
	parsed := net.ParseIP(forwarded)
	if parsed == nil {
		return "", errors.New("invalid forwarding address")
	}
	return parsed.String(), nil
}

func (s *Server) servePublicIndex(w http.ResponseWriter, head bool) {
	s.publicHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.index)))
	w.WriteHeader(http.StatusOK)
	if !head {
		_, _ = w.Write(s.index)
	}
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	clean := filepath.Clean("/" + r.URL.Path)
	if clean != r.URL.Path || strings.Contains(clean, "\\") {
		s.serveNotFound(w)
		return
	}
	relative := strings.TrimPrefix(clean, "/")
	candidates := []string{relative}
	if relative == "favicon.ico" {
		candidates = []string{"favicon.svg"}
	}
	if filepath.Ext(relative) == "" {
		candidates = append(candidates, relative+".html")
	}
	for _, candidate := range candidates {
		path := filepath.Join(s.config.PublicDir, candidate)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		s.publicHeaders(w)
		http.ServeFile(w, r, path)
		return
	}
	s.serveNotFound(w)
}

func (s *Server) serveNotFound(w http.ResponseWriter) {
	s.publicHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.notFound)))
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(s.notFound)
}

func (s *Server) publicHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	for _, profile := range s.config.Profiles {
		connection, err := net.DialTimeout("tcp", profile.Backend, s.config.Timeouts.BackendDial.Value())
		if err != nil {
			http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = connection.Close()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ready\n")
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	metrics := s.manager.Metrics()
	capacity := s.manager.Capacity()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w,
		"tproxy_sessions_live %d\n"+
			"tproxy_streams_live %d\n"+
			"tproxy_backend_dials_in_flight %d\n"+
			"tproxy_pending_bytes %d\n"+
			"tproxy_pending_items %d\n"+
			"tproxy_sessions_created_total %d\n"+
			"tproxy_sessions_closed_total %d\n"+
			"tproxy_streams_opened_total %d\n"+
			"tproxy_streams_rejected_total %d\n"+
			"tproxy_backend_dial_failures_total %d\n"+
			"tproxy_bytes_up_total %d\n"+
			"tproxy_bytes_down_total %d\n"+
			"tproxy_limit_hits_total %d\n",
		capacity.Sessions, capacity.Streams, capacity.BackendDialsInFlight,
		capacity.PendingBytes, capacity.PendingItems,
		metrics.SessionsCreated, metrics.SessionsClosed,
		metrics.StreamsOpened, metrics.StreamsRejected,
		metrics.BackendDialFailures, metrics.BytesUp, metrics.BytesDown,
		metrics.LimitHits)
}

func readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, error) {
	reader := http.MaxBytesReader(w, r.Body, int64(limit))
	defer reader.Close()
	result, err := io.ReadAll(reader)
	if err != nil || len(result) == 0 {
		return nil, errors.New("invalid body")
	}
	return result, nil
}

func emptyBody(r *http.Request) bool {
	if r.ContentLength > 0 {
		return false
	}
	if r.Body == nil {
		return true
	}
	var single [1]byte
	read, _ := r.Body.Read(single[:])
	return read == 0
}

func binaryContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/octet-stream" && len(parameters) == 0
}

func bearerToken(value string) (string, bool) {
	if !strings.HasPrefix(value, "Bearer ") || strings.Count(value, " ") != 1 {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return token, err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}

func canonicalUint(value string) (uint64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') || strings.HasPrefix(value, "+") {
		return 0, false
	}
	result, err := strconv.ParseUint(value, 10, 64)
	return result, err == nil && strconv.FormatUint(result, 10) == value
}
