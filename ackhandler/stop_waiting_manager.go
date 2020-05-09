package ackhandler

import (
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/wire"
)

// This stopWaitingManager is not supposed to satisfy the StopWaitingManager interface, which is a remnant of the legacy AckHandler, and should be remove once we drop support for QUIC 33
type stopWaitingManager struct {
	largestLeastUnackedSent protocol.PacketNumber
	nextLeastUnacked        protocol.PacketNumber

	lastStopWaitingFrame *wire.StopWaitingFrame
}

func (s *stopWaitingManager) GetStopWaitingFrame(force bool) *wire.StopWaitingFrame {
	if s.nextLeastUnacked <= s.largestLeastUnackedSent {
		if force {
			if s.lastStopWaitingFrame == nil && s.largestLeastUnackedSent > 0 {
				// This case is possible when no previous SWF were sent (lost first packet)
				swf := &wire.StopWaitingFrame{
					LeastUnacked: s.nextLeastUnacked,
				}
				s.lastStopWaitingFrame = swf
			}
			return s.lastStopWaitingFrame
		}
		return nil
	}

	s.largestLeastUnackedSent = s.nextLeastUnacked
	swf := &wire.StopWaitingFrame{
		LeastUnacked: s.nextLeastUnacked,
	}
	s.lastStopWaitingFrame = swf
	return swf
}

func (s *stopWaitingManager) ReceivedAck(ack *wire.AckFrame) {
	if ack.LargestAcked >= s.nextLeastUnacked {
		s.nextLeastUnacked = ack.LargestAcked + 1
	}
}

func (s *stopWaitingManager) QueuedRetransmissionForPacketNumber(p protocol.PacketNumber) {
	if p >= s.nextLeastUnacked {
		s.nextLeastUnacked = p + 1
	}
}
