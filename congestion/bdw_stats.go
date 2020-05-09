package congestion

import (
	"time"

	"github.com/lucas-clemente/pstream/internal/protocol"
)

// BDWStats provides estimated bandwidth statistics
type BDWStats struct {
	bandwidth       Bandwidth //  bit per second
	compareWindow   [10]Bandwidth
	roundRobinIndex uint8 //  resume where ended
}

// NewBDWStats makes a properly initialized BDWStats object
func NewBDWStats(bandwidth Bandwidth) *BDWStats {
	return &BDWStats{
		bandwidth: bandwidth,
	}
}

//GetBandwidth returns estimated bandwidth in Mbps
func (b *BDWStats) GetBandwidth() Bandwidth { return b.bandwidth / Bandwidth(1048576) }

// UpdateBDW updates the bandwidth based on a new sample.
func (b *BDWStats) UpdateBDW(sentDelta protocol.ByteCount, sentDelay time.Duration) {
	disable := true
	if !disable {

		bdw := Bandwidth(sentDelta) * Bandwidth(time.Second) / Bandwidth(sentDelay) * BytesPerSecond
		size := uint8(len(b.compareWindow))
		startIndex := b.roundRobinIndex
		b.compareWindow[(startIndex)%size] = bdw

		b.roundRobinIndex = (b.roundRobinIndex + 1) % size

		for i := uint8(0); i < size; i++ {

			if b.bandwidth < b.compareWindow[i] {
				b.bandwidth = b.compareWindow[i]
			}
		}

	}
}
