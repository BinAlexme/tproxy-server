package session

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/frame"
)

func TestSequenceRetryAndMismatch(t *testing.T) {
	manager, token, value := testSession(t)
	defer manager.Shutdown()

	first := frame.Encode(frame.Open, 1, nil)
	if ack, err := value.ProcessUp(1, first); err != nil || ack != 1 {
		t.Fatalf("first sequence failed: ack=%d error=%v", ack, err)
	}
	if ack, err := value.ProcessUp(1, append([]byte(nil), first...)); err != nil || ack != 1 {
		t.Fatalf("identical retry failed: ack=%d error=%v", ack, err)
	}
	if _, err := value.ProcessUp(1, frame.Encode(frame.Close, 1, nil)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("changed retry was accepted: %v", err)
	}
	if _, err := manager.Get(token); err == nil {
		deadline := time.Now().Add(time.Second)
		for err == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			_, err = manager.Get(token)
		}
		if err == nil {
			t.Fatal("protocol-failed session token remained active")
		}
	}
}

func TestRejectsStreamReuseAndWindowOverrun(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()

	batch := append(frame.Encode(frame.Open, 7, nil), frame.Encode(frame.Close, 7, nil)...)
	if _, err := value.ProcessUp(1, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := value.ProcessUp(2, frame.Encode(frame.Open, 7, nil)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("closed stream id was reused: %v", err)
	}

	manager2, _, value2 := testSession(t)
	defer manager2.Shutdown()
	tooLarge := bytes.Repeat([]byte{1}, frame.InitialStreamWindow+1)
	overrun := append(frame.Encode(frame.Open, 8, nil), frame.Encode(frame.Data, 8, tooLarge)...)
	if _, err := value2.ProcessUp(1, overrun); !errors.Is(err, ErrProtocol) {
		t.Fatalf("receive window overrun was accepted: %v", err)
	}
}

func TestPendingByteLimitIncludesBackendWrites(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()
	value.limits.MaxPendingPerSession = 4
	batch := append(frame.Encode(frame.Open, 9, nil), frame.Encode(frame.Data, 9, []byte("12345"))...)
	if _, err := value.ProcessUp(1, batch); !errors.Is(err, ErrLimit) {
		t.Fatalf("backend write exceeded the pending-byte limit: %v", err)
	}
}

func TestProcessPendingByteLimitIncludesBackendWrites(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()
	manager.config.Limits.MaxPendingGlobal = 4
	batch := append(frame.Encode(frame.Open, 10, nil), frame.Encode(frame.Data, 10, []byte("12345"))...)
	if _, err := value.ProcessUp(1, batch); !errors.Is(err, ErrLimit) {
		t.Fatalf("backend write exceeded the process pending-byte limit: %v", err)
	}
}

func TestProcessPendingItemLimitIncludesBackendWrites(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()
	manager.config.Limits.MaxPendingItemsGlobal = 1
	batch := append(frame.Encode(frame.Open, 15, nil), frame.Encode(frame.Data, 15, []byte{1})...)
	batch = append(batch, frame.Encode(frame.Open, 16, nil)...)
	batch = append(batch, frame.Encode(frame.Data, 16, []byte{2})...)
	if _, err := value.ProcessUp(1, batch); !errors.Is(err, ErrLimit) {
		t.Fatalf("backend writes exceeded the process pending-item limit: %v", err)
	}
}

func TestPendingItemLimitIncludesTinyWrites(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()
	value.limits.MaxPendingItemsPerSession = 1
	batch := append(frame.Encode(frame.Open, 11, nil), frame.Encode(frame.Data, 11, []byte{1})...)
	batch = append(batch, frame.Encode(frame.Open, 12, nil)...)
	batch = append(batch, frame.Encode(frame.Data, 12, []byte{2})...)
	if _, err := value.ProcessUp(1, batch); !errors.Is(err, ErrLimit) {
		t.Fatalf("tiny writes exceeded the pending-item limit: %v", err)
	}
}

func TestQueuedFramesChargeOverheadAndLimitBatchCount(t *testing.T) {
	value := config.Defaults()
	session := newSession(sessionOptions{
		profile:  &config.Profile{},
		limits:   value.Limits,
		timeouts: value.Timeouts,
		budget:   func(int, int) bool { return true },
	})
	session.mu.Lock()
	for id := uint32(1); id <= frame.MaxBatchFrames+7; id++ {
		if !session.queueFrameLocked(frame.Close, id, nil) {
			session.mu.Unlock()
			t.Fatal("could not queue bounded test frames")
		}
	}
	if session.pendingCost != (frame.MaxBatchFrames+7)*(frame.HeaderSize+queueItemCost) || session.pendingItems != frame.MaxBatchFrames+7 {
		session.mu.Unlock()
		t.Fatal("queued frame accounting omitted item overhead")
	}
	session.mu.Unlock()

	body, cursor, err := session.Poll(context.Background(), 0)
	if err != nil || cursor != 1 {
		t.Fatalf("poll failed: cursor=%d error=%v", cursor, err)
	}
	frames, err := frame.ParseAll(body, frame.MaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != frame.MaxBatchFrames {
		t.Fatalf("downlink returned %d frames", len(frames))
	}
	session.Close()
}

func TestConcurrentCarriersAreRejected(t *testing.T) {
	manager, _, value := testSession(t)
	defer manager.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, _, err := value.Poll(ctx, 0)
		first <- err
	}()
	waitFor(t, func() bool {
		value.mu.Lock()
		defer value.mu.Unlock()
		return value.downActive
	})
	if _, _, err := value.Poll(context.Background(), 0); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("concurrent downlink was accepted: %v", err)
	}
	value.mu.Lock()
	value.upActive = true
	value.mu.Unlock()
	if _, err := value.ProcessUp(1, frame.Encode(frame.Open, 13, nil)); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("concurrent uplink was accepted: %v", err)
	}
	value.mu.Lock()
	value.upActive = false
	value.mu.Unlock()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first downlink did not stop on cancellation: %v", err)
	}
}

func TestSessionCloseStopsBackendGoroutines(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	configuration := testConfig(listener.Addr().String())
	manager := NewManager(configuration)
	bootstrap, err := manager.IssueBootstrap(&configuration.Profiles[0], "198.51.100.10")
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(bootstrap, "198.51.100.10", frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.Session.ProcessUp(1, frame.Encode(frame.Open, 14, nil)); err != nil {
		t.Fatal(err)
	}
	var peer net.Conn
	select {
	case peer = <-accepted:
		defer peer.Close()
	case <-time.After(time.Second):
		t.Fatal("backend connection was not established")
	}
	created.Session.mu.Lock()
	backend := created.Session.streams[14].backend
	created.Session.mu.Unlock()

	shutdown := make(chan struct{})
	go func() {
		manager.Shutdown()
		close(shutdown)
	}()
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("manager shutdown stranded a backend goroutine")
	}
	select {
	case <-backend.finished:
	default:
		t.Fatal("backend lifecycle was not complete when shutdown returned")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.pendingGlobalCost != 0 || manager.pendingGlobalItems != 0 {
		t.Fatal("shutdown retained pending queue budget")
	}
}

func TestBackendEOFClosesOnlyItsStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()

	configuration := testConfig(listener.Addr().String())
	manager := NewManager(configuration)
	defer manager.Shutdown()
	bootstrap, err := manager.IssueBootstrap(&configuration.Profiles[0], "198.51.100.15")
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(bootstrap, "198.51.100.15", frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := created.Session.ProcessUp(1, frame.Encode(frame.Open, 17, nil)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		created.Session.mu.Lock()
		defer created.Session.mu.Unlock()
		_, live := created.Session.streams[17]
		return !live
	})
	body, _, err := created.Session.Poll(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := frame.ParseAll(body, frame.MaxPayload)
	if err != nil || len(frames) != 1 || frames[0].Type != frame.Close || frames[0].StreamID != 17 {
		t.Fatalf("backend EOF did not produce one stream close: frames=%v error=%v", frames, err)
	}
	if _, err := manager.Get(created.Token); err != nil {
		t.Fatal("backend EOF closed the parent session")
	}
}

func TestStreamCancellationWakesZeroCreditWaiter(t *testing.T) {
	configuration := testConfig("127.0.0.1:1")
	value := newSession(sessionOptions{
		profile:  &configuration.Profiles[0],
		limits:   configuration.Limits,
		timeouts: configuration.Timeouts,
		budget:   func(int, int) bool { return true },
	})
	backend := newBackendStream(value, 18, configuration.Profiles[0].Backend)
	value.streams[18] = &streamState{
		backend:      backend,
		creditNotify: make(chan struct{}, 1),
		writeNotify:  make(chan struct{}, 1),
	}
	result := make(chan bool, 1)
	go func() {
		_, ok := value.nextReadAllowance(18, backend.ctx.Done())
		result <- ok
	}()
	backend.close()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("zero-credit waiter resumed with allowance after close")
		}
	case <-time.After(time.Second):
		t.Fatal("zero-credit waiter remained blocked after stream cancellation")
	}
	value.Close()
}

func TestBootstrapLimitsExpiryAndConsumption(t *testing.T) {
	configuration := testConfig("127.0.0.1:1")
	configuration.Limits.MaxBootstrapsPerIP = 2
	configuration.Limits.MaxBootstrapsGlobal = 3
	configuration.Limits.NewBootstrapsBurst = 10
	configuration.Limits.NewBootstrapsPerMinute = 600
	manager := NewManager(configuration)
	defer manager.Shutdown()
	profile := &configuration.Profiles[0]
	first, err := manager.IssueBootstrap(profile, "198.51.100.11")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.11"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.11"); !errors.Is(err, ErrLimit) {
		t.Fatalf("per-IP bootstrap limit was not enforced: %v", err)
	}
	if _, err := manager.Create(first, "198.51.100.11", frame.Encode(frame.Hello, 0, []byte{1})); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.11"); err != nil {
		t.Fatalf("using a bootstrap did not release its per-IP slot: %v", err)
	}

	expiringConfig := testConfig("127.0.0.1:1")
	expiringConfig.Limits.MaxBootstrapsPerIP = 1
	expiringConfig.Timeouts.BootstrapLifetime = config.Duration(time.Millisecond)
	expiring := NewManager(expiringConfig)
	defer expiring.Shutdown()
	expired, err := expiring.IssueBootstrap(&expiringConfig.Profiles[0], "198.51.100.12")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if _, err := expiring.IssueBootstrap(&expiringConfig.Profiles[0], "198.51.100.12"); err != nil {
		t.Fatalf("expired bootstrap retained its slot: %v", err)
	}
	if _, err := expiring.Create(expired, "198.51.100.12", frame.Encode(frame.Hello, 0, []byte{1})); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("expired bootstrap was accepted: %v", err)
	}
}

func TestBootstrapRateAndGlobalEviction(t *testing.T) {
	configuration := testConfig("127.0.0.1:1")
	configuration.Limits.MaxBootstrapsPerIP = 10
	configuration.Limits.MaxBootstrapsGlobal = 2
	configuration.Limits.NewBootstrapsBurst = 2
	configuration.Limits.NewBootstrapsPerMinute = 1
	manager := NewManager(configuration)
	defer manager.Shutdown()
	profile := &configuration.Profiles[0]
	oldest, err := manager.IssueBootstrap(profile, "198.51.100.13")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.13"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.13"); !errors.Is(err, ErrLimit) {
		t.Fatalf("bootstrap creation rate was not enforced: %v", err)
	}
	if _, err := manager.IssueBootstrap(profile, "198.51.100.14"); err != nil {
		t.Fatalf("global bootstrap pool did not evict an unused entry: %v", err)
	}
	if _, err := manager.Create(oldest, "198.51.100.13", frame.Encode(frame.Hello, 0, []byte{1})); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("evicted bootstrap was accepted: %v", err)
	}
}

func testSession(t *testing.T) (*Manager, string, *Session) {
	t.Helper()
	value := testConfig("127.0.0.1:1")
	manager := NewManager(value)
	bootstrap, err := manager.IssueBootstrap(&value.Profiles[0], "198.51.100.9")
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(bootstrap, "198.51.100.9", frame.Encode(frame.Hello, 0, []byte{1}))
	if err != nil {
		t.Fatal(err)
	}
	return manager, created.Token, created.Session
}

func testConfig(backend string) config.Config {
	value := config.Defaults()
	value.Timeouts.BackendDial = config.Duration(20 * time.Millisecond)
	secret, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	value.Profiles = []config.Profile{{
		Name:       "default",
		Backend:    backend,
		Capability: config.DeriveCapability("proxy.example.com", secret),
	}}
	return value
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
