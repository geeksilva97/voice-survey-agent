// Package speech wraps sherpa-onnx for local STT (Whisper base.en) and
// TTS (Kokoro-82M). One Engine holds both models; calls are serialized with
// a mutex because the underlying sherpa objects are not concurrency-safe.
// For a single-respondent-at-a-time PoC this is fine.
package speech

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"sync"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"
)

// Engine bundles the loaded TTS and STT models.
type Engine struct {
	mu  sync.Mutex
	tts *sherpa.OfflineTts
	rec *sherpa.OfflineRecognizer

	ttsSampleRate int
	voiceID       int
}

// NewEngine loads Kokoro (TTS) and Whisper base.en (STT) from modelsDir.
func NewEngine(modelsDir string) (*Engine, error) {
	kokoro := filepath.Join(modelsDir, "kokoro-en-v0_19")
	tts := sherpa.NewOfflineTts(&sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{
			Kokoro: sherpa.OfflineTtsKokoroModelConfig{
				Model:   filepath.Join(kokoro, "model.onnx"),
				Voices:  filepath.Join(kokoro, "voices.bin"),
				Tokens:  filepath.Join(kokoro, "tokens.txt"),
				DataDir: filepath.Join(kokoro, "espeak-ng-data"),
			},
			NumThreads: 2,
			Provider:   "cpu",
		},
		MaxNumSentences: 1, // ignored by Kokoro; set to 1 to avoid a noisy warning
	})
	if tts == nil {
		return nil, fmt.Errorf("failed to load Kokoro TTS from %s", kokoro)
	}

	whisper := filepath.Join(modelsDir, "sherpa-onnx-whisper-base.en")
	rec := sherpa.NewOfflineRecognizer(&sherpa.OfflineRecognizerConfig{
		ModelConfig: sherpa.OfflineModelConfig{
			Whisper: sherpa.OfflineWhisperModelConfig{
				Encoder: filepath.Join(whisper, "base.en-encoder.int8.onnx"),
				Decoder: filepath.Join(whisper, "base.en-decoder.int8.onnx"),
			},
			Tokens:     filepath.Join(whisper, "base.en-tokens.txt"),
			NumThreads: 2,
			Provider:   "cpu",
			ModelType:  "whisper",
		},
	})
	if rec == nil {
		sherpa.DeleteOfflineTts(tts)
		return nil, fmt.Errorf("failed to load Whisper STT from %s", whisper)
	}

	return &Engine{
		tts:           tts,
		rec:           rec,
		ttsSampleRate: tts.SampleRate(),
		voiceID:       0, // Kokoro voice/speaker id; 0 = af (default US female)
	}, nil
}

// Close frees the native models.
func (e *Engine) Close() {
	if e.tts != nil {
		sherpa.DeleteOfflineTts(e.tts)
	}
	if e.rec != nil {
		sherpa.DeleteOfflineRecognizer(e.rec)
	}
}

// SetVoice picks the Kokoro speaker id (0..NumSpeakers-1).
func (e *Engine) SetVoice(id int) { e.voiceID = id }

// Synthesize renders text to a mono 16-bit PCM WAV (ready to hand to the
// browser as a binary blob for decodeAudioData / <audio>).
func (e *Engine) Synthesize(text string) ([]byte, error) {
	return e.SynthesizeVoice(text, e.voiceID)
}

// SynthesizeVoice renders text with a SPECIFIC Kokoro voice id without changing
// the engine's default voice. Used by the QA harness so each simulated persona
// can speak in a distinct voice while the agent keeps its own. Falls back to the
// default voice if the requested id yields no audio.
func (e *Engine) SynthesizeVoice(text string, voiceID int) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	audio := e.tts.Generate(text, voiceID, 1.0)
	if audio == nil || len(audio.Samples) == 0 {
		if voiceID != e.voiceID {
			audio = e.tts.Generate(text, e.voiceID, 1.0)
		}
		if audio == nil || len(audio.Samples) == 0 {
			return nil, fmt.Errorf("tts produced no audio for %q", text)
		}
	}
	return encodeWAV(audio.Samples, audio.SampleRate), nil
}

// Silence returns a WAV of ms milliseconds of silence at the TTS sample rate.
// It's used to insert a deliberate pause between two spoken beats (an
// acknowledgment, then the question): Kokoro has no SSML, so the gap is baked
// into the audio stream as a silent buffer the client plays like any other,
// keeping the whole turn a single continuous playback.
func (e *Engine) Silence(ms int) []byte {
	if ms < 0 {
		ms = 0
	}
	n := e.ttsSampleRate * ms / 1000
	return encodeWAV(make([]float32, n), e.ttsSampleRate)
}

// Transcribe decodes mono PCM16 samples (any sample rate; sherpa resamples
// internally) into text. Pass the sample rate the browser captured at (16k).
func (e *Engine) Transcribe(pcm16 []byte, sampleRate int) string {
	if len(pcm16) < 2 {
		return ""
	}
	samples := pcm16ToFloat32(pcm16)
	e.mu.Lock()
	defer e.mu.Unlock()
	stream := sherpa.NewOfflineStream(e.rec)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples)
	e.rec.Decode(stream)
	return stream.GetResult().Text
}

// pcm16ToFloat32 converts little-endian mono PCM16 to normalized float32.
func pcm16ToFloat32(pcm []byte) []float32 {
	out := make([]float32, len(pcm)/2)
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		out[i/2] = float32(s) / 32768.0
	}
	return out
}

// encodeWAV wraps normalized float32 samples in a mono 16-bit PCM WAV container.
func encodeWAV(samples []float32, sampleRate int) []byte {
	const bitsPerSample = 16
	const numChannels = 1
	dataLen := len(samples) * 2
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8

	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], numChannels)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))

	for i, s := range samples {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(int16(s*32767)))
	}
	return buf
}
