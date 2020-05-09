package wire

import (
	"bytes"
	"encoding/binary"
	"errors"

	"github.com/lucas-clemente/pstream/internal/handshake"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
)

// A PublicReset is a PUBLIC_RESET
type PublicReset struct {
	RejectedPacketNumber protocol.PacketNumber
	Nonce                uint64
}

// WritePublicReset writes a Public Reset
func WritePublicReset(connectionID protocol.ConnectionID, rejectedPacketNumber protocol.PacketNumber, nonceProof uint64) []byte {
	// TODO (QDC): a public reset should also contains the path ID
	b := &bytes.Buffer{}
	b.WriteByte(0x0a)
	utils.LittleEndian.WriteUint64(b, uint64(connectionID))
	utils.LittleEndian.WriteUint32(b, uint32(handshake.TagPRST))
	utils.LittleEndian.WriteUint32(b, 2)
	utils.LittleEndian.WriteUint32(b, uint32(handshake.TagRNON))
	utils.LittleEndian.WriteUint32(b, 8)
	utils.LittleEndian.WriteUint32(b, uint32(handshake.TagRSEQ))
	utils.LittleEndian.WriteUint32(b, 16)
	utils.LittleEndian.WriteUint64(b, nonceProof)
	utils.LittleEndian.WriteUint64(b, uint64(rejectedPacketNumber))
	return b.Bytes()
}

// ParsePublicReset parses a Public Reset
func ParsePublicReset(r *bytes.Reader) (*PublicReset, error) {
	// TODO (QDC): a public reset should also contains the path ID
	pr := PublicReset{}
	msg, err := handshake.ParseHandshakeMessage(r)
	if err != nil {
		return nil, err
	}
	if msg.Tag != handshake.TagPRST {
		return nil, errors.New("wrong public reset tag")
	}

	rseq, ok := msg.Data[handshake.TagRSEQ]
	if !ok {
		return nil, errors.New("RSEQ missing")
	}
	if len(rseq) != 8 {
		return nil, errors.New("invalid RSEQ tag")
	}
	pr.RejectedPacketNumber = protocol.PacketNumber(binary.LittleEndian.Uint64(rseq))

	rnon, ok := msg.Data[handshake.TagRNON]
	if !ok {
		return nil, errors.New("RNON missing")
	}
	if len(rnon) != 8 {
		return nil, errors.New("invalid RNON tag")
	}
	pr.Nonce = binary.LittleEndian.Uint64(rnon)

	return &pr, nil
}
