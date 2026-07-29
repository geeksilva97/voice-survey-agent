package main

import (
	"strings"
	"unicode"

	"voicesurvey/internal/ws"
)

// The scorers here are all DETERMINISTIC on purpose. The defect this harness was
// built to measure — the agent opening its farewell with the respondent's own
// filler ("Sure thing, thanks for sharing!") — is exactly string-detectable, so
// putting an LLM judge in front of it would only add noise and cost between us
// and the number. Judgment-shaped criteria (groundedness, specificity) are a
// separate, later, ungated concern.

// FillerOpener is deliberately NOT reimplemented here — it delegates to
// internal/ws, which is also where the production guard lives.
//
// Two copies of the filler list would drift, and the failure would be silent in
// the worst possible way: the eval would report a clean opener while a phrase the
// guard doesn't strip still reached the respondent's ears, or vice versa. One
// definition means the measurement and the repair cannot disagree.
func FillerOpener(line string) string { return ws.FillerOpener(line) }

// WordCount counts spoken words, ignoring punctuation-only tokens so "—" in
// "lavender — take care" doesn't inflate the count against the 35-word rule.
func WordCount(line string) int {
	n := 0
	for _, f := range strings.Fields(line) {
		if strings.IndexFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			n++
		}
	}
	return n
}

// HasQuestion reports whether the line contains a question mark. ClosingSystem
// forbids questions — a farewell that asks something invites a reply the agent is
// about to stop listening for.
func HasQuestion(line string) bool { return strings.Contains(line, "?") }

// normalize lowercases and replaces every non-alphanumeric rune with a space, so
// word comparisons ignore punctuation and casing.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String()
}

// agentVocabulary is the closing line's own register — words the agent legitimately
// uses regardless of what the respondent said. Anything here is exempt from the
// unsupported-word check below.
var agentVocabulary = map[string]bool{
	// gratitude and farewell
	"thanks": true, "thank": true, "you": true, "your": true, "yours": true,
	"much": true, "so": true, "very": true, "really": true, "truly": true,
	"take": true, "care": true, "have": true, "a": true, "great": true, "good": true,
	"wonderful": true, "lovely": true, "nice": true, "day": true, "evening": true,
	"time": true, "today": true, "bye": true, "goodbye": true, "cheers": true,
	// reacting
	"glad": true, "love": true, "loved": true, "hear": true, "hearing": true,
	"sounds": true, "sound": true, "like": true, "for": true, "sharing": true,
	"share": true, "shared": true, "feedback": true, "thoughts": true, "input": true,
	"noted": true, "got": true, "it": true, "that": true, "this": true, "the": true,
	"and": true, "with": true, "to": true, "of": true, "in": true, "on": true,
	"is": true, "was": true, "are": true, "were": true, "be": true, "been": true,
	"i": true, "we": true, "us": true, "our": true, "me": true, "my": true,
	"appreciate": true, "chatting": true, "chat": true, "talking": true, "talk": true,
	"letting": true, "know": true, "know's": true, "mentioned": true, "said": true,
	"about": true, "too": true, "also": true, "but": true, "as": true, "at": true,
	"they": true, "them": true, "their": true, "there": true, "here": true,
	"one": true, "some": true, "more": true, "most": true, "out": true, "up": true,
	"if": true, "when": true, "how": true, "what": true, "all": true, "well": true,
	"do": true, "does": true, "did": true, "will": true, "would": true, "can": true,
	"its": true, "it's": true, "you're": true, "i'd": true, "i'm": true, "that's": true,
	"hope": true, "helps": true, "help": true, "helpful": true, "honest": true,
}

// UnsupportedWords returns content words present in the closing line but absent
// from BOTH the respondent's answers and the agent's own register.
//
// This is a cheap groundedness proxy, and it exists because of a real regression:
// a first attempt at fixing the filler-opener defect added an example closing
// ("Lavender for the bedroom — lovely.") to the prompt, and the 3B model copied it
// into all 13 cases — telling a respondent who said only "Vanilla." about lavender
// and bedrooms. Every deterministic score stayed at 100% while the output got much
// worse, because nothing was checking whether the words were EARNED.
//
// Reported, never gated: a legitimate paraphrase ("scent" for "smell") shows up
// here too, so it is a signal to look at rather than a verdict.
func UnsupportedWords(line string, answers []string) []string {
	supported := map[string]bool{}
	for _, w := range strings.Fields(normalize(strings.Join(answers, " "))) {
		supported[w] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(normalize(line)) {
		if len(w) < 4 || supported[w] || agentVocabulary[w] || seen[w] {
			continue
		}
		// Tolerate simple morphology against the transcript ("scents"/"scent").
		if supported[strings.TrimSuffix(w, "s")] || supported[w+"s"] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// LongestCopiedSpan returns the length (in words) of the longest run of
// consecutive words the closing line shares with the respondent's answers, plus
// the span itself.
//
// ClosingSystem asks for "their idea, not their exact words", and this is the
// measurable form of that. It is reported rather than gated: a long shared span
// can be legitimate when the respondent used the only sensible wording for a
// product feature, so it is a signal to look at, not a verdict.
func LongestCopiedSpan(line string, answers []string) (int, string) {
	words := strings.Fields(normalize(line))
	hay := " " + strings.Join(strings.Fields(normalize(strings.Join(answers, " "))), " ") + " "
	bestN, bestText := 0, ""
	for i := range words {
		for j := i + 1; j <= len(words); j++ {
			span := strings.Join(words[i:j], " ")
			if !strings.Contains(hay, " "+span+" ") {
				break // extending can only fail too
			}
			if n := j - i; n > bestN {
				bestN, bestText = n, span
			}
		}
	}
	return bestN, bestText
}
