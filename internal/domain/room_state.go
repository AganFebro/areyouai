package domain

import "fmt"

type RoomState string

const (
	RoomStateOpen   RoomState = "OPEN"
	RoomStateActive RoomState = "ACTIVE"
	RoomStateClosed RoomState = "CLOSED"
	RoomStatePurged RoomState = "PURGED"
)

func (s RoomState) CanTransitionTo(next RoomState) bool {
	switch s {
	case RoomStateOpen:
		return next == RoomStateActive || next == RoomStateClosed
	case RoomStateActive:
		return next == RoomStateClosed
	case RoomStateClosed:
		return next == RoomStatePurged
	case RoomStatePurged:
		return false
	default:
		return false
	}
}

func TransitionState(current, next RoomState) error {
	if !current.CanTransitionTo(next) {
		return fmt.Errorf("invalid room transition %s -> %s", current, next)
	}
	return nil
}
