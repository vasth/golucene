package util

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestByteCount(t *testing.T) {
	rand.Seed(time.Now().UnixNano())

	inters := 10 // >= 3
	for i := 0; i < inters; i++ {
		valueCount := rand.Int31n(math.MaxInt32-1) + 1 // [1, 1^32-1]
		for j := 0; j <= 1; j++ {
			format := PackedFormat(j)
			for bpv := uint32(1); bpv <= 64; bpv++ {
				byteCount := format.ByteCount(PACKED_VERSION_CURRENT, valueCount, bpv)
				msg := fmt.Sprintf("format=%v, byteCount=%v, valueCount=%v, bpv=%v", format, byteCount, valueCount, bpv)
				if byteCount*8 < int64(valueCount)*int64(bpv) {
					t.Errorf(msg)
				}
				if format == PACKED {
					if (byteCount-1)*8 >= int64(valueCount)*int64(bpv) {
						t.Errorf(msg)
					}
				}
			}
		}
	}
}
