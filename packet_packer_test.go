package quic

import (
	"bytes"
	"math"

	"github.com/lucas-clemente/pstream/ackhandler"
	"github.com/lucas-clemente/pstream/congestion"
	"github.com/lucas-clemente/pstream/internal/handshake"
	"github.com/lucas-clemente/pstream/internal/mocks"
	"github.com/lucas-clemente/pstream/internal/mocks/mocks_fc"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/wire"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type mockSealer struct{}

func (s *mockSealer) Seal(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) []byte {
	return append(src, bytes.Repeat([]byte{0}, 12)...)
}

func (s *mockSealer) Overhead() int { return 12 }

var _ handshake.Sealer = &mockSealer{}

type mockCryptoSetup struct {
	handleErr          error
	divNonce           []byte
	encLevelSeal       protocol.EncryptionLevel
	encLevelSealCrypto protocol.EncryptionLevel
}

var _ handshake.CryptoSetup = &mockCryptoSetup{}

func (m *mockCryptoSetup) HandleCryptoStream() error {
	return m.handleErr
}
func (m *mockCryptoSetup) Open(dst, src []byte, packetNumber protocol.PacketNumber, associatedData []byte) ([]byte, protocol.EncryptionLevel, error) {
	return nil, protocol.EncryptionUnspecified, nil
}
func (m *mockCryptoSetup) GetSealer() (protocol.EncryptionLevel, handshake.Sealer) {
	return m.encLevelSeal, &mockSealer{}
}
func (m *mockCryptoSetup) GetSealerForCryptoStream() (protocol.EncryptionLevel, handshake.Sealer) {
	return m.encLevelSealCrypto, &mockSealer{}
}
func (m *mockCryptoSetup) GetSealerWithEncryptionLevel(protocol.EncryptionLevel) (handshake.Sealer, error) {
	return &mockSealer{}, nil
}
func (m *mockCryptoSetup) DiversificationNonce() []byte            { return m.divNonce }
func (m *mockCryptoSetup) SetDiversificationNonce(divNonce []byte) { m.divNonce = divNonce }

var _ = Describe("Packet packer", func() {
	var (
		packer          *packetPacker
		publicHeaderLen protocol.ByteCount
		maxFrameSize    protocol.ByteCount
		streamFramer    *streamFramer
		cryptoStream    *stream
		pth             *path
	)

	BeforeEach(func() {
		mockCpm := mocks.NewMockConnectionParametersManager(mockCtrl)
		mockCpm.EXPECT().TruncateConnectionID().Return(false).AnyTimes()

		cryptoStream = &stream{}

		streamsMap := newStreamsMapPriority(nil, protocol.PerspectiveServer, nil)
		streamsMap.streams[1] = cryptoStream
		streamsMap.openStreams = []protocol.StreamID{1}
		streamFramer = newStreamFramer(streamsMap, nil)

		pth = &path{
			streamQuota:           make(map[protocol.StreamID]uint8),
			sentPacketHandler:     ackhandler.NewSentPacketHandler(0, &congestion.RTTStats{}, &congestion.BDWStats{}, nil, nil),
			packetNumberGenerator: newPacketNumberGenerator(protocol.SkipPacketAveragePeriodLength),
		}

		packer = &packetPacker{
			cryptoSetup:          &mockCryptoSetup{encLevelSeal: protocol.EncryptionForwardSecure},
			connectionParameters: mockCpm,
			connectionID:         0x1337,
			streamFramer:         streamFramer,
			perspective:          protocol.PerspectiveServer,
			stopWaiting:          make(map[protocol.PathID]*wire.StopWaitingFrame),
			ackFrame:             make(map[protocol.PathID]*wire.AckFrame),
		}
		publicHeaderLen = 1 + 8 + 2 // 1 flag byte, 8 connection ID, 2 packet number
		maxFrameSize = protocol.MaxPacketSize - protocol.ByteCount((&mockSealer{}).Overhead()) - publicHeaderLen
		packer.version = protocol.VersionWhatever
	})

	It("returns nil when no packet is queued", func() {
		p, err := packer.PackPacket(pth)
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("returns nil when no packet is queued", func() {
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("packs single packets", func() {
		f := &wire.StreamFrame{
			StreamID: 5,
			Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		b := &bytes.Buffer{}
		f.Write(b, 0)
		Expect(p.frames).To(HaveLen(1))
		Expect(p.raw).To(ContainSubstring(string(b.Bytes())))
	})

	It("packs single packets", func() {
		f := &wire.StreamFrame{
			StreamID: 5,
			Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacketOfStream(pth, 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		b := &bytes.Buffer{}
		f.Write(b, 0)
		Expect(p.frames).To(HaveLen(1))
		Expect(p.raw).To(ContainSubstring(string(b.Bytes())))
	})

	It("stores the encryption level a packet was sealed with", func() {
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionForwardSecure
		f := &wire.StreamFrame{
			StreamID: 5,
			Data:     []byte("foobar"),
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.encryptionLevel).To(Equal(protocol.EncryptionForwardSecure))
	})

	It("stores the encryption level a packet was sealed with", func() {
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionForwardSecure
		f := &wire.StreamFrame{
			StreamID: 5,
			Data:     []byte("foobar"),
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err := packer.PackPacketOfStream(pth, 5)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.encryptionLevel).To(Equal(protocol.EncryptionForwardSecure))
	})

	Context("diversificaton nonces", func() {
		var nonce []byte

		BeforeEach(func() {
			nonce = bytes.Repeat([]byte{'e'}, 32)
			packer.cryptoSetup.(*mockCryptoSetup).divNonce = nonce
		})

		It("doesn't include a div nonce, when sending a packet with initial encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionUnencrypted, pth)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})

		It("includes a div nonce, when sending a packet with secure encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionSecure, pth)
			Expect(ph.DiversificationNonce).To(Equal(nonce))
		})

		It("doesn't include a div nonce, when sending a packet with forward-secure encryption", func() {
			ph := packer.getPublicHeader(protocol.EncryptionForwardSecure, pth)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})

		It("doesn't send a div nonce as a client", func() {
			packer.perspective = protocol.PerspectiveClient
			ph := packer.getPublicHeader(protocol.EncryptionSecure, pth)
			Expect(ph.DiversificationNonce).To(BeEmpty())
		})
	})

	It("packs a ConnectionClose", func() {
		ccf := wire.ConnectionCloseFrame{
			ErrorCode:    0x1337,
			ReasonPhrase: "foobar",
		}
		p, err := packer.PackConnectionClose(&ccf, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(&ccf))
	})

	It("doesn't send any other frames when sending a ConnectionClose", func() {
		ccf := wire.ConnectionCloseFrame{
			ErrorCode:    0x1337,
			ReasonPhrase: "foobar",
		}
		packer.controlFrames = []wire.Frame{&wire.WindowUpdateFrame{StreamID: 37}}
		streamFramer.AddFrameForRetransmission(&wire.StreamFrame{
			StreamID: 5,
			Data:     []byte("foobar"),
		})
		p, err := packer.PackConnectionClose(&ccf, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(&ccf))
	})

	It("packs only control frames", func() {
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(&wire.WindowUpdateFrame{}, pth)
		p, err := packer.PackPacket(pth)
		Expect(p).ToNot(BeNil())
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.raw).NotTo(BeEmpty())
	})

	It("packs only control frames", func() {
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(&wire.WindowUpdateFrame{}, pth)
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(p).ToNot(BeNil())
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.raw).NotTo(BeEmpty())
	})

	It("increases the packet number", func() {
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		p1, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p1).ToNot(BeNil())
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		p2, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p2).ToNot(BeNil())
		Expect(p2.number).To(BeNumerically(">", p1.number))
	})

	It("increases the packet number", func() {
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		p1, err := packer.PackPacketOfStream(pth, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(p1).ToNot(BeNil())
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		p2, err := packer.PackPacketOfStream(pth, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(p2).ToNot(BeNil())
		Expect(p2.number).To(BeNumerically(">", p1.number))
	})

	It("packs a StopWaitingFrame first", func() {
		pth.packetNumberGenerator.next = 15
		swf := &wire.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.frames[0]).To(Equal(swf))
	})

	It("packs a StopWaitingFrame first", func() {
		pth.packetNumberGenerator.next = 15
		swf := &wire.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.frames).To(HaveLen(2))
		Expect(p.frames[0]).To(Equal(swf))
	})

	It("sets the LeastUnackedDelta length of a StopWaitingFrame", func() {
		packetNumber := protocol.PacketNumber(0xDECAFB) // will result in a 4 byte packet number
		pth.packetNumberGenerator.next = packetNumber
		swf := &wire.StopWaitingFrame{LeastUnacked: packetNumber - 0x100}
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames[0].(*wire.StopWaitingFrame).PacketNumberLen).To(Equal(protocol.PacketNumberLen4))
	})

	It("sets the LeastUnackedDelta length of a StopWaitingFrame", func() {
		packetNumber := protocol.PacketNumber(0xDECAFB) // will result in a 4 byte packet number
		pth.packetNumberGenerator.next = packetNumber
		swf := &wire.StopWaitingFrame{LeastUnacked: packetNumber - 0x100}
		packer.QueueControlFrame(&wire.RstStreamFrame{}, pth)
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(p.frames[0].(*wire.StopWaitingFrame).PacketNumberLen).To(Equal(protocol.PacketNumberLen4))
	})

	It("does not pack a packet containing only a StopWaitingFrame", func() {
		swf := &wire.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacket(pth)
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})
	It("does not pack a packet containing only a StopWaitingFrame", func() {
		swf := &wire.StopWaitingFrame{LeastUnacked: 10}
		packer.QueueControlFrame(swf, pth)
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
	})

	It("packs a packet if it has queued control frames, but no new control frames", func() {
		packer.controlFrames = []wire.Frame{&wire.BlockedFrame{StreamID: 0}}
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
	})

	It("packs a packet if it has queued control frames, but no new control frames", func() {
		packer.controlFrames = []wire.Frame{&wire.BlockedFrame{StreamID: 0}}
		p, err := packer.PackPacketOfStream(pth, 1)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
	})

	It("adds the version flag to the public header before the crypto handshake is finished", func() {
		packer.perspective = protocol.PerspectiveClient
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
		packer.controlFrames = []wire.Frame{&wire.BlockedFrame{StreamID: 0}}
		packer.connectionID = 0x1337
		packer.version = 123
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		hdr, err := wire.ParsePublicHeader(bytes.NewReader(p.raw), protocol.PerspectiveClient, packer.version)
		Expect(err).ToNot(HaveOccurred())
		Expect(hdr.VersionFlag).To(BeTrue())
		Expect(hdr.VersionNumber).To(Equal(packer.version))
	})

	It("doesn't add the version flag to the public header for forward-secure packets", func() {
		packer.perspective = protocol.PerspectiveClient
		packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionForwardSecure
		packer.controlFrames = []wire.Frame{&wire.BlockedFrame{StreamID: 0}}
		packer.connectionID = 0x1337
		p, err := packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		hdr, err := wire.ParsePublicHeader(bytes.NewReader(p.raw), protocol.PerspectiveClient, packer.version)
		Expect(err).ToNot(HaveOccurred())
		Expect(hdr.VersionFlag).To(BeFalse())
	})

	It("packs many control frames into 1 packets", func() {
		f := &wire.AckFrame{LargestAcked: 1}
		b := &bytes.Buffer{}
		f.Write(b, protocol.VersionWhatever)
		maxFramesPerPacket := int(maxFrameSize) / b.Len()
		var controlFrames []wire.Frame
		for i := 0; i < maxFramesPerPacket; i++ {
			controlFrames = append(controlFrames, f)
		}
		packer.controlFrames = controlFrames
		payloadFrames, err := packer.composeNextPacket(maxFrameSize, false, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(maxFramesPerPacket))
		payloadFrames, err = packer.composeNextPacket(maxFrameSize, false, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(BeEmpty())
	})

	It("packs a lot of control frames into 2 packets if they don't fit into one", func() {
		blockedFrame := &wire.BlockedFrame{
			StreamID: 0x1337,
		}
		minLength, _ := blockedFrame.MinLength(0)
		maxFramesPerPacket := int(maxFrameSize) / int(minLength)
		var controlFrames []wire.Frame
		for i := 0; i < maxFramesPerPacket+10; i++ {
			controlFrames = append(controlFrames, blockedFrame)
		}
		packer.controlFrames = controlFrames
		payloadFrames, err := packer.composeNextPacket(maxFrameSize, false, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(maxFramesPerPacket))
		payloadFrames, err = packer.composeNextPacket(maxFrameSize, false, pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(payloadFrames).To(HaveLen(10))
	})

	It("only increases the packet number when there is an actual packet to send", func() {
		pth.packetNumberGenerator.nextToSkip = 1000
		p, err := packer.PackPacket(pth)
		Expect(p).To(BeNil())
		Expect(err).ToNot(HaveOccurred())
		Expect(pth.packetNumberGenerator.Peek()).To(Equal(protocol.PacketNumber(1)))
		f := &wire.StreamFrame{
			StreamID: 5,
			Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
		}
		streamFramer.AddFrameForRetransmission(f)
		p, err = packer.PackPacket(pth)
		Expect(err).ToNot(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.number).To(Equal(protocol.PacketNumber(1)))
		Expect(pth.packetNumberGenerator.Peek()).To(Equal(protocol.PacketNumber(2)))
	})

	Context("Stream Frame handling", func() {
		It("does not splits a stream frame with maximum size", func() {
			f := &wire.StreamFrame{
				Offset:         1,
				StreamID:       5,
				DataLenPresent: false,
			}
			minLength, _ := f.MinLength(0)
			maxStreamFrameDataLen := maxFrameSize - minLength
			f.Data = bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen))
			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(BeEmpty())
		})

		It("correctly handles a stream frame with one byte less than maximum size", func() {
			maxStreamFrameDataLen := maxFrameSize - (1 + 1 + 2) - 1
			f1 := &wire.StreamFrame{
				StreamID: 5,
				Offset:   1,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)),
			}
			f2 := &wire.StreamFrame{
				StreamID: 5,
				Offset:   1,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize - 1)))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			p, err = packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
		})

		It("packs multiple small stream frames into single packet", func() {
			f1 := &wire.StreamFrame{
				StreamID: 5,
				Data:     []byte{0xDE, 0xCA, 0xFB, 0xAD},
			}
			f2 := &wire.StreamFrame{
				StreamID: 5,
				Data:     []byte{0xBE, 0xEF, 0x13, 0x37},
			}
			f3 := &wire.StreamFrame{
				StreamID: 3,
				Data:     []byte{0xCA, 0xFE},
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			streamFramer.AddFrameForRetransmission(f3)
			p, err := packer.PackPacket(pth)
			Expect(p).ToNot(BeNil())
			Expect(err).ToNot(HaveOccurred())
			b := &bytes.Buffer{}
			f1.Write(b, 0)
			f2.Write(b, 0)
			f3.Write(b, 0)
			Expect(p.frames).To(HaveLen(3))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[1].(*wire.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[2].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(p.raw).To(ContainSubstring(string(f1.Data)))
			Expect(p.raw).To(ContainSubstring(string(f2.Data)))
			Expect(p.raw).To(ContainSubstring(string(f3.Data)))
		})

		It("pop stream frames on a path proportionally to priority", func() {

			// server side
			streamsMap := newStreamsMapPriority(nil, protocol.PerspectiveServer, nil)

			const (
				id1 = protocol.StreamID(2)
				id2 = protocol.StreamID(4)
				id3 = protocol.StreamID(1)
			)

			data1 := &wire.StreamFrame{
				StreamID: 2,
				Data:     []byte("foobar"),
			}

			data2 := &wire.StreamFrame{
				StreamID: 4,
				Data:     []byte("fooba"),
			}

			stream1 := &stream{streamID: id1, priority: nil}
			stream2 := &stream{streamID: id2, priority: nil}
			cryptoStream = &stream{streamID: id3, priority: nil}

			// normal data
			stream1.dataForWriting = data1.Data
			Expect(stream1.dataForWriting).ToNot(BeNil())
			stream2.dataForWriting = data2.Data
			Expect(stream2.dataForWriting).ToNot(BeNil())

			streamsMap.putStream(stream1)
			streamsMap.putStream(stream2)
			streamsMap.putStream(cryptoStream)
			streamsMap.sortStreamPriorityOrder()

			pth.streamIDs = append(pth.streamIDs, stream1.streamID)
			pth.streamIDs = append(pth.streamIDs, stream2.streamID)
			pth.streamIDs = append(pth.streamIDs, cryptoStream.streamID)

			mockFcm := mocks_fc.NewMockFlowControlManager(mockCtrl)

			packer.streamFramer = newStreamFramer(streamsMap, mockFcm)

			mockFcm.EXPECT().SendWindowSize(id1).Return(protocol.MaxByteCount, nil)
			mockFcm.EXPECT().AddBytesSent(id1, protocol.ByteCount(6))
			mockFcm.EXPECT().RemainingConnectionWindowSize().Return(protocol.MaxByteCount)
			//add retransmit data

			mockFcm.EXPECT().SendWindowSize(id2).Return(protocol.MaxByteCount, nil)
			mockFcm.EXPECT().AddBytesSent(id2, protocol.ByteCount(5))
			mockFcm.EXPECT().RemainingConnectionWindowSize().Return(protocol.MaxByteCount)

			//pack and check
			packet, err := packer.PackPacketOfPath(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(packet.raw).To(ContainSubstring(string(data1.Data)))

			// Expect(packet.frames).To(HaveLen(2))
			// pop retransmit frames before normal frames
			// Expect(packet.frames[1]).To(Equal(data1))

			// packet2, err := packer.PackPacketOfStream(pth, id2)
			// Expect(err).ToNot(HaveOccurred())
			// Expect(packet2.raw).To(ContainSubstring(string(data2.Data)))

		})

		PIt("packs stream frames (normal and retransmit) into single packet separately according to stream id", func() {

			// server side
			streamsMap := newStreamsMapPriority(nil, protocol.PerspectiveServer, nil)

			const (
				id1 = protocol.StreamID(2)
				id2 = protocol.StreamID(4)
				id3 = protocol.StreamID(1)
			)

			data1 := &wire.StreamFrame{
				StreamID: 2,
				Data:     []byte("foobar"),
			}

			data2 := &wire.StreamFrame{
				StreamID: 4,
				Data:     []byte("fooba"),
			}

			//retransmit data of streamID 2
			retransmittedFrame1 := &wire.StreamFrame{
				StreamID: 2,
				Data:     []byte{0x13, 0x37},
			}
			retransmittedFrame2 := &wire.StreamFrame{
				StreamID: 4,
				Data:     []byte{0x14, 0x38},
			}
			retransmittedFrame3 := &wire.StreamFrame{
				StreamID: 4,
				Data:     []byte{0x15, 0x39},
			}

			stream1 := &stream{streamID: id1}
			stream2 := &stream{streamID: id2}
			cryptoStream = &stream{streamID: id3}

			// normal data
			stream1.dataForWriting = data1.Data
			Expect(stream1.dataForWriting).ToNot(BeNil())
			stream2.dataForWriting = data2.Data
			Expect(stream2.dataForWriting).ToNot(BeNil())

			streamsMap.putStream(stream1)
			streamsMap.putStream(stream2)
			streamsMap.putStream(cryptoStream)

			mockFcm := mocks_fc.NewMockFlowControlManager(mockCtrl)

			packer.streamFramer = newStreamFramer(streamsMap, mockFcm)

			mockFcm.EXPECT().SendWindowSize(id1).Return(protocol.MaxByteCount, nil)
			mockFcm.EXPECT().AddBytesSent(id1, protocol.ByteCount(6))
			mockFcm.EXPECT().RemainingConnectionWindowSize().Return(protocol.MaxByteCount)
			//add retransmit data
			mockFcm.EXPECT().AddBytesRetrans(retransmittedFrame1.StreamID, retransmittedFrame1.DataLen())
			packer.streamFramer.AddFrameForRetransmission(retransmittedFrame1)

			mockFcm.EXPECT().SendWindowSize(id2).Return(protocol.MaxByteCount, nil)
			mockFcm.EXPECT().AddBytesSent(id2, protocol.ByteCount(5))
			mockFcm.EXPECT().RemainingConnectionWindowSize().Return(protocol.MaxByteCount)
			mockFcm.EXPECT().AddBytesRetrans(retransmittedFrame2.StreamID, retransmittedFrame2.DataLen())
			packer.streamFramer.AddFrameForRetransmission(retransmittedFrame2)
			mockFcm.EXPECT().AddBytesRetrans(retransmittedFrame3.StreamID, retransmittedFrame3.DataLen())
			packer.streamFramer.AddFrameForRetransmission(retransmittedFrame3)

			//pack and check
			packet, err := packer.PackPacketOfStream(pth, id1)
			Expect(err).ToNot(HaveOccurred())
			Expect(packet.raw).To(ContainSubstring(string(data1.Data)))
			Expect(packet.raw).To(ContainSubstring(string(retransmittedFrame1.Data)))
			Expect(packet.raw).ToNot(ContainSubstring(string(retransmittedFrame2.Data)))
			Expect(packet.raw).ToNot(ContainSubstring(string(retransmittedFrame3.Data)))
			Expect(packet.frames).To(HaveLen(2))
			//pop retransmit frames before normal frames
			Expect(packet.frames[0]).To(Equal(retransmittedFrame1))
			Expect(packet.frames[1]).To(Equal(data1))

			packet2, err := packer.PackPacketOfStream(pth, id2)
			Expect(err).ToNot(HaveOccurred())
			Expect(packet2.raw).To(ContainSubstring(string(data2.Data)))
			Expect(packet2.raw).ToNot(ContainSubstring(string(retransmittedFrame1.Data)))
			Expect(packet2.raw).To(ContainSubstring(string(retransmittedFrame2.Data)))
			Expect(packet2.raw).To(ContainSubstring(string(retransmittedFrame3.Data)))

		})
		It("splits one stream frame larger than maximum size", func() {
			f := &wire.StreamFrame{
				StreamID: 7,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			maxStreamFrameDataLen := maxFrameSize - minLength
			f.Data = bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+200)
			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(payloadFrames[0].(*wire.StreamFrame).Data).To(HaveLen(int(maxStreamFrameDataLen)))
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			Expect(payloadFrames[0].(*wire.StreamFrame).Data).To(HaveLen(200))
			Expect(payloadFrames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(BeEmpty())
		})

		It("packs 2 stream frames that are too big for one packet correctly", func() {
			maxStreamFrameDataLen := maxFrameSize - (1 + 1 + 2)
			f1 := &wire.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+100),
				Offset:   1,
			}
			f2 := &wire.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, int(maxStreamFrameDataLen)+100),
				Offset:   1,
			}
			streamFramer.AddFrameForRetransmission(f1)
			streamFramer.AddFrameForRetransmission(f2)
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
			p, err = packer.PackPacket(pth)
			Expect(p.frames).To(HaveLen(2))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeTrue())
			Expect(p.frames[1].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(err).ToNot(HaveOccurred())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
			p, err = packer.PackPacket(pth)
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
			Expect(err).ToNot(HaveOccurred())
			Expect(p).ToNot(BeNil())
			p, err = packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("packs a packet that has the maximum packet size when given a large enough stream frame", func() {
			f := &wire.StreamFrame{
				StreamID: 5,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			f.Data = bytes.Repeat([]byte{'f'}, int(maxFrameSize-minLength+1)) // + 1 since MinceLength is 1 bigger than the actual StreamFrame header
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).ToNot(BeNil())
			Expect(p.raw).To(HaveLen(int(protocol.MaxPacketSize)))
		})

		It("splits a stream frame larger than the maximum size", func() {
			f := &wire.StreamFrame{
				StreamID: 5,
				Offset:   1,
			}
			minLength, _ := f.MinLength(0)
			f.Data = bytes.Repeat([]byte{'f'}, int(maxFrameSize-minLength+2)) // + 2 since MinceLength is 1 bigger than the actual StreamFrame header

			streamFramer.AddFrameForRetransmission(f)
			payloadFrames, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
			payloadFrames, err = packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(payloadFrames).To(HaveLen(1))
		})

		It("refuses to send unencrypted stream data on a data stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionUnencrypted
			f := &wire.StreamFrame{
				StreamID: 3,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket(pth)
			Expect(err).NotTo(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("sends non forward-secure data as the client", func() {
			packer.perspective = protocol.PerspectiveClient
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
			f := &wire.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			Expect(p.frames[0]).To(Equal(f))
		})

		It("does not send non forward-secure data as the server", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionSecure
			f := &wire.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(BeNil())
		})

		It("sends unencrypted stream data on the crypto stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSealCrypto = protocol.EncryptionUnencrypted
			cryptoStream.dataForWriting = []byte("foobar")
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionUnencrypted))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0]).To(Equal(&wire.StreamFrame{StreamID: 1, Data: []byte("foobar")}))
		})

		It("sends encrypted stream data on the crypto stream", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSealCrypto = protocol.EncryptionSecure
			cryptoStream.dataForWriting = []byte("foobar")
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			Expect(p.frames).To(HaveLen(1))
			Expect(p.frames[0]).To(Equal(&wire.StreamFrame{StreamID: 1, Data: []byte("foobar")}))
		})

		It("does not pack stream frames if not allowed", func() {
			packer.cryptoSetup.(*mockCryptoSetup).encLevelSeal = protocol.EncryptionUnencrypted
			packer.QueueControlFrame(&wire.AckFrame{}, pth)
			streamFramer.AddFrameForRetransmission(&wire.StreamFrame{StreamID: 3, Data: []byte("foobar")})
			p, err := packer.PackPacket(pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(HaveLen(1))
			Expect(func() { _ = p.frames[0].(*wire.AckFrame) }).NotTo(Panic())
		})
	})

	Context("Blocked frames", func() {
		It("queues a BLOCKED frame", func() {
			length := 100
			streamFramer.blockedFrameQueue = []*wire.BlockedFrame{{StreamID: 5}}
			f := &wire.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, length),
			}
			streamFramer.AddFrameForRetransmission(f)
			_, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(packer.controlFrames[0]).To(Equal(&wire.BlockedFrame{StreamID: 5}))
		})

		It("removes the dataLen attribute from the last StreamFrame, even if it queued a BLOCKED frame", func() {
			length := 100
			streamFramer.blockedFrameQueue = []*wire.BlockedFrame{{StreamID: 5}}
			f := &wire.StreamFrame{
				StreamID: 5,
				Data:     bytes.Repeat([]byte{'f'}, length),
			}
			streamFramer.AddFrameForRetransmission(f)
			p, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).To(HaveLen(1))
			Expect(p[0].(*wire.StreamFrame).DataLenPresent).To(BeFalse())
		})

		It("packs a connection-level BlockedFrame", func() {
			streamFramer.blockedFrameQueue = []*wire.BlockedFrame{{StreamID: 0}}
			f := &wire.StreamFrame{
				StreamID: 5,
				Data:     []byte("foobar"),
			}
			streamFramer.AddFrameForRetransmission(f)
			_, err := packer.composeNextPacket(maxFrameSize, true, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(packer.controlFrames[0]).To(Equal(&wire.BlockedFrame{StreamID: 0}))
		})
	})

	It("returns nil if we only have a single STOP_WAITING", func() {
		packer.QueueControlFrame(&wire.StopWaitingFrame{}, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).NotTo(HaveOccurred())
		Expect(p).To(BeNil())
	})

	It("packs a single ACK", func() {
		ack := &wire.AckFrame{LargestAcked: 42}
		packer.QueueControlFrame(ack, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).NotTo(HaveOccurred())
		Expect(p).ToNot(BeNil())
		Expect(p.frames[0]).To(Equal(ack))
	})

	It("does not return nil if we only have a single ACK but request it to be sent", func() {
		ack := &wire.AckFrame{}
		packer.QueueControlFrame(ack, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).NotTo(HaveOccurred())
		Expect(p).ToNot(BeNil())
	})

	It("queues a control frame to be sent in the next packet", func() {
		wuf := &wire.WindowUpdateFrame{StreamID: 5}
		packer.QueueControlFrame(wuf, pth)
		p, err := packer.PackPacket(pth)
		Expect(err).NotTo(HaveOccurred())
		Expect(p.frames).To(HaveLen(1))
		Expect(p.frames[0]).To(Equal(wuf))
	})

	Context("retransmitting of handshake packets", func() {
		swf := &wire.StopWaitingFrame{LeastUnacked: 1}
		sf := &wire.StreamFrame{
			StreamID: 1,
			Data:     []byte("foobar"),
		}

		BeforeEach(func() {
			packer.QueueControlFrame(swf, pth)
		})

		It("packs a retransmission for a packet sent with no encryption", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionUnencrypted,
				Frames:          []wire.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(ContainElement(sf))
			Expect(p.frames).To(ContainElement(swf))
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionUnencrypted))
		})

		It("packs a retransmission for a packet sent with initial encryption", func() {
			nonce := bytes.Repeat([]byte{'e'}, 32)
			packer.cryptoSetup.(*mockCryptoSetup).divNonce = nonce
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames:          []wire.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.frames).To(ContainElement(sf))
			Expect(p.frames).To(ContainElement(swf))
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
			// a packet sent by the server with initial encryption contains the SHLO
			// it needs to have a diversification nonce
			Expect(p.raw).To(ContainSubstring(string(nonce)))
		})

		It("includes the diversification nonce on packets sent with initial encryption", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames:          []wire.Frame{sf},
			}
			p, err := packer.PackHandshakeRetransmission(packet, pth)
			Expect(err).ToNot(HaveOccurred())
			Expect(p.encryptionLevel).To(Equal(protocol.EncryptionSecure))
		})

		// this should never happen, since non forward-secure packets are limited to a size smaller than MaxPacketSize, such that it is always possible to retransmit them without splitting the StreamFrame
		// (note that the retransmitted packet needs to have enough space for the StopWaitingFrame)
		It("refuses to send a packet larger than MaxPacketSize", func() {
			packet := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
				Frames: []wire.Frame{
					&wire.StreamFrame{
						StreamID: 1,
						Data:     bytes.Repeat([]byte{'f'}, int(protocol.MaxPacketSize-5)),
					},
				},
			}
			_, err := packer.PackHandshakeRetransmission(packet, pth)
			Expect(err).To(MatchError("PacketPacker BUG: packet too large"))
		})

		It("refuses to retransmit packets that were sent with forward-secure encryption", func() {
			p := &ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionForwardSecure,
			}
			_, err := packer.PackHandshakeRetransmission(p, pth)
			Expect(err).To(MatchError("PacketPacker BUG: forward-secure encrypted handshake packets don't need special treatment"))
		})

		It("refuses to retransmit packets without a StopWaitingFrame", func() {
			packer.stopWaiting = nil
			_, err := packer.PackHandshakeRetransmission(&ackhandler.Packet{
				EncryptionLevel: protocol.EncryptionSecure,
			}, pth)
			Expect(err).To(MatchError("PacketPacker BUG: Handshake retransmissions must contain a StopWaitingFrame"))
		})
	})

	Context("packing ACK packets", func() {
		It("packs ACK packets", func() {
			packer.QueueControlFrame(&wire.AckFrame{}, pth)
			p, err := packer.PackAckPacket(pth)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.frames).To(Equal([]wire.Frame{&wire.AckFrame{DelayTime: math.MaxInt64}}))
		})

		It("packs ACK packets with SWFs", func() {
			packer.QueueControlFrame(&wire.AckFrame{}, pth)
			packer.QueueControlFrame(&wire.StopWaitingFrame{}, pth)
			p, err := packer.PackAckPacket(pth)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.frames).To(Equal([]wire.Frame{
				&wire.AckFrame{DelayTime: math.MaxInt64},
				&wire.StopWaitingFrame{PacketNumber: 1, PacketNumberLen: 2},
			}))
		})
	})
})
