// Command probe drives the voice conversation over WebSocket without a browser
// or microphone, so the full server loop (STT -> LLM classify -> state machine
// -> TTS -> protocol -> ending) can be verified headlessly.
//
// It creates a poll, connects, and for each agent turn that expects a reply it
// sends a canned 16kHz utterance (a real speech clip), then reports the flow.
//
// Usage: go run ./cmd/probe -addr localhost:8090 -mode happy
//
//	-mode happy  : answer every question until the survey completes
//	-mode silent : never answer, exercising the silence backstop
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	addr := flag.String("addr", "localhost:8090", "server host:port")
	product := flag.String("product", "scented soy candles for the home", "product to poll")
	mode := flag.String("mode", "happy", "happy | silent")
	wavPath := flag.String("wav", "models/sherpa-onnx-whisper-base.en/test_wavs/0.wav", "16kHz PCM16 wav used as canned answers")
	maxTurns := flag.Int("max", 20, "safety cap on agent turns")
	flag.Parse()

	utterance, err := wavData(*wavPath)
	if err != nil {
		log.Fatalf("read wav: %v", err)
	}

	// 1) create poll
	id := createPoll(*addr, *product)
	fmt.Printf("poll created: %s\n", id)

	// 2) connect
	c, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/ws?poll=%s", *addr, id), nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.WriteJSON(map[string]string{"type": "ready"})

	turns := 0
	lastKind := ""
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			fmt.Printf("connection closed: %v\n", err)
			return
		}
		if mt == websocket.BinaryMessage {
			continue // a turn streams several audio chunks; wait for tts_end
		}

		var m struct {
			Type, Text, Kind, Reason string
			Index, Total             int
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "agent_say":
			lastKind = m.Kind
			pos := ""
			if m.Total > 0 {
				pos = fmt.Sprintf(" [%d/%d]", m.Index, m.Total)
			}
			fmt.Printf("AGENT(%s)%s: %s\n", m.Kind, pos, m.Text)
		case "tts_end":
			// The agent finished this turn's audio. Ack playback.
			_ = c.WriteJSON(map[string]string{"type": "playback_done"})
			if lastKind == "closing" {
				continue // no reply expected; a "done" will arrive
			}
			turns++
			if turns > *maxTurns {
				log.Fatalf("exceeded max turns (%d) without ending", *maxTurns)
			}
			if *mode == "silent" {
				fmt.Println("  · (staying silent)")
				continue // exercise the silence backstop
			}
			// Mirror the browser client: signal "speaking" (resets the silence
			// timer) before delivering the utterance.
			time.Sleep(250 * time.Millisecond)
			_ = c.WriteJSON(map[string]string{"type": "speaking"})
			fmt.Println("  → sending canned answer")
			_ = c.WriteMessage(websocket.BinaryMessage, utterance)
		case "transcript":
			fmt.Printf("  heard: %q\n", m.Text)
		case "done":
			fmt.Printf("\n=== DONE, reason=%s ===\n", m.Reason)
			return
		}
	}
}

func createPoll(addr, product string) string {
	body, _ := json.Marshal(map[string]string{"product": product})
	r, err := http.Post("http://"+addr+"/api/polls", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("create poll: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		log.Fatalf("create poll status %d", r.StatusCode)
	}
	var out struct {
		ID        string   `json:"id"`
		Questions []string `json:"questions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&out)
	fmt.Printf("questions:\n")
	for i, q := range out.Questions {
		fmt.Printf("  %d. %s\n", i+1, q)
	}
	return out.ID
}

// wavData returns the raw PCM16 sample bytes from the data chunk of a WAV file.
func wavData(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a WAV file")
	}
	for i := 12; i+8 <= len(b); {
		id := string(b[i : i+4])
		sz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		if id == "data" {
			end := body + sz
			if end > len(b) {
				end = len(b)
			}
			return b[body:end], nil
		}
		i = body + sz
	}
	return nil, fmt.Errorf("no data chunk")
}
