package server

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/session"
)

func (s *Server) bridgeProfile(r *http.Request) *config.Profile {
	if r.Method != http.MethodGet || r.URL.EscapedPath() != "/" ||
		len(r.URL.RawQuery) != len("bridge=")+43 ||
		!strings.HasPrefix(r.URL.RawQuery, "bridge=") {
		return nil
	}
	text := strings.TrimPrefix(r.URL.RawQuery, "bridge=")
	value, err := base64.RawURLEncoding.Strict().DecodeString(text)
	if err != nil {
		return nil
	}
	return s.manager.MatchCapability(value)
}

// Inspect metadata before interpreting credential syntax. Parsing only the
// canonical Authorization or bridge fields would leak real secrets in duplicate
// headers, malformed queries, cookies, referrers, or wrong-path requests. Bodies
// and trailers are deliberately not read: public uploads must remain streaming.
func (s *Server) hasInternalSecret(r *http.Request) bool {
	if s.config.LegacyTokenDrain && legacyCarrierCredential(r) {
		return true
	}
	if s.containsSecret(r.URL.String()) || s.containsSecret(r.Host) {
		return true
	}
	for name, values := range r.Header {
		if s.containsSecret(name) {
			return true
		}
		for _, value := range values {
			if s.containsSecret(value) {
				return true
			}
		}
	}
	return false
}

func legacyCarrierCredential(r *http.Request) bool {
	for _, value := range r.Header.Values("Authorization") {
		if _, ok := bearerToken(value); ok {
			return true
		}
	}
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(value, ",") {
			token, _, _, ok := webSocketCredentials(strings.TrimSpace(protocol))
			if ok {
				if _, ok := bearerToken("Bearer " + token); ok {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) containsSecret(text string) bool {
	// Decode individual escapes so a malformed escape elsewhere cannot hide a
	// capability. Do not parse/re-encode the public query: even malformed query
	// strings belong to the application when they carry no authentic secret.
	if strings.Contains(text, "%") {
		var decoded strings.Builder
		for i := 0; i < len(text); i++ {
			if text[i] == '%' && i+2 < len(text) {
				if value, err := url.PathUnescape(text[i : i+3]); err == nil {
					decoded.WriteString(value)
					i += 2
					continue
				}
			}
			decoded.WriteByte(text[i])
		}
		text = decoded.String()
	}
	start := 0
	for i := 0; i <= len(text); i++ {
		if i < len(text) && base64Byte(text[i]) {
			continue
		}
		for ; start+43 <= i; start++ {
			candidate := text[start : start+43]
			if s.manager.ClassifyToken(candidate) != session.TokenExternal {
				return true
			}
			value, err := base64.RawURLEncoding.DecodeString(candidate)
			if err == nil && s.manager.MatchCapability(value) != nil {
				return true
			}
		}
		start = i + 1
	}
	return false
}

func base64Byte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '_'
}
