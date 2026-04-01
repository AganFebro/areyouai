package httpapi

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/febrian/areyouai/internal/repository"
)

type roomEventHub struct {
	mu sync.RWMutex

	nextSubID int64
	buffer    int

	subsByRoom map[string]map[int64]*roomEventSubscription
}

type roomEventSubscription struct {
	hub     *roomEventHub
	roomID  string
	id      int64
	ch      chan repository.RoomEvent
	once    sync.Once
	dropped atomic.Bool
}

func newRoomEventHub(buffer int) *roomEventHub {
	if buffer <= 0 {
		buffer = 64
	}
	return &roomEventHub{
		buffer:     buffer,
		subsByRoom: make(map[string]map[int64]*roomEventSubscription),
	}
}

func (h *roomEventHub) Subscribe(roomID string) *roomEventSubscription {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		ch := make(chan repository.RoomEvent)
		close(ch)
		return &roomEventSubscription{ch: ch}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextSubID++
	id := h.nextSubID
	sub := &roomEventSubscription{
		hub:    h,
		roomID: roomID,
		id:     id,
		ch:     make(chan repository.RoomEvent, h.buffer),
	}
	if h.subsByRoom[roomID] == nil {
		h.subsByRoom[roomID] = make(map[int64]*roomEventSubscription)
	}
	h.subsByRoom[roomID][id] = sub
	return sub
}

func (h *roomEventHub) Publish(ev repository.RoomEvent) {
	roomID := strings.TrimSpace(ev.RoomID)
	if roomID == "" {
		return
	}

	h.mu.RLock()
	subs := h.subsByRoom[roomID]
	if len(subs) == 0 {
		h.mu.RUnlock()
		return
	}

	toDrop := make([]int64, 0, len(subs))
	for id, sub := range subs {
		select {
		case sub.ch <- ev:
		default:
			toDrop = append(toDrop, id)
		}
	}
	h.mu.RUnlock()

	if len(toDrop) == 0 {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	subs = h.subsByRoom[roomID]
	for _, id := range toDrop {
		sub, ok := subs[id]
		if !ok {
			continue
		}
		sub.dropped.Store(true)
		delete(subs, id)
		close(sub.ch)
	}
	if len(subs) == 0 {
		delete(h.subsByRoom, roomID)
	}
}

func (h *roomEventHub) SubscriberCount(roomID string) int {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subsByRoom[roomID])
}

func (s *roomEventSubscription) Events() <-chan repository.RoomEvent {
	if s == nil {
		return nil
	}
	return s.ch
}

func (s *roomEventSubscription) Dropped() bool {
	if s == nil {
		return false
	}
	return s.dropped.Load()
}

func (s *roomEventSubscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.once.Do(func() {
		s.hub.unsubscribe(s.roomID, s.id)
	})
}

func (h *roomEventHub) unsubscribe(roomID string, subID int64) {
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
