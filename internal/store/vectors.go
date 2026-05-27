package store

import (
	"encoding/binary"
	"math"
)

// encodeVec serializes a float32 vector as little-endian IEEE-754 bytes
// (4 bytes per element) for BLOB storage.
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec is the inverse of encodeVec. A nil/empty slice decodes to an
// empty vector. Trailing bytes that don't form a full float32 are ignored.
func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
