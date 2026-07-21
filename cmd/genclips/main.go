// Command genclips synthesizes a few spoken "answer" clips with the same Kokoro
// engine, so a browser demo can feed them through a fake microphone. One-off.
package main

import (
	"log"
	"os"
	"path/filepath"

	"voicesurvey/internal/speech"
)

func main() {
	eng, err := speech.NewEngine("models")
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close()
	eng.SetVoice(9) // a different voice than the agent, so it sounds like "the respondent"

	out := "web/static/demo"
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}
	clips := map[string]string{
		"ans0.wav": "I really like them. The scent is relaxing and they last a long time. I'd definitely recommend them to a friend.",
		"ans1.wav": "I usually burn one in the evening while I'm reading, maybe three or four times a week.",
		"ans2.wav": "I love lavender and vanilla the most. Something warm and calming for the living room.",
		"bail.wav":  "Actually, I need to go now. Thanks, but I don't have time for any more questions.",
	}
	for name, text := range clips {
		wav, err := eng.Synthesize(text)
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		p := filepath.Join(out, name)
		if err := os.WriteFile(p, wav, 0o644); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s (%d bytes)", p, len(wav))
	}
}
