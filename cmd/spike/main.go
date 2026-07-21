// Command spike is the Phase 0 proof: it verifies that Kokoro TTS and
// Whisper STT both load and run from Go via sherpa-onnx on this machine.
//
// It (1) synthesizes a sentence with Kokoro, (2) transcribes a known 16kHz
// test clip with Whisper, and (3) round-trips the synthesized audio back
// through Whisper. If all three print sane output, the backbone is proven.
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"
)

const modelsDir = "models"

func main() {
	fmt.Println("=== Phase 0 spike: sherpa-onnx STT + TTS on", "darwin/arm64 ===")

	// ---- TTS: Kokoro ----
	kokoro := filepath.Join(modelsDir, "kokoro-en-v0_19")
	ttsCfg := &sherpa.OfflineTtsConfig{
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
		MaxNumSentences: 1,
	}
	t0 := time.Now()
	tts := sherpa.NewOfflineTts(ttsCfg)
	if tts == nil {
		fmt.Println("FAIL: NewOfflineTts returned nil (check kokoro paths)")
		os.Exit(1)
	}
	defer sherpa.DeleteOfflineTts(tts)
	fmt.Printf("TTS loaded in %v | sampleRate=%d numSpeakers=%d\n",
		time.Since(t0).Round(time.Millisecond), tts.SampleRate(), tts.NumSpeakers())

	text := "Hi! Thanks for taking a moment to share your thoughts about our candles."
	t0 = time.Now()
	audio := tts.Generate(text, 0, 1.0)
	if audio == nil || len(audio.Samples) == 0 {
		fmt.Println("FAIL: TTS produced no audio")
		os.Exit(1)
	}
	genDur := time.Since(t0)
	audioSecs := float64(len(audio.Samples)) / float64(audio.SampleRate)
	outWav := filepath.Join(os.TempDir(), "spike_tts.wav")
	audio.Save(outWav)
	fmt.Printf("TTS generated %.2fs of audio in %v (%.1fx realtime) -> %s\n",
		audioSecs, genDur.Round(time.Millisecond), audioSecs/genDur.Seconds(), outWav)

	// ---- STT: Whisper base.en ----
	whisper := filepath.Join(modelsDir, "sherpa-onnx-whisper-base.en")
	recCfg := &sherpa.OfflineRecognizerConfig{
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
	}
	t0 = time.Now()
	rec := sherpa.NewOfflineRecognizer(recCfg)
	if rec == nil {
		fmt.Println("FAIL: NewOfflineRecognizer returned nil (check whisper paths)")
		os.Exit(1)
	}
	defer sherpa.DeleteOfflineRecognizer(rec)
	fmt.Printf("STT loaded in %v\n", time.Since(t0).Round(time.Millisecond))

	// (a) known 16kHz test clip
	testWav := filepath.Join(whisper, "test_wavs", "0.wav")
	if samples, sr, err := readWavPCM16(testWav); err == nil {
		fmt.Printf("STT test clip -> %q\n", transcribe(rec, sr, samples))
	} else {
		fmt.Printf("(skipped test clip: %v)\n", err)
	}

	// (b) round-trip: transcribe what we just synthesized
	t0 = time.Now()
	rt := transcribe(rec, audio.SampleRate, audio.Samples)
	fmt.Printf("STT round-trip (%v) -> %q\n", time.Since(t0).Round(time.Millisecond), rt)

	fmt.Println("=== spike OK ===")
}

func transcribe(rec *sherpa.OfflineRecognizer, sampleRate int, samples []float32) string {
	stream := sherpa.NewOfflineStream(rec)
	defer sherpa.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples)
	rec.Decode(stream)
	return stream.GetResult().Text
}

// readWavPCM16 parses a mono 16-bit PCM WAV into normalized float32 samples.
func readWavPCM16(path string) ([]float32, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a RIFF/WAVE file")
	}
	sampleRate := int(binary.LittleEndian.Uint32(b[24:28]))
	// find the "data" chunk
	i := 12
	for i+8 <= len(b) {
		id := string(b[i : i+4])
		sz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			end := body + sz
			if end > len(b) {
				end = len(b)
			}
			pcm := b[body:end]
			out := make([]float32, len(pcm)/2)
			for j := 0; j+1 < len(pcm); j += 2 {
				s := int16(binary.LittleEndian.Uint16(pcm[j : j+2]))
				out[j/2] = float32(s) / 32768.0
			}
			return out, sampleRate, nil
		}
		i = body + sz
	}
	return nil, 0, fmt.Errorf("no data chunk")
}
