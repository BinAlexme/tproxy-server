package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/telegramdesktop/tproxy-server/internal/config"
	"github.com/telegramdesktop/tproxy-server/internal/frame"
)

const queueItemCost = 256

type sessionOptions struct {
	profile    *config.Profile
	clientIP   string
	limits     config.Limits
	timeouts   config.Timeouts
	budget     func(int, int) bool
	onFinished func(*Session)
	onStream   func()
	onUp       func(int)
	onDown     func(int)
}

type streamState struct {
	backend           *backendStream
	receiveWindow     uint32
	sendCredit        uint64
	pendingWriteBytes int
	pendingWriteCost  int
	pendingWriteItems int
	creditNotify      chan struct{}
	writes            [][]byte
	writeNotify       chan struct{}
}

type streamSnapshot struct {
	receiveWindow uint32
	sendCredit    uint64
}

type queuedFrame struct {
	encoded  []byte
	typeCode frame.Type
	streamID uint32
	cost     int
}

type downBatch struct {
	body  []byte
	cost  int
	items int
}

type Session struct {
	profile  *config.Profile
	clientIP string
	limits   config.Limits
	timeouts config.Timeouts
	budget   func(int, int) bool

	mu             sync.Mutex
	streams        map[uint32]*streamState
	closedStreams  map[uint32]struct{}
	closedOrder    []uint32
	closedStart    int
	pendingFrames  []queuedFrame
	pendingCost    int
	pendingItems   int
	unacked        []byte
	unackedCost    int
	unackedItems   int
	unackedBase    uint64
	downCursor     uint64
	lastUpSequence uint64
	lastUpDigest   [sha256.Size]byte
	upActive       bool
	downActive     bool
	closed         bool
	lastActivity   time.Time
	notify         chan struct{}
	done           chan struct{}
	finishOnce     sync.Once
	backendWG      sync.WaitGroup
	onFinished     func(*Session)
	onStream       func()
	onUp           func(int)
	onDown         func(int)
}

func newSession(options sessionOptions) *Session {
	return &Session{
		profile:       options.profile,
		clientIP:      options.clientIP,
		limits:        options.limits,
		timeouts:      options.timeouts,
		budget:        options.budget,
		streams:       make(map[uint32]*streamState),
		closedStreams: make(map[uint32]struct{}),
		lastActivity:  time.Now(),
		notify:        make(chan struct{}, 1),
		done:          make(chan struct{}),
		onFinished:    options.onFinished,
		onStream:      options.onStream,
		onUp:          options.onUp,
		onDown:        options.onDown,
	}
}

func (s *Session) ProcessUp(sequence uint64, body []byte) (uint64, error) {
	digest := sha256.Sum256(body)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	s.lastActivity = time.Now()
	if sequence == s.lastUpSequence && sequence != 0 {
		match := bytes.Equal(digest[:], s.lastUpDigest[:])
		s.mu.Unlock()
		if !match {
			s.protocolFailure()
			return 0, ErrProtocol
		}
		return sequence, nil
	}
	if sequence != s.lastUpSequence+1 || sequence == 0 {
		s.mu.Unlock()
		s.protocolFailure()
		return 0, ErrProtocol
	}
	if s.upActive {
		s.mu.Unlock()
		return 0, ErrConcurrent
	}
	s.upActive = true
	s.mu.Unlock()

	frames, err := frame.ParseAll(body, s.limits.MaxFramePayload)
	if err == nil {
		for _, value := range frames {
			if shapeErr := frame.ValidateClientShape(value); shapeErr != nil {
				err = shapeErr
				break
			}
		}
	}

	s.mu.Lock()
	s.upActive = false
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if err != nil || !s.validateBatchLocked(frames) {
		s.mu.Unlock()
		s.protocolFailure()
		return 0, ErrProtocol
	}
	opened, closed, applied := s.applyBatchLocked(frames)
	if !applied {
		s.mu.Unlock()
		for _, value := range closed {
			value.close()
		}
		for _, value := range opened {
			value.close()
		}
		return 0, ErrLimit
	}
	s.backendWG.Add(len(opened))
	s.lastUpSequence = sequence
	s.lastUpDigest = digest
	s.mu.Unlock()

	for _, value := range closed {
		value.close()
	}
	for _, value := range opened {
		go s.runBackend(value)
	}
	if s.onUp != nil {
		s.onUp(len(body))
	}
	return sequence, nil
}

func (s *Session) Poll(ctx context.Context, cursor uint64) ([]byte, uint64, error) {
	deadline := time.NewTimer(s.timeouts.LongPoll.Value())
	defer deadline.Stop()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, cursor, ErrClosed
	}
	if s.downActive {
		s.mu.Unlock()
		return nil, cursor, ErrConcurrent
	}
	s.lastActivity = time.Now()
	if len(s.unacked) != 0 {
		if cursor == s.unackedBase {
			result := s.unacked
			next := s.downCursor
			s.mu.Unlock()
			return result, next, nil
		}
		if cursor != s.downCursor {
			s.mu.Unlock()
			s.protocolFailure()
			return nil, cursor, ErrProtocol
		}
		s.releasePendingLocked(s.unackedCost, s.unackedItems)
		s.unacked = nil
		s.unackedCost = 0
		s.unackedItems = 0
	} else if cursor != s.downCursor {
		s.mu.Unlock()
		s.protocolFailure()
		return nil, cursor, ErrProtocol
	}
	s.downActive = true
	for {
		if len(s.pendingFrames) != 0 {
			batch := s.takeDownBatchLocked()
			s.downCursor++
			s.unackedBase = cursor
			s.unacked = batch.body
			s.unackedCost = batch.cost
			s.unackedItems = batch.items
			s.downActive = false
			next := s.downCursor
			s.mu.Unlock()
			if s.onDown != nil {
				s.onDown(len(batch.body))
			}
			return batch.body, next, nil
		}
		if s.closed {
			s.downActive = false
			s.mu.Unlock()
			return nil, cursor, ErrClosed
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.downActive = false
			s.mu.Unlock()
			return nil, cursor, ctx.Err()
		case <-deadline.C:
			s.mu.Lock()
			if len(s.pendingFrames) != 0 {
				continue
			}
			s.downActive = false
			s.lastActivity = time.Now()
			s.mu.Unlock()
			return nil, cursor, nil
		case <-s.notify:
			s.mu.Lock()
		}
	}
}

func (s *Session) LastActivity() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActivity
}

func (s *Session) Close() {
	s.mu.Lock()
	s.closeLocked()
	s.mu.Unlock()
}

func (s *Session) wait() {
	s.backendWG.Wait()
}

func (s *Session) validateBatchLocked(values []frame.Frame) bool {
	live := make(map[uint32]streamSnapshot, len(s.streams))
	for id, value := range s.streams {
		live[id] = streamSnapshot{
			receiveWindow: value.receiveWindow,
			sendCredit:    value.sendCredit,
		}
	}
	closed := make(map[uint32]struct{})
	for _, value := range values {
		if value.StreamID == 0 {
			if value.Type != frame.Pong {
				return false
			}
			continue
		}
		state, exists := live[value.StreamID]
		_, wasClosedBefore := s.closedStreams[value.StreamID]
		_, closedInBatch := closed[value.StreamID]
		wasClosed := wasClosedBefore || closedInBatch
		switch value.Type {
		case frame.Open:
			if exists || wasClosed || len(live) >= s.limits.MaxStreamsPerSession {
				return false
			}
			live[value.StreamID] = streamSnapshot{
				receiveWindow: frame.InitialStreamWindow,
				sendCredit:    frame.InitialStreamWindow,
			}
		case frame.Data:
			if wasClosed {
				continue
			}
			if !exists || uint32(len(value.Payload)) > state.receiveWindow {
				return false
			}
			state.receiveWindow -= uint32(len(value.Payload))
			live[value.StreamID] = state
		case frame.Window:
			if wasClosed {
				continue
			}
			if !exists {
				return false
			}
			amount, _ := frame.WindowAmount(value.Payload)
			state.sendCredit += uint64(amount)
			if state.sendCredit > math.MaxUint32 {
				state.sendCredit = math.MaxUint32
			}
			live[value.StreamID] = state
		case frame.Close:
			if wasClosed {
				continue
			}
			if !exists {
				return false
			}
			delete(live, value.StreamID)
			closed[value.StreamID] = struct{}{}
		default:
			return false
		}
	}
	return true
}

func (s *Session) applyBatchLocked(values []frame.Frame) ([]*backendStream, []*backendStream, bool) {
	opened := make([]*backendStream, 0)
	closed := make([]*backendStream, 0)
	for _, value := range values {
		if value.StreamID == 0 {
			continue
		}
		state := s.streams[value.StreamID]
		_, wasClosed := s.closedStreams[value.StreamID]
		switch value.Type {
		case frame.Open:
			backend := newBackendStream(s, value.StreamID, s.profile.Backend)
			state = &streamState{
				backend:       backend,
				receiveWindow: frame.InitialStreamWindow,
				sendCredit:    frame.InitialStreamWindow,
				creditNotify:  make(chan struct{}, 1),
				writeNotify:   make(chan struct{}, 1),
			}
			s.streams[value.StreamID] = state
			opened = append(opened, backend)
			if s.onStream != nil {
				s.onStream()
			}
		case frame.Data:
			if wasClosed {
				continue
			}
			if !s.queueBackendWriteLocked(state, value.Payload) {
				s.closeLocked()
				return opened, closed, false
			}
			state.receiveWindow -= uint32(len(value.Payload))
			signal(state.writeNotify)
		case frame.Window:
			if wasClosed {
				continue
			}
			amount, _ := frame.WindowAmount(value.Payload)
			state.sendCredit += uint64(amount)
			if state.sendCredit > math.MaxUint32 {
				state.sendCredit = math.MaxUint32
			}
			signal(state.creditNotify)
		case frame.Close:
			if wasClosed {
				continue
			}
			s.releaseStreamWritesLocked(state)
			delete(s.streams, value.StreamID)
			s.rememberClosedLocked(value.StreamID)
			closed = append(closed, state.backend)
		}
	}
	return opened, closed, true
}

func (s *Session) queueBackendWriteLocked(state *streamState, payload []byte) bool {
	cost := len(payload) + queueItemCost
	items := 1
	coalesce := len(state.writes) != 0 && len(state.writes[len(state.writes)-1])+len(payload) <= frame.DataChunk
	if coalesce {
		cost = len(payload)
		items = 0
	}
	if !s.reservePendingLocked(cost, items) {
		return false
	}
	if coalesce {
		last := len(state.writes) - 1
		state.writes[last] = append(state.writes[last], payload...)
	} else {
		state.writes = append(state.writes, append([]byte(nil), payload...))
	}
	state.pendingWriteBytes += len(payload)
	state.pendingWriteCost += cost
	state.pendingWriteItems += items
	return true
}

func (s *Session) nextWrite(id uint32, done <-chan struct{}) ([]byte, bool) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, false
		}
		state := s.streams[id]
		if state == nil {
			s.mu.Unlock()
			return nil, false
		}
		if len(state.writes) != 0 {
			result := state.writes[0]
			state.writes[0] = nil
			state.writes = state.writes[1:]
			s.mu.Unlock()
			return result, true
		}
		notify := state.writeNotify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-s.done:
			return nil, false
		case <-done:
			return nil, false
		}
	}
}

func (s *Session) backendDrained(id uint32, amount int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.streams[id]
	if s.closed || state == nil || amount <= 0 || amount > state.pendingWriteBytes || amount > state.pendingWriteCost || uint64(state.receiveWindow)+uint64(amount) > frame.InitialStreamWindow {
		return false
	}
	state.pendingWriteBytes -= amount
	state.pendingWriteCost -= amount
	s.releasePendingLocked(amount, 0)
	state.receiveWindow += uint32(amount)
	if !s.queueFrameLocked(frame.Window, id, frame.WindowPayload(uint32(amount))) {
		s.closeLocked()
		return false
	}
	return true
}

func (s *Session) backendWriteFinished(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.streams[id]
	if s.closed || state == nil || state.pendingWriteItems == 0 || state.pendingWriteCost < queueItemCost {
		return false
	}
	state.pendingWriteItems--
	state.pendingWriteCost -= queueItemCost
	s.releasePendingLocked(queueItemCost, 1)
	return true
}

func (s *Session) nextReadAllowance(id uint32, done <-chan struct{}) (int, bool) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, false
		}
		state := s.streams[id]
		if state == nil {
			s.mu.Unlock()
			return 0, false
		}
		if state.sendCredit != 0 {
			result := int(state.sendCredit)
			if result > frame.DataChunk {
				result = frame.DataChunk
			}
			s.mu.Unlock()
			return result, true
		}
		notify := state.creditNotify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-s.done:
			return 0, false
		case <-done:
			return 0, false
		}
	}
}

func (s *Session) backendData(id uint32, data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.streams[id]
	if s.closed || state == nil || len(data) == 0 || uint64(len(data)) > state.sendCredit {
		return false
	}
	state.sendCredit -= uint64(len(data))
	if !s.queueFrameLocked(frame.Data, id, data) {
		s.closeLocked()
		return false
	}
	return true
}

func (s *Session) backendClosed(id uint32, backend *backendStream) {
	s.mu.Lock()
	state := s.streams[id]
	if !s.closed && state != nil && state.backend == backend {
		s.releaseStreamWritesLocked(state)
		delete(s.streams, id)
		s.rememberClosedLocked(id)
		if !s.queueFrameLocked(frame.Close, id, nil) {
			s.closeLocked()
		}
	}
	s.mu.Unlock()
	backend.close()
}

func (s *Session) queueFrameLocked(t frame.Type, id uint32, payload []byte) bool {
	if len(s.pendingFrames) != 0 {
		last := &s.pendingFrames[len(s.pendingFrames)-1]
		if last.typeCode == frame.Window && t == frame.Window && last.streamID == id {
			previous, _ := frame.WindowAmount(last.encoded[frame.HeaderSize:])
			amount, _ := frame.WindowAmount(payload)
			total := uint64(previous) + uint64(amount)
			if total <= math.MaxUint32 {
				binary.BigEndian.PutUint32(last.encoded[frame.HeaderSize:], uint32(total))
				signal(s.notify)
				return true
			}
		}
		if last.typeCode == frame.Data && t == frame.Data && last.streamID == id && len(last.encoded)-frame.HeaderSize+len(payload) <= s.limits.MaxFramePayload {
			if !s.reservePendingLocked(len(payload), 0) {
				return false
			}
			last.encoded = append(last.encoded, payload...)
			last.cost += len(payload)
			binary.BigEndian.PutUint32(last.encoded[4:8], uint32(len(last.encoded)-frame.HeaderSize))
			signal(s.notify)
			return true
		}
	}
	encoded := frame.Encode(t, id, payload)
	cost := len(encoded) + queueItemCost
	if !s.reservePendingLocked(cost, 1) {
		return false
	}
	s.pendingFrames = append(s.pendingFrames, queuedFrame{
		encoded:  encoded,
		typeCode: t,
		streamID: id,
		cost:     cost,
	})
	signal(s.notify)
	return true
}

func (s *Session) reservePendingLocked(cost, items int) bool {
	if cost <= 0 || items < 0 || s.pendingCost > s.limits.MaxPendingPerSession-cost || s.pendingItems > s.limits.MaxPendingItemsPerSession-items {
		return false
	}
	if s.budget != nil && !s.budget(cost, items) {
		return false
	}
	s.pendingCost += cost
	s.pendingItems += items
	return true
}

func (s *Session) takeDownBatchLocked() downBatch {
	size := 0
	cost := 0
	count := 0
	for count < len(s.pendingFrames) && count < frame.MaxBatchFrames {
		next := len(s.pendingFrames[count].encoded)
		if count != 0 && size+next > s.limits.CarrierBatchBytes {
			break
		}
		size += next
		cost += s.pendingFrames[count].cost
		count++
	}
	result := make([]byte, 0, size)
	for index := 0; index != count; index++ {
		result = append(result, s.pendingFrames[index].encoded...)
		s.pendingFrames[index] = queuedFrame{}
	}
	s.pendingFrames = s.pendingFrames[count:]
	if len(s.pendingFrames) == 0 {
		s.pendingFrames = nil
	}
	return downBatch{body: result, cost: cost, items: count}
}

func (s *Session) rememberClosedLocked(id uint32) {
	if _, exists := s.closedStreams[id]; exists {
		return
	}
	s.closedStreams[id] = struct{}{}
	if len(s.closedOrder) < s.limits.MaxClosedStreamIDs {
		s.closedOrder = append(s.closedOrder, id)
		return
	}
	old := s.closedOrder[s.closedStart]
	delete(s.closedStreams, old)
	s.closedOrder[s.closedStart] = id
	s.closedStart = (s.closedStart + 1) % len(s.closedOrder)
}

func (s *Session) releaseStreamWritesLocked(state *streamState) {
	if state.pendingWriteCost != 0 || state.pendingWriteItems != 0 {
		s.releasePendingLocked(state.pendingWriteCost, state.pendingWriteItems)
	}
	state.pendingWriteBytes = 0
	state.pendingWriteCost = 0
	state.pendingWriteItems = 0
	state.writes = nil
}

func (s *Session) releasePendingLocked(cost, items int) {
	if cost == 0 && items == 0 {
		return
	}
	if cost < 0 || items < 0 || cost > s.pendingCost || items > s.pendingItems {
		panic("invalid session pending budget release")
	}
	s.pendingCost -= cost
	s.pendingItems -= items
	if s.budget != nil {
		s.budget(-cost, -items)
	}
}

func (s *Session) protocolFailure() {
	s.mu.Lock()
	s.closeLocked()
	s.mu.Unlock()
}

func (s *Session) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	for _, value := range s.streams {
		value.backend.close()
	}
	s.streams = nil
	if s.pendingCost != 0 || s.pendingItems != 0 {
		if s.budget != nil {
			s.budget(-s.pendingCost, -s.pendingItems)
		}
		s.pendingCost = 0
		s.pendingItems = 0
	}
	s.pendingFrames = nil
	s.unacked = nil
	s.unackedCost = 0
	s.unackedItems = 0
	signal(s.notify)
	s.finishOnce.Do(func() {
		if s.onFinished != nil {
			go s.onFinished(s)
		}
	})
}

func (s *Session) runBackend(value *backendStream) {
	defer s.backendWG.Done()
	value.run()
}

func signal(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

type backendStream struct {
	session   *Session
	id        uint32
	address   string
	ctx       context.Context
	cancel    context.CancelFunc
	connMu    sync.Mutex
	conn      net.Conn
	closeOnce sync.Once
	finished  chan struct{}
}

func newBackendStream(session *Session, id uint32, address string) *backendStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &backendStream{
		session:  session,
		id:       id,
		address:  address,
		ctx:      ctx,
		cancel:   cancel,
		finished: make(chan struct{}),
	}
}

func (s *backendStream) run() {
	defer close(s.finished)
	defer s.session.backendClosed(s.id, s)

	dialer := net.Dialer{Timeout: s.session.timeouts.BackendDial.Value()}
	connection, err := dialer.DialContext(s.ctx, "tcp", s.address)
	if err != nil {
		return
	}
	s.connMu.Lock()
	if s.ctx.Err() != nil {
		s.connMu.Unlock()
		_ = connection.Close()
		return
	}
	s.conn = connection
	s.connMu.Unlock()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		s.writeLoop(connection)
		s.close()
	}()
	s.readLoop(connection)
	s.close()
	<-writeDone
}

func (s *backendStream) writeLoop(connection net.Conn) {
	for {
		data, ok := s.session.nextWrite(s.id, s.ctx.Done())
		if !ok {
			return
		}
		for len(data) != 0 {
			written, err := connection.Write(data)
			if written > 0 {
				if !s.session.backendDrained(s.id, written) {
					return
				}
				data = data[written:]
			}
			if err != nil || written == 0 {
				return
			}
		}
		if !s.session.backendWriteFinished(s.id) {
			return
		}
	}
}

func (s *backendStream) readLoop(connection net.Conn) {
	buffer := make([]byte, frame.DataChunk)
	for {
		allowance, ok := s.session.nextReadAllowance(s.id, s.ctx.Done())
		if !ok {
			return
		}
		read, err := connection.Read(buffer[:allowance])
		if read > 0 && !s.session.backendData(s.id, buffer[:read]) {
			return
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && s.ctx.Err() == nil {
				return
			}
			return
		}
		if read == 0 {
			return
		}
	}
}

func (s *backendStream) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.connMu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
			s.conn = nil
		}
		s.connMu.Unlock()
	})
}
