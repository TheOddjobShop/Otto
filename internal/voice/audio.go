package voice

import (
	"bytes"
	"encoding/binary"
	"math"
)

// Audio primitives. All pure functions over sample slices, so the parts of the
// capture pipeline that decide *what* is speech are testable without a
// microphone — only the sox subprocess that produces the samples is not.

const (
	// sampleRate is 16 kHz mono, which is what whisper.cpp expects and what
	// piper produces. Resampling anywhere in the chain would be pure loss.
	sampleRate = 16000

	// frameSamples is one 100 ms frame. Small enough that the level meter
	// updates smoothly (10 Hz) and the VAD reacts promptly, large enough that
	// per-frame overhead is irrelevant.
	frameSamples = 1600
)

// rms returns a frame's root-mean-square amplitude normalized to [0,1].
// This is the whole voice-activity signal: cheap, and adequate because the
// threshold it feeds adapts to the room's noise floor.
func rms(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		f := float64(s)
		sum += f * f
	}
	return math.Sqrt(sum/float64(len(frame))) / 32768.0
}

// pcmToWav wraps signed 16-bit little-endian mono PCM in a WAV container.
//
// whisper.cpp will not read raw PCM, and shelling out to sox or ffmpeg for the
// conversion would add a subprocess to every single utterance. The header is 44
// fixed bytes; writing it directly is both faster and one less dependency.
func pcmToWav(samples []int16, sr int) []byte {
	numBytes := len(samples) * 2
	var b bytes.Buffer
	b.Grow(44 + numBytes)

	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+numBytes))
	b.WriteString("WAVE")

	b.WriteString("fmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16)) // subchunk size
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))  // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(sr))
	_ = binary.Write(&b, binary.LittleEndian, uint32(sr*2)) // byte rate
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))    // block align
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))   // bits per sample

	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(numBytes))
	_ = binary.Write(&b, binary.LittleEndian, samples)
	return b.Bytes()
}

// frameRing is a fixed-length FIFO of recent frames, used to prepend audio from
// just *before* speech was detected.
//
// Without it every utterance loses its first syllable: the VAD can only notice
// speech after a frame has already been captured, so the onset — which for a
// wake word is most of the word — would be clipped.
type frameRing struct {
	buf [][]int16
	max int
}

func newFrameRing(n int) *frameRing {
	if n < 1 {
		n = 1
	}
	return &frameRing{buf: make([][]int16, 0, n), max: n}
}

func (r *frameRing) push(f []int16) {
	if len(r.buf) == r.max {
		r.buf = r.buf[1:]
	}
	r.buf = append(r.buf, f)
}

// drain returns the buffered frames and empties the ring.
func (r *frameRing) drain() [][]int16 {
	out := r.buf
	r.buf = make([][]int16, 0, r.max)
	return out
}

func (r *frameRing) reset() { r.buf = r.buf[:0] }
