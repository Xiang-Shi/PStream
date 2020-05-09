package quic

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lucas-clemente/pstream/ackhandler"
	"github.com/lucas-clemente/pstream/internal/handshake"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
	"github.com/lucas-clemente/pstream/internal/wire"
)

type packedPacket struct {
	number          protocol.PacketNumber
	raw             []byte
	frames          []wire.Frame
	encryptionLevel protocol.EncryptionLevel
}

type packetPacker struct {
	connectionID protocol.ConnectionID
	perspective  protocol.Perspective
	version      protocol.VersionNumber
	cryptoSetup  handshake.CryptoSetup

	connectionParameters handshake.ConnectionParametersManager
	streamFramer         *streamFramer

	controlFrames []wire.Frame
	stopWaiting   map[protocol.PathID]*wire.StopWaitingFrame
	ackFrame      map[protocol.PathID]*wire.AckFrame
}

func newPacketPacker(connectionID protocol.ConnectionID,
	cryptoSetup handshake.CryptoSetup,
	connectionParameters handshake.ConnectionParametersManager,
	streamFramer *streamFramer,
	perspective protocol.Perspective,
	version protocol.VersionNumber,
) *packetPacker {
	return &packetPacker{
		cryptoSetup:          cryptoSetup,
		connectionID:         connectionID,
		connectionParameters: connectionParameters,
		perspective:          perspective,
		version:              version,
		streamFramer:         streamFramer,
		stopWaiting:          make(map[protocol.PathID]*wire.StopWaitingFrame),
		ackFrame:             make(map[protocol.PathID]*wire.AckFrame),
	}
}

// PackConnectionClose packs a packet that ONLY contains a ConnectionCloseFrame
func (p *packetPacker) PackConnectionClose(ccf *wire.ConnectionCloseFrame, pth *path) (*packedPacket, error) {
	frames := []wire.Frame{ccf}
	encLevel, sealer := p.cryptoSetup.GetSealer()
	ph := p.getPublicHeader(encLevel, pth)
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, err
}

// PackPing packs a packet that ONLY contains a PingFrame
func (p *packetPacker) PackPing(pf *wire.PingFrame, pth *path) (*packedPacket, error) {
	// Add the PingFrame in front of the controlFrames
	pth.SetLeastUnacked(pth.sentPacketHandler.GetLeastUnacked())
	p.controlFrames = append([]wire.Frame{pf}, p.controlFrames...)
	return p.PackPacket(pth)
}

func (p *packetPacker) PackAckPacket(pth *path) (*packedPacket, error) {
	if p.ackFrame[pth.pathID] == nil {
		return nil, errors.New("packet packer BUG: no ack frame queued")
	}
	encLevel, sealer := p.cryptoSetup.GetSealer()
	ph := p.getPublicHeader(encLevel, pth)
	frames := []wire.Frame{p.ackFrame[pth.pathID]}
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = ph.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = ph.PacketNumberLen
		frames = append(frames, p.stopWaiting[pth.pathID])
		p.stopWaiting[pth.pathID] = nil
	}
	p.ackFrame[pth.pathID] = nil
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, err
}

// PackHandshakeRetransmission retransmits a handshake packet, that was sent with less than forward-secure encryption
func (p *packetPacker) PackHandshakeRetransmission(packet *ackhandler.Packet, pth *path) (*packedPacket, error) {
	if packet.EncryptionLevel == protocol.EncryptionForwardSecure {
		return nil, errors.New("PacketPacker BUG: forward-secure encrypted handshake packets don't need special treatment")
	}
	sealer, err := p.cryptoSetup.GetSealerWithEncryptionLevel(packet.EncryptionLevel)
	if err != nil {
		return nil, err
	}
	if p.stopWaiting[pth.pathID] == nil {
		return nil, errors.New("PacketPacker BUG: Handshake retransmissions must contain a StopWaitingFrame")
	}
	ph := p.getPublicHeader(packet.EncryptionLevel, pth)
	p.stopWaiting[pth.pathID].PacketNumber = ph.PacketNumber
	p.stopWaiting[pth.pathID].PacketNumberLen = ph.PacketNumberLen
	frames := append([]wire.Frame{p.stopWaiting[pth.pathID]}, packet.Frames...)
	p.stopWaiting[pth.pathID] = nil
	raw, err := p.writeAndSealPacket(ph, frames, sealer, pth)
	return &packedPacket{
		number:          ph.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: packet.EncryptionLevel,
	}, err
}

// PackPacket packs a new packet
// the other controlFrames are sent in the next packet, but might be queued and sent in the next packet if the packet would overflow MaxPacketSize otherwise
func (p *packetPacker) PackPacket(pth *path) (*packedPacket, error) {
	if p.streamFramer.HasCryptoStreamFrame() {
		return p.packCryptoPacket(pth)
	}

	encLevel, sealer := p.cryptoSetup.GetSealer()

	publicHeader := p.getPublicHeader(encLevel, pth)
	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		return nil, err
	}
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = publicHeader.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = publicHeader.PacketNumberLen
	}

	// TODO (QDC): rework this part with PING
	var isPing bool
	if len(p.controlFrames) > 0 {
		_, isPing = p.controlFrames[0].(*wire.PingFrame)
	}

	var payloadFrames []wire.Frame
	if isPing {
		payloadFrames = []wire.Frame{p.controlFrames[0]}
		// Remove the ping frame from the control frames
		p.controlFrames = p.controlFrames[1:len(p.controlFrames)]
	} else {
		maxSize := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - publicHeaderLength
		payloadFrames, err = p.composeNextPacket(maxSize, p.canSendData(encLevel), pth)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have enough frames to send
	if len(payloadFrames) == 0 {
		return nil, nil
	}
	// Don't send out packets that only contain a StopWaitingFrame
	if len(payloadFrames) == 1 && p.stopWaiting[pth.pathID] != nil {
		return nil, nil
	}
	p.stopWaiting[pth.pathID] = nil
	p.ackFrame[pth.pathID] = nil

	raw, err := p.writeAndSealPacket(publicHeader, payloadFrames, sealer, pth)
	if err != nil {
		return nil, err
	}
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          payloadFrames,
		encryptionLevel: encLevel,
	}, nil
}

// PackPacket packs data of streams reside in this path
func (p *packetPacker) PackPacketOfPath(pth *path) (*packedPacket, error) {
	if p.streamFramer.HasCryptoStreamFrame() {
		return p.packCryptoPacket(pth)
	}

	encLevel, sealer := p.cryptoSetup.GetSealer()

	publicHeader := p.getPublicHeader(encLevel, pth)
	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		return nil, err
	}
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = publicHeader.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = publicHeader.PacketNumberLen
	}

	// TODO (QDC): rework this part with PING
	var isPing bool
	if len(p.controlFrames) > 0 {
		_, isPing = p.controlFrames[0].(*wire.PingFrame)
	}

	var payloadFrames []wire.Frame
	if isPing {
		payloadFrames = []wire.Frame{p.controlFrames[0]}
		// Remove the ping frame from the control frames
		p.controlFrames = p.controlFrames[1:len(p.controlFrames)]
	} else {
		maxSize := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - publicHeaderLength
		payloadFrames, err = p.composeNextPacketOfPath(maxSize, p.canSendData(encLevel), pth)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have enough frames to send
	if len(payloadFrames) == 0 {
		return nil, nil
	}
	// Don't send out packets that only contain a StopWaitingFrame
	if len(payloadFrames) == 1 && p.stopWaiting[pth.pathID] != nil {
		return nil, nil
	}
	p.stopWaiting[pth.pathID] = nil
	p.ackFrame[pth.pathID] = nil

	raw, err := p.writeAndSealPacket(publicHeader, payloadFrames, sealer, pth)
	if err != nil {
		return nil, err
	}
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          payloadFrames,
		encryptionLevel: encLevel,
	}, nil
}

// PackPacket packs a new packet of a stream
func (p *packetPacker) PackPacketOfStream(pth *path, streamID protocol.StreamID) (*packedPacket, error) {
	if p.streamFramer.HasCryptoStreamFrame() {
		return p.packCryptoPacket(pth)
	}

	encLevel, sealer := p.cryptoSetup.GetSealer()

	publicHeader := p.getPublicHeader(encLevel, pth)
	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		return nil, err
	}
	if p.stopWaiting[pth.pathID] != nil {
		p.stopWaiting[pth.pathID].PacketNumber = publicHeader.PacketNumber
		p.stopWaiting[pth.pathID].PacketNumberLen = publicHeader.PacketNumberLen
	}

	// TODO (QDC): rework this part with PING
	var isPing bool
	if len(p.controlFrames) > 0 {
		_, isPing = p.controlFrames[0].(*wire.PingFrame)
	}

	var payloadFrames []wire.Frame
	if isPing {
		payloadFrames = []wire.Frame{p.controlFrames[0]}
		// Remove the ping frame from the control frames
		p.controlFrames = p.controlFrames[1:len(p.controlFrames)]
	} else {
		maxSize := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - publicHeaderLength
		payloadFrames, err = p.composeNextPacketOfStream(maxSize, p.canSendData(encLevel), pth, streamID)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have enough frames to send
	if len(payloadFrames) == 0 {
		return nil, nil
	}
	// Don't send out packets that only contain a StopWaitingFrame
	if len(payloadFrames) == 1 && p.stopWaiting[pth.pathID] != nil {
		return nil, nil
	}
	p.stopWaiting[pth.pathID] = nil
	p.ackFrame[pth.pathID] = nil

	raw, err := p.writeAndSealPacket(publicHeader, payloadFrames, sealer, pth)
	if err != nil {
		return nil, err
	}
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          payloadFrames,
		encryptionLevel: encLevel,
	}, nil
}

func (p *packetPacker) packCryptoPacket(pth *path) (*packedPacket, error) {
	encLevel, sealer := p.cryptoSetup.GetSealerForCryptoStream()
	publicHeader := p.getPublicHeader(encLevel, pth)
	publicHeaderLength, err := publicHeader.GetLength(p.perspective)
	if err != nil {
		return nil, err
	}
	maxLen := protocol.MaxPacketSize - protocol.ByteCount(sealer.Overhead()) - protocol.NonForwardSecurePacketSizeReduction - publicHeaderLength
	frames := []wire.Frame{p.streamFramer.PopCryptoStreamFrame(maxLen)}
	raw, err := p.writeAndSealPacket(publicHeader, frames, sealer, pth)
	if err != nil {
		return nil, err
	}
	if utils.Debug() {
		utils.Debugf("packCryptoPacket: packet number %d\n", publicHeader.PacketNumber)
	}
	return &packedPacket{
		number:          publicHeader.PacketNumber,
		raw:             raw,
		frames:          frames,
		encryptionLevel: encLevel,
	}, nil
}

func (p *packetPacker) composeNextPacket(
	maxFrameSize protocol.ByteCount,
	canSendStreamFrames bool,
	pth *path,
) ([]wire.Frame, error) {
	var payloadLength protocol.ByteCount
	var payloadFrames []wire.Frame

	// STOP_WAITING and ACK will always fit
	if p.stopWaiting[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.stopWaiting[pth.pathID])
		l, err := p.stopWaiting[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	if p.ackFrame[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.ackFrame[pth.pathID])
		l, err := p.ackFrame[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}

	for len(p.controlFrames) > 0 {
		frame := p.controlFrames[len(p.controlFrames)-1]
		minLength, err := frame.MinLength(p.version)
		if err != nil {
			return nil, err
		}
		if payloadLength+minLength > maxFrameSize {
			break
		}
		payloadFrames = append(payloadFrames, frame)
		payloadLength += minLength
		p.controlFrames = p.controlFrames[:len(p.controlFrames)-1]
	}

	if payloadLength > maxFrameSize {
		return nil, fmt.Errorf("Packet Packer BUG: packet payload (%d) too large (%d)", payloadLength, maxFrameSize)
	}

	if !canSendStreamFrames {
		return payloadFrames, nil
	}

	// temporarily increase the maxFrameSize by 2 bytes
	// this leads to a properly sized packet in all cases, since we do all the packet length calculations with StreamFrames that have the DataLen set
	// however, for the last StreamFrame in the packet, we can omit the DataLen, thus saving 2 bytes and yielding a packet of exactly the correct size
	maxFrameSize += 2

	fs := p.streamFramer.PopStreamFrames(maxFrameSize - payloadLength)
	if len(fs) != 0 {
		fs[len(fs)-1].DataLenPresent = false
	}

	// TODO: Simplify
	for _, f := range fs {
		payloadFrames = append(payloadFrames, f)
	}

	for b := p.streamFramer.PopBlockedFrame(); b != nil; b = p.streamFramer.PopBlockedFrame() {
		p.controlFrames = append(p.controlFrames, b)
	}

	return payloadFrames, nil
}

func (p *packetPacker) composeNextPacketOfStream(
	maxFrameSize protocol.ByteCount,
	canSendStreamFrames bool,
	pth *path,
	streamID protocol.StreamID,
) ([]wire.Frame, error) {
	var payloadLength protocol.ByteCount
	var payloadFrames []wire.Frame

	// STOP_WAITING and ACK will always fit
	if p.stopWaiting[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.stopWaiting[pth.pathID])
		l, err := p.stopWaiting[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	if p.ackFrame[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.ackFrame[pth.pathID])
		l, err := p.ackFrame[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	// pack control frames here(e.g. window update frames)
	for len(p.controlFrames) > 0 {
		frame := p.controlFrames[len(p.controlFrames)-1]
		minLength, err := frame.MinLength(p.version)
		if err != nil {
			return nil, err
		}
		if payloadLength+minLength > maxFrameSize {
			break
		}
		payloadFrames = append(payloadFrames, frame)
		payloadLength += minLength
		p.controlFrames = p.controlFrames[:len(p.controlFrames)-1]
	}

	if payloadLength > maxFrameSize {
		return nil, fmt.Errorf("Packet Packer BUG: packet payload (%d) too large (%d)", payloadLength, maxFrameSize)
	}

	if !canSendStreamFrames {
		return payloadFrames, nil
	}

	// temporarily increase the maxFrameSize by 2 bytes
	// this leads to a properly sized packet in all cases, since we do all the packet length calculations with StreamFrames that have the DataLen set
	// however, for the last StreamFrame in the packet, we can omit the DataLen, thus saving 2 bytes and yielding a packet of exactly the correct size
	maxFrameSize += 2

	fs := p.streamFramer.PopStreamFramesOfOneStream((maxFrameSize - payloadLength), streamID)
	if len(fs) != 0 {
		fs[len(fs)-1].DataLenPresent = false
	}

	// TODO: Simplify
	for _, f := range fs {
		payloadFrames = append(payloadFrames, f)
	}

	for b := p.streamFramer.PopBlockedFrame(); b != nil; b = p.streamFramer.PopBlockedFrame() {
		p.controlFrames = append(p.controlFrames, b)
	}

	return payloadFrames, nil
}

func (p *packetPacker) composeNextPacketOfPath(
	maxFrameSize protocol.ByteCount,
	canSendStreamFrames bool,
	pth *path,
) ([]wire.Frame, error) {
	var payloadLength protocol.ByteCount
	var payloadFrames []wire.Frame

	// STOP_WAITING and ACK will always fit
	if p.stopWaiting[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.stopWaiting[pth.pathID])
		l, err := p.stopWaiting[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	if p.ackFrame[pth.pathID] != nil {
		payloadFrames = append(payloadFrames, p.ackFrame[pth.pathID])
		l, err := p.ackFrame[pth.pathID].MinLength(p.version)
		if err != nil {
			return nil, err
		}
		payloadLength += l
	}
	// pack control frames here(e.g. window update frames)
	for len(p.controlFrames) > 0 {
		frame := p.controlFrames[len(p.controlFrames)-1]
		minLength, err := frame.MinLength(p.version)
		if err != nil {
			return nil, err
		}
		if payloadLength+minLength > maxFrameSize {
			break
		}
		payloadFrames = append(payloadFrames, frame)
		payloadLength += minLength
		p.controlFrames = p.controlFrames[:len(p.controlFrames)-1]
	}

	if payloadLength > maxFrameSize {
		return nil, fmt.Errorf("Packet Packer BUG: packet payload (%d) too large (%d)", payloadLength, maxFrameSize)
	}

	if !canSendStreamFrames {
		return payloadFrames, nil
	}

	// temporarily increase the maxFrameSize by 2 bytes
	// this leads to a properly sized packet in all cases, since we do all the packet length calculations with StreamFrames that have the DataLen set
	// however, for the last StreamFrame in the packet, we can omit the DataLen, thus saving 2 bytes and yielding a packet of exactly the correct size
	maxFrameSize += 2

	fs := p.streamFramer.PopStreamFramesOfPath((maxFrameSize - payloadLength), pth)
	if len(fs) != 0 {
		fs[len(fs)-1].DataLenPresent = false
	}

	// TODO: Simplify
	for _, f := range fs {
		payloadFrames = append(payloadFrames, f)
	}

	for b := p.streamFramer.PopBlockedFrame(); b != nil; b = p.streamFramer.PopBlockedFrame() {
		p.controlFrames = append(p.controlFrames, b)
	}

	return payloadFrames, nil
}

func (p *packetPacker) QueueControlFrame(frame wire.Frame, pth *path) {
	switch f := frame.(type) {
	case *wire.StopWaitingFrame:
		p.stopWaiting[pth.pathID] = f
	case *wire.AckFrame:
		p.ackFrame[pth.pathID] = f
	default:
		p.controlFrames = append(p.controlFrames, f)
	}
}

func (p *packetPacker) getPublicHeader(encLevel protocol.EncryptionLevel, pth *path) *wire.PublicHeader {
	pnum := pth.packetNumberGenerator.Peek()
	packetNumberLen := protocol.GetPacketNumberLengthForPublicHeader(pnum, pth.leastUnacked)
	publicHeader := &wire.PublicHeader{
		ConnectionID:         p.connectionID,
		PacketNumber:         pnum,
		PacketNumberLen:      packetNumberLen,
		TruncateConnectionID: p.connectionParameters.TruncateConnectionID(),
	}

	if p.perspective == protocol.PerspectiveServer && encLevel == protocol.EncryptionSecure {
		publicHeader.DiversificationNonce = p.cryptoSetup.DiversificationNonce()
	}
	if p.perspective == protocol.PerspectiveClient && encLevel != protocol.EncryptionForwardSecure {
		publicHeader.VersionFlag = true
		publicHeader.VersionNumber = p.version
	}

	// XXX (QDC): need a additional check because of tests
	if pth.sess != nil && pth.sess.handshakeComplete && p.version >= protocol.VersionMP {
		publicHeader.MultipathFlag = true
		publicHeader.PathID = pth.pathID
		// XXX (QDC): in case of doubt, never truncate the connection ID. This might change...
		publicHeader.TruncateConnectionID = false
	}

	return publicHeader
}

func (p *packetPacker) writeAndSealPacket(
	publicHeader *wire.PublicHeader,
	payloadFrames []wire.Frame,
	sealer handshake.Sealer,
	pth *path,
) ([]byte, error) {
	raw := getPacketBuffer()
	buffer := bytes.NewBuffer(raw)

	if err := publicHeader.Write(buffer, p.version, p.perspective); err != nil {
		return nil, err
	}
	payloadStartIndex := buffer.Len()
	for _, frame := range payloadFrames {
		err := frame.Write(buffer, p.version)
		if err != nil {
			return nil, err
		}
	}
	if protocol.ByteCount(buffer.Len()+sealer.Overhead()) > protocol.MaxPacketSize {
		return nil, errors.New("PacketPacker BUG: packet too large")
	}

	raw = raw[0:buffer.Len()]
	_ = sealer.Seal(raw[payloadStartIndex:payloadStartIndex], raw[payloadStartIndex:], publicHeader.PacketNumber, raw[:payloadStartIndex])
	raw = raw[0 : buffer.Len()+sealer.Overhead()]

	num := pth.packetNumberGenerator.Pop()
	if num != publicHeader.PacketNumber {
		return nil, errors.New("packetPacker BUG: Peeked and Popped packet numbers do not match")
	}

	return raw, nil
}

func (p *packetPacker) canSendData(encLevel protocol.EncryptionLevel) bool {
	if p.perspective == protocol.PerspectiveClient {
		return encLevel >= protocol.EncryptionSecure
	}
	return encLevel == protocol.EncryptionForwardSecure
}
