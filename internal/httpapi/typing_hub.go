package httpapi

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultTypingTTL = 7 * time.Second
	minTypingTTL     = 1 * time.Second
	maxTypingTTL     = 30 * time.Second
)

type roomTypingEvent struct {
	Type      string    `json:"type"`
	RoomID    string    `json:"room_id"`
	ActorID   string    `json:"actor_id"`
	State     string    `json:"state"`
	TTLMS     int       `json:"ttl_ms,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type typingHub struct {
	mu sync.Mutex

	nextSubID int64
	buffer    int

	subsByRoom   map[string]map[int64]*typingSubscription
	activeByRoom map[string]map[string]*typingPresence
}

type typingSubscription struct {
	hub     *typingHub
	roomID  string
	id      int64
	ch      chan roomTypingEvent
	once    sync.Once
	dropped atomic.Bool
}

type typingPresence struct {
	event roomTypingEvent
	timer *time.Timer
}

func newTypingHub(buffer int) *typingHub {
	if buffer <= 0 {
		buffer = 32
	}
	return &typingHub{
		buffer:       buffer,
		subsByRoom:   make(map[string]map[int64]*typingSubscription),
		activeByRoom: make(map[string]map[string]*typingPresence),
	}
}

func (h *typingHub) Subscribe(roomID string) *typingSubscription {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		ch := make(chan roomTypingEvent)
		close(ch)
		return &typingSubscription{ch: ch}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subscribeLocked(roomID)
}

func (h *typingHub) SubscribeWithSnapshot(roomID string, now time.Time) (*typingSubscription, []roomTypingEvent) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		ch := make(chan roomTypingEvent)
		close(ch)
		return &typingSubscription{ch: ch}, nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.pruneExpiredLocked(roomID, now)
	return h.subscribeLocked(roomID), h.snapshotLocked(roomID)
}

func (h *typingHub) Snapshot(roomID string, now time.Time) []roomTypingEvent {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneExpiredLocked(roomID, now)
	return h.snapshotLocked(roomID)
}

func (h *typingHub) subscribeLocked(roomID string) *typingSubscription {
	h.nextSubID++
	id := h.nextSubID
	sub := &typingSubscription{
		hub:    h,
		roomID: roomID,
		id:     id,
		ch:     make(chan roomTypingEvent, h.buffer),
	}
	if h.subsByRoom[roomID] == nil {
		h.subsByRoom[roomID] = make(map[int64]*typingSubscription)
	}
	h.subsByRoom[roomID][id] = sub
	return sub
}

func (h *typingHub) snapshotLocked(roomID string) []roomTypingEvent {
	active := h.activeByRoom[roomID]
	if len(active) == 0 {
		return nil
	}
	out := make([]roomTypingEvent, 0, len(active))
	for _, presence := range active {
		out = append(out, presence.event)
	}
	return out
}

func (h *typingHub) Start(roomID, actorID string, now time.Time, ttl time.Duration) roomTypingEvent {
	roomID = strings.TrimSpace(roomID)
	actorID = strings.TrimSpace(actorID)
	if roomID == "" || actorID == "" {
		return roomTypingEvent{}
	}
	if ttl < minTypingTTL {
		ttl = defaultTypingTTL
	}
	if ttl > maxTypingTTL {
		ttl = maxTypingTTL
	}
	event := roomTypingEvent{
		Type:      "agent.typing",
		RoomID:    roomID,
		ActorID:   actorID,
		State:     "start",
		TTLMS:     int(ttl / time.Millisecond),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}

	h.mu.Lock()
	h.pruneExpiredLocked(roomID, now)
	roomStates := h.ensureRoomStateLocked(roomID)
	if prev := roomStates[actorID]; prev != nil && prev.timer != nil {
		prev.timer.Stop()
	}
	if roomStates == nil {
		roomStates = h.ensureRoomStateLocked(roomID)
	}
	timer := time.AfterFunc(ttl, func() {
		h.expire(roomID, actorID, event.ExpiresAt)
	})
	roomStates[actorID] = &typingPresence{event: event, timer: timer}
	subs := h.subscribersLocked(roomID)
	h.mu.Unlock()

	h.broadcast(subs, event)
	return event
}

func (h *typingHub) Stop(roomID, actorID string, now time.Time) (roomTypingEvent, bool) {
	roomID = strings.TrimSpace(roomID)
	actorID = strings.TrimSpace(actorID)
	if roomID == "" || actorID == "" {
		return roomTypingEvent{}, false
	}

	h.mu.Lock()
	roomStates := h.activeByRoom[roomID]
	current, ok := roomStates[actorID]
	if !ok {
		h.mu.Unlock()
		return roomTypingEvent{}, false
	}
	if current.timer != nil {
		current.timer.Stop()
	}
	delete(roomStates, actorID)
	if len(roomStates) == 0 {
		delete(h.activeByRoom, roomID)
	}
	event := roomTypingEvent{
		Type:      "agent.typing",
		RoomID:    roomID,
		ActorID:   actorID,
		State:     "stop",
		CreatedAt: now,
		ExpiresAt: now,
	}
	subs := h.subscribersLocked(roomID)
	h.mu.Unlock()

	h.broadcast(subs, event)
	return event, true
}

func (h *typingHub) ClearRoom(roomID string, now time.Time) []roomTypingEvent {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil
	}

	h.mu.Lock()
	roomStates := h.activeByRoom[roomID]
	if len(roomStates) == 0 {
		h.mu.Unlock()
		return nil
	}
	events := make([]roomTypingEvent, 0, len(roomStates))
	for actorID, presence := range roomStates {
		if presence.timer != nil {
			presence.timer.Stop()
		}
		events = append(events, roomTypingEvent{
			Type:      "agent.typing",
			RoomID:    roomID,
			ActorID:   actorID,
			State:     "stop",
			CreatedAt: now,
			ExpiresAt: now,
		})
	}
	delete(h.activeByRoom, roomID)
	subs := h.subscribersLocked(roomID)
	h.mu.Unlock()

	for _, event := range events {
		h.broadcast(subs, event)
	}
	return events
}

func (h *typingHub) expire(roomID, actorID string, expectedExpiresAt time.Time) {
	now := time.Now().UTC()
	h.mu.Lock()
	roomStates := h.activeByRoom[roomID]
	current, ok := roomStates[actorID]
	if !ok || !current.event.ExpiresAt.Equal(expectedExpiresAt) {
		h.mu.Unlock()
		return
	}
	delete(roomStates, actorID)
	if len(roomStates) == 0 {
		delete(h.activeByRoom, roomID)
	}
	subs := h.subscribersLocked(roomID)
	h.mu.Unlock()

	h.broadcast(subs, roomTypingEvent{
		Type:      "agent.typing",
		RoomID:    roomID,
		ActorID:   actorID,
		State:     "stop",
		CreatedAt: now,
		ExpiresAt: now,
	})
}

func (h *typingHub) ensureRoomStateLocked(roomID string) map[string]*typingPresence {
	if h.activeByRoom[roomID] == nil {
		h.activeByRoom[roomID] = make(map[string]*typingPresence)
	}
	return h.activeByRoom[roomID]
}

func (h *typingHub) pruneExpiredLocked(roomID string, now time.Time) {
	roomStates := h.activeByRoom[roomID]
	if len(roomStates) == 0 {
		return
	}
	for actorID, presence := range roomStates {
		if presence == nil || now.Before(presence.event.ExpiresAt) {
			continue
		}
		if presence.timer != nil {
			presence.timer.Stop()
		}
		delete(roomStates, actorID)
	}
	if len(roomStates) == 0 {
		delete(h.activeByRoom, roomID)
	}
}

func (h *typingHub) subscribersLocked(roomID string) []*typingSubscription {
	subs := h.subsByRoom[roomID]
	if len(subs) == 0 {
		return nil
	}
	out := make([]*typingSubscription, 0, len(subs))
	for _, sub := range subs {
		out = append(out, sub)
	}
	return out
}

func (h *typingHub) broadcast(subs []*typingSubscription, event roomTypingEvent) {
	if len(subs) == 0 {
		return
	}
	toDrop := make([]int64, 0, len(subs))
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			toDrop = append(toDrop, sub.id)
		}
	}
	if len(toDrop) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	roomSubs := h.subsByRoom[event.RoomID]
	for _, id := range toDrop {
		sub, ok := roomSubs[id]
		if !ok {
			continue
		}
		sub.dropped.Store(true)
		delete(roomSubs, id)
		close(sub.ch)
	}
	if len(roomSubs) == 0 {
		delete(h.subsByRoom, event.RoomID)
	}
}

func (s *typingSubscription) Events() <-chan roomTypingEvent {
	if s == nil {
		return nil
	}
	return s.ch
}

func (s *typingSubscription) Dropped() bool {
	if s == nil {
		return false
	}
	return s.dropped.Load()
}

func (s *typingSubscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.once.Do(func() {
		s.hub.unsubscribeTyping(s.roomID, s.id)
	})
}

func (h *typingHub) unsubscribeTyping(roomID string, subID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	subs := h.subsByRoom[roomID]
	if len(subs) == 0 {
		return
	}
	sub, ok := subs[subID]
	if !ok {
		return
	}
	delete(subs, subID)
	close(sub.ch)
	if len(subs) == 0 {
		delete(h.subsByRoom, roomID)
	}
}
