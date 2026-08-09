package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/frame"
)

var (
	ErrAuthentication = errors.New("authentication failed")
	ErrLimit          = errors.New("resource limit reached")
	ErrProtocol       = errors.New("protocol error")
	ErrConcurrent     = errors.New("concurrent request")
	ErrClosed         = errors.New("session closed")
)

type CreateResult struct {
	Token   string
	Welcome []byte
	Session *Session
}

type bootstrap struct {
	expires      time.Time
	profile      *config.Profile
	clientIP     string
	bodyDigest   [sha256.Size]byte
	sessionToken string
	session      *Session
	used         bool
}

type rateState struct {
	tokens float64
	last   time.Time
}

type Metrics struct {
	SessionsCreated uint64
	SessionsClosed  uint64
	StreamsOpened   uint64
	BytesUp         uint64
	BytesDown       uint64
	LimitHits       uint64
}

type Manager struct {
	config config.Config

	mu                 sync.Mutex
	bootstraps         map[[sha256.Size]byte]*bootstrap
	bootstrapsPerIP    map[string]int
	sessions           map[[sha256.Size]byte]*Session
	closedTokens       map[[sha256.Size]byte]time.Time
	sessionsPerIP      map[string]int
	rates              map[string]rateState
	bootstrapRates     map[string]rateState
	pendingGlobalCost  int64
	pendingGlobalItems int64
	closed             bool
	stop               chan struct{}
	done               chan struct{}

	sessionsCreated atomic.Uint64
	sessionsClosed  atomic.Uint64
	streamsOpened   atomic.Uint64
	bytesUp         atomic.Uint64
	bytesDown       atomic.Uint64
	limitHits       atomic.Uint64
}

func NewManager(value config.Config) *Manager {
	result := &Manager{
		config:          value,
		bootstraps:      make(map[[sha256.Size]byte]*bootstrap),
		bootstrapsPerIP: make(map[string]int),
		sessions:        make(map[[sha256.Size]byte]*Session),
		closedTokens:    make(map[[sha256.Size]byte]time.Time),
		sessionsPerIP:   make(map[string]int),
		rates:           make(map[string]rateState),
		bootstrapRates:  make(map[string]rateState),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	go result.cleanupLoop()
	return result
}

func (m *Manager) MatchCapability(value []byte) *config.Profile {
	if len(value) != sha256.Size {
		return nil
	}
	matched := -1
	for i := range m.config.Profiles {
		equal := subtle.ConstantTimeCompare(value, m.config.Profiles[i].Capability[:])
		if equal == 1 {
			matched = i
		}
	}
	if matched < 0 {
		return nil
	}
	return &m.config.Profiles[matched]
}

func (m *Manager) IssueBootstrap(profile *config.Profile, clientIP string) (string, error) {
	now := time.Now()
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", ErrClosed
	}
	m.removeExpiredBootstrapsLocked(now)
	if m.bootstrapsPerIP[clientIP] >= m.config.Limits.MaxBootstrapsPerIP || !allowRateLocked(
		m.bootstrapRates,
		clientIP,
		now,
		m.config.Limits.NewBootstrapsPerMinute,
		m.config.Limits.NewBootstrapsBurst) {
		m.limitHits.Add(1)
		return "", ErrLimit
	}
	if len(m.bootstraps) >= m.config.Limits.MaxBootstrapsGlobal &&
		!m.evictOldestUnusedBootstrapLocked() {
		m.limitHits.Add(1)
		return "", ErrLimit
	}
	m.bootstraps[hash] = &bootstrap{
		expires:  now.Add(m.config.Timeouts.BootstrapLifetime.Value()),
		profile:  profile,
		clientIP: clientIP,
	}
	m.bootstrapsPerIP[clientIP]++
	return token, nil
}

func (m *Manager) Create(token, clientIP string, body []byte) (CreateResult, error) {
	if err := frame.ParseHello(body); err != nil {
		return CreateResult{}, ErrProtocol
	}
	tokenHash, err := tokenHash(token)
	if err != nil {
		return CreateResult{}, ErrAuthentication
	}
	bodyDigest := sha256.Sum256(body)
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.bootstraps[tokenHash]
	if entry == nil || now.After(entry.expires) || entry.clientIP != clientIP {
		return CreateResult{}, ErrAuthentication
	}
	if entry.used {
		if subtle.ConstantTimeCompare(entry.bodyDigest[:], bodyDigest[:]) != 1 || entry.session == nil {
			return CreateResult{}, ErrAuthentication
		}
		return CreateResult{
			Token:   entry.sessionToken,
			Welcome: frame.Encode(frame.Welcome, 0, nil),
			Session: entry.session,
		}, nil
	}
	if m.closed || len(m.sessions) >= m.config.Limits.MaxSessionsGlobal || m.sessionsPerIP[clientIP] >= m.config.Limits.MaxSessionsPerIP {
		m.limitHits.Add(1)
		return CreateResult{}, ErrLimit
	}
	if !allowRateLocked(
		m.rates,
		clientIP,
		now,
		m.config.Limits.NewSessionsPerMinute,
		m.config.Limits.NewSessionsBurst) {
		m.limitHits.Add(1)
		return CreateResult{}, ErrLimit
	}
	sessionToken, sessionHash, err := newToken()
	if err != nil {
		return CreateResult{}, err
	}
	limits := m.config.Limits
	if entry.profile.Limits.MaxStreamsPerSession != 0 {
		limits.MaxStreamsPerSession = entry.profile.Limits.MaxStreamsPerSession
	}
	if entry.profile.Limits.MaxPendingPerSession != 0 {
		limits.MaxPendingPerSession = entry.profile.Limits.MaxPendingPerSession
	}
	created := newSession(sessionOptions{
		profile:    entry.profile,
		clientIP:   clientIP,
		limits:     limits,
		timeouts:   m.config.Timeouts,
		budget:     m.changePendingBudget,
		onFinished: m.sessionFinished,
		onStream:   func() { m.streamsOpened.Add(1) },
		onUp:       func(count int) { m.bytesUp.Add(uint64(count)) },
		onDown:     func(count int) { m.bytesDown.Add(uint64(count)) },
	})
	m.sessions[sessionHash] = created
	m.sessionsPerIP[clientIP]++
	m.releaseUnusedBootstrapLocked(entry)
	entry.used = true
	entry.bodyDigest = bodyDigest
	entry.sessionToken = sessionToken
	entry.session = created
	m.sessionsCreated.Add(1)
	return CreateResult{
		Token:   sessionToken,
		Welcome: frame.Encode(frame.Welcome, 0, nil),
		Session: created,
	}, nil
}

func (m *Manager) Get(token string) (*Session, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return nil, ErrAuthentication
	}
	m.mu.Lock()
	result := m.sessions[hash]
	m.mu.Unlock()
	if result == nil {
		return nil, ErrAuthentication
	}
	return result, nil
}

func (m *Manager) CloseToken(token string) error {
	hash, err := tokenHash(token)
	if err != nil {
		return ErrAuthentication
	}
	m.mu.Lock()
	value := m.sessions[hash]
	_, closed := m.closedTokens[hash]
	m.mu.Unlock()
	if value == nil {
		if closed {
			return nil
		}
		return ErrAuthentication
	}
	value.Close()
	return nil
}

func (m *Manager) Metrics() Metrics {
	return Metrics{
		SessionsCreated: m.sessionsCreated.Load(),
		SessionsClosed:  m.sessionsClosed.Load(),
		StreamsOpened:   m.streamsOpened.Load(),
		BytesUp:         m.bytesUp.Load(),
		BytesDown:       m.bytesDown.Load(),
		LimitHits:       m.limitHits.Load(),
	}
}

func (m *Manager) LiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.done
		return
	}
	m.closed = true
	close(m.stop)
	sessions := make([]*Session, 0, len(m.sessions))
	for _, value := range m.sessions {
		sessions = append(sessions, value)
	}
	m.mu.Unlock()
	for _, value := range sessions {
		value.Close()
	}
	for _, value := range sessions {
		value.wait()
	}
	<-m.done
}

func (m *Manager) sessionFinished(value *Session) {
	m.mu.Lock()
	for hash, current := range m.sessions {
		if current == value {
			delete(m.sessions, hash)
			if len(m.closedTokens) >= m.config.Limits.MaxSessionsGlobal*16 {
				var oldestHash [sha256.Size]byte
				var oldest time.Time
				for candidate, expires := range m.closedTokens {
					if oldest.IsZero() || expires.Before(oldest) {
						oldestHash = candidate
						oldest = expires
					}
				}
				delete(m.closedTokens, oldestHash)
			}
			m.closedTokens[hash] = time.Now().Add(m.config.Timeouts.BootstrapLifetime.Value())
			m.sessionsPerIP[value.clientIP]--
			if m.sessionsPerIP[value.clientIP] == 0 {
				delete(m.sessionsPerIP, value.clientIP)
			}
			break
		}
	}
	for hash, current := range m.bootstraps {
		if current.session == value {
			m.removeBootstrapLocked(hash, current)
		}
	}
	m.mu.Unlock()
	m.sessionsClosed.Add(1)
}

func (m *Manager) changePendingBudget(costDelta, itemDelta int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if costDelta > 0 && (m.pendingGlobalCost > int64(m.config.Limits.MaxPendingGlobal-costDelta)) {
		m.limitHits.Add(1)
		return false
	}
	if itemDelta > 0 && (m.pendingGlobalItems > int64(m.config.Limits.MaxPendingItemsGlobal-itemDelta)) {
		m.limitHits.Add(1)
		return false
	}
	m.pendingGlobalCost += int64(costDelta)
	m.pendingGlobalItems += int64(itemDelta)
	if m.pendingGlobalCost < 0 || m.pendingGlobalItems < 0 {
		panic("negative global pending budget")
	}
	return true
}

func (m *Manager) releaseUnusedBootstrapLocked(value *bootstrap) {
	if value.used {
		return
	}
	m.bootstrapsPerIP[value.clientIP]--
	if m.bootstrapsPerIP[value.clientIP] <= 0 {
		delete(m.bootstrapsPerIP, value.clientIP)
	}
}

func (m *Manager) removeBootstrapLocked(
	hash [sha256.Size]byte,
	value *bootstrap) {
	m.releaseUnusedBootstrapLocked(value)
	delete(m.bootstraps, hash)
}

func (m *Manager) evictOldestUnusedBootstrapLocked() bool {
	var oldestHash [sha256.Size]byte
	var oldest *bootstrap
	for hash, value := range m.bootstraps {
		if value.used || (oldest != nil && !value.expires.Before(oldest.expires)) {
			continue
		}
		oldestHash = hash
		oldest = value
	}
	if oldest == nil {
		return false
	}
	m.removeBootstrapLocked(oldestHash, oldest)
	return true
}

func allowRateLocked(
	states map[string]rateState,
	clientIP string,
	now time.Time,
	perMinute int,
	burstLimit int) bool {
	state := states[clientIP]
	burst := float64(burstLimit)
	if state.last.IsZero() {
		state.tokens = burst
		state.last = now
	} else {
		perSecond := float64(perMinute) / 60
		state.tokens += now.Sub(state.last).Seconds() * perSecond
		if state.tokens > burst {
			state.tokens = burst
		}
		state.last = now
	}
	if state.tokens < 1 {
		states[clientIP] = state
		return false
	}
	state.tokens--
	states[clientIP] = state
	return true
}

func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		close(m.done)
	}()
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			m.mu.Lock()
			m.removeExpiredBootstrapsLocked(now)
			sessions := make([]*Session, 0, len(m.sessions))
			for _, value := range m.sessions {
				sessions = append(sessions, value)
			}
			m.mu.Unlock()
			for _, value := range sessions {
				if now.Sub(value.LastActivity()) > m.config.Timeouts.ReconnectGrace.Value() {
					value.Close()
				}
			}
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) removeExpiredBootstrapsLocked(now time.Time) {
	for hash, value := range m.bootstraps {
		if now.After(value.expires) {
			m.removeBootstrapLocked(hash, value)
		}
	}
	for hash, expires := range m.closedTokens {
		if now.After(expires) {
			delete(m.closedTokens, hash)
		}
	}
	removeIdleRatesLocked(
		m.rates,
		m.sessionsPerIP,
		now,
		m.config.Limits.NewSessionsPerMinute,
		m.config.Limits.NewSessionsBurst)
	removeIdleRatesLocked(
		m.bootstrapRates,
		m.bootstrapsPerIP,
		now,
		m.config.Limits.NewBootstrapsPerMinute,
		m.config.Limits.NewBootstrapsBurst)
}

func removeIdleRatesLocked(
	states map[string]rateState,
	active map[string]int,
	now time.Time,
	perMinute int,
	burst int) {
	refill := time.Duration(
		float64(time.Minute) * float64(burst) / float64(perMinute))
	for clientIP, state := range states {
		if active[clientIP] == 0 && now.Sub(state.last) > refill {
			delete(states, clientIP)
		}
	}
}

func newToken() (string, [sha256.Size]byte, error) {
	var input [32]byte
	if _, err := rand.Read(input[:]); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("random token: %w", err)
	}
	text := base64.RawURLEncoding.EncodeToString(input[:])
	return text, sha256.Sum256(input[:]), nil
}

func tokenHash(token string) ([sha256.Size]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return [sha256.Size]byte{}, ErrAuthentication
	}
	return sha256.Sum256(decoded), nil
}
