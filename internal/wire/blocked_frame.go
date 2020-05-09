package wire

import (
	"bytes"

	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
)

// A BlockedFrame in QUIC
type BlockedFrame struct {
	StreamID protocol.StreamID
}

//Write writes a BlockedFrame frame
func (f *BlockedFrame) Write(b *bytes.Buffer, version protocol.VersionNumber) error {
	b.WriteByte(0x05)
	utils.GetByteOrder(version).WriteUint32(b, uint32(f.StreamID))
	return nil
}

// MinLength of a written frame
func (f *BlockedFrame) MinLength(version protocol.VersionNumber) (protocol.ByteCount, error) {
	return 1 + 4, nil
}

// ParseBlockedFrame parses a BLOCKED frame
func ParseBlockedFrame(r *bytes.Reader, version protocol.VersionNumber) (*BlockedFrame, error) {
	frame := &BlockedFrame{}

	// read the TypeByte
	if _, err := r.ReadByte(); err != nil {
		return nil, err
	}
	sid, err := utils.GetByteOrder(version).ReadUint32(r)
	if err != nil {
		return nil, err
	}
	frame.StreamID = protocol.StreamID(sid)
	return frame, nil
}
