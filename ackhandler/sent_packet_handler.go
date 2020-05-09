package ackhandler

import (
	"errors"
	"fmt"
	"time"

	"github.com/lucas-clemente/pstream/congestion"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
	"github.com/lucas-clemente/pstream/internal/wire"
	"github.com/lucas-clemente/pstream/qerr"
)

const (
	// Maximum reordering in time space before time based loss detection considers a packet lost.
	// In fraction of an RTT.
	timeReorderingFraction = 1.0 / 8
	// defaultRTOTimeout is the RTO time on new connections
	defaultRTOTimeout = 500 * time.Millisecond
	// Minimum time in the future an RTO alarm may be set for.
	minRTOTimeout = 200 * time.Millisecond
	// maxRTOTimeout is the maximum RTO time
	maxRTOTimeout = 60 * time.Second
	// Sends up to two tail loss probes before firing a RTO, as per
	// draft RFC draft-dukkipati-tcpm-tcp-loss-probe
	maxTailLossProbes = 2
	// TCP RFC calls for 1 second RTO however Linux differs from this default and
	// define the minimum RTO to 200ms, we will use the same until we have data to
	// support a higher or lower value
	minRetransmissionTime = 200 * time.Millisecond
	// Minimum tail loss probe time in ms
	minTailLossProbeTimeout = 10 * time.Millisecond
)

var (
	// ErrDuplicateOrOutOfOrderAck occurs when a duplicate or an out-of-order ACK is received
	ErrDuplicateOrOutOfOrderAck = errors.New("SentPacketHandler: Duplicate or out-of-order ACK")
	// ErrTooManyTrackedSentPackets occurs when the sentPacketHandler has to keep track of too many packets
	ErrTooManyTrackedSentPackets = errors.New("Too many outstanding non-acked and non-retransmitted packets")
	// ErrAckForSkippedPacket occurs when the client sent an ACK for a packet number that we intentionally skipped
	ErrAckForSkippedPacket = qerr.Error(qerr.InvalidAckData, "Received an ACK for a skipped packet number")
	errAckForUnsentPacket  = qerr.Error(qerr.InvalidAckData, "Received ACK for an unsent package")
)

var errPacketNumberNotIncreasing = errors.New("Already sent a packet with a higher packet number")

type sentPacketHandler struct {
	lastSentPacketNumber protocol.PacketNumber
	skippedPackets       []protocol.PacketNumber

	pathID protocol.PathID // record corresponding path ID

	numNonRetransmittablePackets int // number of non-retransmittable packets since the last retransmittable packet

	LargestAcked protocol.PacketNumber

	largestReceivedPacketWithAck protocol.PacketNumber

	packetHistory      *PacketList
	stopWaitingManager stopWaitingManager

	retransmissionQueue []*Packet

	bytesInFlight protocol.ByteCount

	congestion congestion.SendAlgorithm
	rttStats   *congestion.RTTStats
	bdwStats   *congestion.BDWStats

	onRTOCallback func(time.Time) bool

	// The number of times an RTO has been sent without receiving an ack.
	rtoCount uint32

	// The number of times a TLP has been sent without receiving an ACK
	tlpCount uint32

	// The time at which the next packet will be considered lost based on early transmit or exceeding the reordering window in time.
	lossTime time.Time

	// The time the last packet was sent, used to set the retransmission timeout
	lastSentTime time.Time

	// The alarm timeout
	alarm time.Time

	packets         uint64
	retransmissions uint64
	losses          uint64
}

// NewSentPacketHandler creates a new sentPacketHandler
func NewSentPacketHandler(pathID protocol.PathID, rttStats *congestion.RTTStats, bdwStats *congestion.BDWStats, cong congestion.SendAlgorithm, onRTOCallback func(time.Time) bool) SentPacketHandler {
	var congestionControl congestion.SendAlgorithm

	if cong != nil {
		congestionControl = cong
	} else {
		congestionControl = congestion.NewCubicSender(
			congestion.DefaultClock{},
			rttStats,
			false, /* don't use reno since chromium doesn't (why?) */
			protocol.InitialCongestionWindow,
			protocol.DefaultMaxCongestionWindow,
		)
	}

	return &sentPacketHandler{
		pathID:             pathID,
		packetHistory:      NewPacketList(),
		stopWaitingManager: stopWaitingManager{},
		rttStats:           rttStats,
		bdwStats:           bdwStats,
		congestion:         congestionControl,
		onRTOCallback:      onRTOCallback,
	}
}

func (h *sentPacketHandler) GetStatistics() (uint64, uint64, uint64) {
	return h.packets, h.retransmissions, h.losses
}

func (h *sentPacketHandler) largestInOrderAcked() protocol.PacketNumber {
	if f := h.packetHistory.Front(); f != nil {
		return f.Value.PacketNumber - 1
	}
	return h.LargestAcked
}

func (h *sentPacketHandler) ShouldSendRetransmittablePacket() bool {
	return h.numNonRetransmittablePackets >= protocol.MaxNonRetransmittablePackets
}

func (h *sentPacketHandler) SentPacket(packet *Packet) error {
	if packet.PacketNumber <= h.lastSentPacketNumber {
		return errPacketNumberNotIncreasing
	}

	if protocol.PacketNumber(len(h.retransmissionQueue)+h.packetHistory.Len()+1) > protocol.MaxTrackedSentPackets {
		return ErrTooManyTrackedSentPackets
	}

	for p := h.lastSentPacketNumber + 1; p < packet.PacketNumber; p++ {
		h.skippedPackets = append(h.skippedPackets, p)

		if len(h.skippedPackets) > protocol.MaxTrackedSkippedPackets {
			h.skippedPackets = h.skippedPackets[1:]
		}
	}

	h.lastSentPacketNumber = packet.PacketNumber
	now := time.Now()

	// Update some statistics
	h.packets++

	// XXX RTO and TLP are recomputed based on the possible last sent retransmission. Is it ok like this?
	h.lastSentTime = now

	packet.Frames = stripNonRetransmittableFrames(packet.Frames)
	isRetransmittable := len(packet.Frames) != 0

	if isRetransmittable {
		packet.SendTime = now
		h.bytesInFlight += packet.Length
		h.packetHistory.PushBack(*packet)
		h.numNonRetransmittablePackets = 0
	} else {
		h.numNonRetransmittablePackets++
	}

	h.congestion.OnPacketSent(
		now,
		h.bytesInFlight,
		packet.PacketNumber,
		packet.Length,
		isRetransmittable,
	)

	h.updateLossDetectionAlarm()
	return nil
}

func (h *sentPacketHandler) ReceivedAck(ackFrame *wire.AckFrame, withPacketNumber protocol.PacketNumber, rcvTime time.Time) error {
	if ackFrame.LargestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}

	// duplicate or out-of-order ACK
	if withPacketNumber <= h.largestReceivedPacketWithAck {
		return ErrDuplicateOrOutOfOrderAck
	}
	h.largestReceivedPacketWithAck = withPacketNumber

	// ignore repeated ACK (ACKs that don't have a higher LargestAcked than the last ACK)
	if ackFrame.LargestAcked <= h.largestInOrderAcked() {
		return nil
	}
	h.LargestAcked = ackFrame.LargestAcked

	if h.skippedPacketsAcked(ackFrame) {
		return ErrAckForSkippedPacket
	}

	rttUpdated := h.maybeUpdateRTT(ackFrame.LargestAcked, ackFrame.DelayTime, rcvTime)

	if rttUpdated {
		h.congestion.MaybeExitSlowStart()
	}

	ackedPackets, err := h.determineNewlyAckedPackets(ackFrame)
	if err != nil {
		return err
	}

	flag := 0
	var sentDelay time.Duration
	if len(ackedPackets) > 0 {
		preInflight := h.bytesInFlight
		if utils.Debug() {
			utils.Debugf("In test: now preInflight = %d bytes", preInflight)
		}
		for _, p := range ackedPackets {
			packet := p.Value
			if packet.PacketNumber == ackFrame.LargestAcked {
				flag = 1
				sentDelay = rcvTime.Sub(packet.SendTime)
				if sentDelay > ackFrame.DelayTime {
					sentDelay -= ackFrame.DelayTime
				}
				if utils.Debug() {
					utils.Debugf("In test: now sentDelay = %s ", sentDelay.String())
				}
			}

			h.onPacketAcked(p)
			h.congestion.OnPacketAcked(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}

		changeInflight := preInflight - h.bytesInFlight
		if utils.Debug() {
			utils.Debugf("In test:  preInflight = %d, h.bytesInFlight = %d, changeInflight = %d", preInflight, h.bytesInFlight, changeInflight)
		}
		if flag == 1 {
			h.bdwStats.UpdateBDW(changeInflight, sentDelay)
		}

	}

	h.detectLostPackets()
	h.updateLossDetectionAlarm()

	h.garbageCollectSkippedPackets()
	h.stopWaitingManager.ReceivedAck(ackFrame)

	return nil
}

func (h *sentPacketHandler) ReceivedClosePath(f *wire.ClosePathFrame, withPacketNumber protocol.PacketNumber, rcvTime time.Time) error {
	if f.LargestAcked > h.lastSentPacketNumber {
		return errAckForUnsentPacket
	}

	// this should never happen, since a closePath frame should be the last packet on a path
	if withPacketNumber <= h.largestReceivedPacketWithAck {
		return ErrDuplicateOrOutOfOrderAck
	}
	h.largestReceivedPacketWithAck = withPacketNumber

	// Compared to ACK frames, we should not ignore duplicate LargestAcked

	if h.skippedPacketsAckedClosePath(f) {
		return ErrAckForSkippedPacket
	}

	// No need for RTT estimation

	ackedPackets, err := h.determineNewlyAckedPacketsClosePath(f)
	if err != nil {
		return err
	}

	if len(ackedPackets) > 0 {
		for _, p := range ackedPackets {
			h.onPacketAcked(p)
			h.congestion.OnPacketAcked(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}

	h.SetInflightAsLost()

	h.garbageCollectSkippedPackets()
	// We do not send any STOP WAITING Frames, so no need to update the manager

	return nil
}

func (h *sentPacketHandler) determineNewlyAckedPackets(ackFrame *wire.AckFrame) ([]*PacketElement, error) {
	var ackedPackets []*PacketElement
	ackRangeIndex := 0
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		packetNumber := packet.PacketNumber

		// Ignore packets below the LowestAcked
		if packetNumber < ackFrame.LowestAcked {
			continue
		}
		// Break after LargestAcked is reached
		if packetNumber > ackFrame.LargestAcked {
			break
		}

		if ackFrame.HasMissingRanges() {
			ackRange := ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]

			for packetNumber > ackRange.Last && ackRangeIndex < len(ackFrame.AckRanges)-1 {
				ackRangeIndex++
				ackRange = ackFrame.AckRanges[len(ackFrame.AckRanges)-1-ackRangeIndex]
			}

			if packetNumber >= ackRange.First { // packet i contained in ACK range
				if packetNumber > ackRange.Last {
					return nil, fmt.Errorf("BUG: ackhandler would have acked wrong packet 0x%x, while evaluating range 0x%x -> 0x%x", packetNumber, ackRange.First, ackRange.Last)
				}
				ackedPackets = append(ackedPackets, el)
			}
		} else {
			ackedPackets = append(ackedPackets, el)
		}
	}

	return ackedPackets, nil
}

func (h *sentPacketHandler) determineNewlyAckedPacketsClosePath(f *wire.ClosePathFrame) ([]*PacketElement, error) {
	var ackedPackets []*PacketElement
	ackRangeIndex := 0
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		packetNumber := packet.PacketNumber

		// Ignore packets below the LowestAcked
		if packetNumber < f.LowestAcked {
			continue
		}
		// Break after LargestAcked is reached
		if packetNumber > f.LargestAcked {
			break
		}

		if f.HasMissingRanges() {
			ackRange := f.AckRanges[len(f.AckRanges)-1-ackRangeIndex]

			for packetNumber > ackRange.Last && ackRangeIndex < len(f.AckRanges)-1 {
				ackRangeIndex++
				ackRange = f.AckRanges[len(f.AckRanges)-1-ackRangeIndex]
			}

			if packetNumber >= ackRange.First { // packet i contained in ACK range
				if packetNumber > ackRange.Last {
					return nil, fmt.Errorf("BUG: ackhandler would have acked wrong packet 0x%x, while evaluating range 0x%x -> 0x%x with ClosePath frame", packetNumber, ackRange.First, ackRange.Last)
				}
				ackedPackets = append(ackedPackets, el)
			}
		} else {
			ackedPackets = append(ackedPackets, el)
		}
	}

	return ackedPackets, nil
}

func (h *sentPacketHandler) maybeUpdateRTT(largestAcked protocol.PacketNumber, ackDelay time.Duration, rcvTime time.Time) bool {
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value
		if packet.PacketNumber == largestAcked {
			h.rttStats.UpdateRTT(rcvTime.Sub(packet.SendTime), ackDelay, time.Now())
			return true
		}
		// Packets are sorted by number, so we can stop searching
		if packet.PacketNumber > largestAcked {
			break
		}
	}
	return false
}

func (h *sentPacketHandler) hasOutstandingRetransmittablePacket() bool {
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		if el.Value.IsRetransmittable() {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) updateLossDetectionAlarm() {
	// Cancel the alarm if no packets are outstanding
	if h.packetHistory.Len() == 0 {
		h.alarm = time.Time{}
		return
	}

	// TODO(#496): Handle handshake packets separately
	if !h.lossTime.IsZero() {
		// Early retransmit timer or time loss detection.
		h.alarm = h.lossTime
	} else if h.rttStats.SmoothedRTT() != 0 && h.tlpCount < maxTailLossProbes {
		// TLP
		h.alarm = h.lastSentTime.Add(h.computeTLPTimeout())
	} else {
		// RTO
		h.alarm = h.lastSentTime.Add(utils.MaxDuration(h.computeRTOTimeout(), minRetransmissionTime))
	}
}

func (h *sentPacketHandler) detectLostPackets() {
	h.lossTime = time.Time{}
	now := time.Now()

	maxRTT := float64(utils.MaxDuration(h.rttStats.LatestRTT(), h.rttStats.SmoothedRTT()))
	delayUntilLost := time.Duration((1.0 + timeReorderingFraction) * maxRTT)

	var lostPackets []*PacketElement
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value

		if packet.PacketNumber > h.LargestAcked {
			break
		}

		timeSinceSent := now.Sub(packet.SendTime)
		if timeSinceSent > delayUntilLost {
			// Update statistics
			h.losses++
			lostPackets = append(lostPackets, el)
		} else if h.lossTime.IsZero() {
			// Note: This conditional is only entered once per call
			h.lossTime = now.Add(delayUntilLost - timeSinceSent)
		}
	}

	if len(lostPackets) > 0 {
		for _, p := range lostPackets {
			h.queuePacketForRetransmission(p)
			h.congestion.OnPacketLost(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}
}

func (h *sentPacketHandler) SetInflightAsLost() {
	var lostPackets []*PacketElement
	for el := h.packetHistory.Front(); el != nil; el = el.Next() {
		packet := el.Value

		if packet.PacketNumber > h.LargestAcked {
			break
		}

		h.losses++
		lostPackets = append(lostPackets, el)
	}

	if len(lostPackets) > 0 {
		for _, p := range lostPackets {
			h.queuePacketForRetransmission(p)
			// XXX (QDC): should we?
			h.congestion.OnPacketLost(p.Value.PacketNumber, p.Value.Length, h.bytesInFlight)
		}
	}
}

func (h *sentPacketHandler) OnAlarm() {
	// Do we really have packet to retransmit?
	if !h.hasOutstandingRetransmittablePacket() {
		// Cancel then the alarm
		h.alarm = time.Time{}
		return
	}

	// TODO(#496): Handle handshake packets separately
	if !h.lossTime.IsZero() {
		// Early retransmit or time loss detection
		h.detectLostPackets()

	} else if h.tlpCount < maxTailLossProbes {
		// TLP
		h.retransmitTLP()
		h.tlpCount++
	} else {
		// RTO
		potentiallyFailed := false
		if h.onRTOCallback != nil {
			potentiallyFailed = h.onRTOCallback(h.lastSentTime)
		}
		if potentiallyFailed {
			h.retransmitAllPackets()
		} else {
			h.retransmitOldestTwoPackets()
		}
		h.rtoCount++
	}

	h.updateLossDetectionAlarm()
}

func (h *sentPacketHandler) GetAlarmTimeout() time.Time {
	return h.alarm
}

func (h *sentPacketHandler) onPacketAcked(packetElement *PacketElement) {
	h.bytesInFlight -= packetElement.Value.Length
	h.rtoCount = 0
	h.tlpCount = 0
	h.packetHistory.Remove(packetElement)
}

func (h *sentPacketHandler) DequeuePacketForRetransmission() *Packet {
	if len(h.retransmissionQueue) == 0 {
		return nil
	}
	packet := h.retransmissionQueue[0]
	// Shift the slice and don't retain anything that isn't needed.
	copy(h.retransmissionQueue, h.retransmissionQueue[1:])
	h.retransmissionQueue[len(h.retransmissionQueue)-1] = nil
	h.retransmissionQueue = h.retransmissionQueue[:len(h.retransmissionQueue)-1]
	// Update statistics
	h.retransmissions++
	return packet
}

func (h *sentPacketHandler) GetLeastUnacked() protocol.PacketNumber {
	return h.largestInOrderAcked() + 1
}

func (h *sentPacketHandler) GetStopWaitingFrame(force bool) *wire.StopWaitingFrame {
	return h.stopWaitingManager.GetStopWaitingFrame(force)
}

func (h *sentPacketHandler) SendingAllowed() bool {
	congestionLimited := h.bytesInFlight > h.congestion.GetCongestionWindow()
	maxTrackedLimited := protocol.PacketNumber(len(h.retransmissionQueue)+h.packetHistory.Len()) >= protocol.MaxTrackedSentPackets
	if congestionLimited {
		utils.Debugf("Congestion limited: Path %x, bytes in flight %d, window %d",
			h.pathID,
			h.bytesInFlight,
			h.congestion.GetCongestionWindow())
	}
	// Workaround for #555:
	// Always allow sending of retransmissions. This should probably be limited
	// to RTOs, but we currently don't have a nice way of distinguishing them.
	haveRetransmissions := len(h.retransmissionQueue) > 0
	return !maxTrackedLimited && (!congestionLimited || haveRetransmissions)
}

func (h *sentPacketHandler) retransmitTLP() {
	if p := h.packetHistory.Back(); p != nil {
		h.queuePacketForRetransmission(p)
	}
}

func (h *sentPacketHandler) retransmitAllPackets() {
	for h.packetHistory.Len() > 0 {
		h.queueRTO(h.packetHistory.Front())
	}
	h.congestion.OnRetransmissionTimeout(true)
}

func (h *sentPacketHandler) retransmitOldestPacket() {
	if p := h.packetHistory.Front(); p != nil {
		h.queueRTO(p)
	}
}

func (h *sentPacketHandler) retransmitOldestTwoPackets() {
	h.retransmitOldestPacket()
	h.retransmitOldestPacket()
	h.congestion.OnRetransmissionTimeout(true)
}

func (h *sentPacketHandler) queueRTO(el *PacketElement) {
	packet := &el.Value
	utils.Debugf(
		"\tQueueing packet 0x%x for retransmission (RTO), %d outstanding",
		packet.PacketNumber,
		h.packetHistory.Len(),
	)
	h.queuePacketForRetransmission(el)
	h.losses++
	h.congestion.OnPacketLost(packet.PacketNumber, packet.Length, h.bytesInFlight)
}

func (h *sentPacketHandler) queuePacketForRetransmission(packetElement *PacketElement) {
	packet := &packetElement.Value
	h.bytesInFlight -= packet.Length
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
	h.packetHistory.Remove(packetElement)
	h.stopWaitingManager.QueuedRetransmissionForPacketNumber(packet.PacketNumber)
}

func (h *sentPacketHandler) DuplicatePacket(packet *Packet) {
	h.retransmissionQueue = append(h.retransmissionQueue, packet)
}

func (h *sentPacketHandler) computeRTOTimeout() time.Duration {
	rto := h.congestion.RetransmissionDelay()
	if rto == 0 {
		rto = defaultRTOTimeout
	}
	rto = utils.MaxDuration(rto, minRTOTimeout)
	// Exponential backoff
	rto = rto << h.rtoCount
	return utils.MinDuration(rto, maxRTOTimeout)
}

func (h *sentPacketHandler) hasMultipleOutstandingRetransmittablePackets() bool {
	return h.packetHistory.Front() != nil && h.packetHistory.Front().Next() != nil
}

func (h *sentPacketHandler) computeTLPTimeout() time.Duration {
	rtt := h.congestion.SmoothedRTT()
	if h.hasMultipleOutstandingRetransmittablePackets() {
		return utils.MaxDuration(2*rtt, rtt*3/2+minRetransmissionTime/2)
	}
	return utils.MaxDuration(2*rtt, minTailLossProbeTimeout)
}

func (h *sentPacketHandler) skippedPacketsAcked(ackFrame *wire.AckFrame) bool {
	for _, p := range h.skippedPackets {
		if ackFrame.AcksPacket(p) {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) skippedPacketsAckedClosePath(closePathFrame *wire.ClosePathFrame) bool {
	for _, p := range h.skippedPackets {
		if closePathFrame.AcksPacket(p) {
			return true
		}
	}
	return false
}

func (h *sentPacketHandler) garbageCollectSkippedPackets() {
	lioa := h.largestInOrderAcked()
	deleteIndex := 0
	for i, p := range h.skippedPackets {
		if p <= lioa {
			deleteIndex = i + 1
		}
	}
	h.skippedPackets = h.skippedPackets[deleteIndex:]
}
