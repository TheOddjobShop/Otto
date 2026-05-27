package store

import (
	"testing"
)

func TestEncodeDecodeVecRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, 3.14159, -2.71828}
	out := decodeVec(encodeVec(in))
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("out[%d] = %v, want %v", i, out[i], in[i])
		}
	}
}

func TestEncodeVecLengthIsFourBytesPerFloat(t *testing.T) {
	if got := len(encodeVec([]float32{1, 2, 3})); got != 12 {
		t.Fatalf("encoded length = %d, want 12", got)
	}
}

func TestDecodeEmptyIsEmpty(t *testing.T) {
	if got := decodeVec(nil); len(got) != 0 {
		t.Fatalf("decode(nil) len = %d, want 0", len(got))
	}
}
