package ws

import "strings"

// Filler-opener guard for the closing line.
//
// Measured defect (cmd/evalclosing, baseline a2844dfca273): on transcripts where
// the respondent opened every answer with a discourse marker, the agent led its
// farewell with the same marker half the time — "Sure thing, thanks for sharing!"
// after a respondent who kept saying "Sure thing, …". Those phrases answer or
// continue a conversation; they do not close one.
//
// This is the same shape of problem as llm.IsNonSpeechArtifact: a prompt rule the
// small model ignores often enough to matter, and cheaply checkable in code. So it
// gets the same treatment — a deterministic guard that holds regardless of model,
// with the prompt rule as the first line of defence rather than the only one.
//
// It REPAIRS rather than rejects. In every observed failure the rest of the line
// was fine, so emptying it into the fixed-close fallback would throw away a good
// personalized farewell to fix one word.

// fillerOpeners are discourse markers and acknowledgment tokens that answer or
// continue a conversation rather than end one.
//
// Deliberately excludes words that open a farewell perfectly well — "great",
// "glad", "thanks", "love" — because the guard must not flatten legitimate
// variety. Everything here is unambiguously conversational filler.
var fillerOpeners = []string{
	"sure thing", "sure", "of course", "no worries", "no problem",
	"you know", "i mean", "to be fair", "honestly", "truthfully",
	"like", "well", "so", "right", "okay", "ok", "alright", "all right",
	"actually", "basically", "anyway", "look", "listen",
	"absolutely", "definitely", "totally", "got it", "gotcha",
	"yeah", "yep", "yes", "uh", "um", "hmm",
}

// FillerOpener returns the filler phrase a line opens with, or "" if it opens
// cleanly. It is the single source of truth for this check: the guard below strips
// what it finds, and cmd/evalclosing scores with it, so the measurement and the
// repair can never disagree about what counts as filler.
//
// A following COMMA is required. That is what separates the tic ("Well, thanks so
// much") from ordinary content ("Well-made candles are worth the wait"), and it is
// why stripping is safe.
//
// Fillers stack in speech — "Okay so," — so up to two consecutive markers are
// consumed before the comma is required.
func FillerOpener(line string) string {
	l := strings.ToLower(strings.TrimSpace(line))

	// matchOne returns the longest filler prefixing s (at a word boundary, so
	// "sure" cannot match inside "surely") and the remainder after it.
	matchOne := func(s string) (string, string) {
		best := ""
		for _, f := range fillerOpeners {
			if !strings.HasPrefix(s, f) || len(f) <= len(best) {
				continue
			}
			rest := s[len(f):]
			if rest == "" || rest[0] == ' ' || rest[0] == ',' {
				best = f
			}
		}
		if best == "" {
			return "", s
		}
		return best, strings.TrimLeft(s[len(best):], " ")
	}

	first, rest := matchOne(l)
	if first == "" {
		return ""
	}
	if strings.HasPrefix(rest, ",") {
		return first
	}
	if second, rest2 := matchOne(rest); second != "" && strings.HasPrefix(rest2, ",") {
		return first + " " + second
	}
	return ""
}

// StripFillerOpener removes a leading filler phrase so the farewell starts in the
// agent's own voice, and re-capitalizes what follows.
//
// FAILS OPEN: if stripping would leave nothing usable, the original line is
// returned unchanged. A guard that can empty the agent's last words is worse than
// the tic it was added to remove.
func StripFillerOpener(line string) string {
	f := FillerOpener(line)
	if f == "" {
		return line
	}
	trimmed := strings.TrimSpace(line)
	// Walk past the matched filler and the comma that qualified it. The filler may
	// contain a space ("okay so"), and the source may differ in case, so advance by
	// rune count over the matched prefix rather than by byte-slicing the lowercase.
	rest := trimmed[len(matchedPrefix(trimmed, f)):]
	rest = strings.TrimLeft(rest, " ")
	rest = strings.TrimPrefix(rest, ",")
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return line // nothing left to say — keep the original
	}
	return upperFirst(rest)
}

// matchedPrefix returns the leading substring of line corresponding to filler,
// preserving the original casing and internal spacing.
func matchedPrefix(line, filler string) string {
	// filler was matched case-insensitively against a space-normalized prefix, so
	// walk both in step, skipping extra spaces in line.
	li, fi := 0, 0
	for fi < len(filler) && li < len(line) {
		if filler[fi] == ' ' {
			for li < len(line) && line[li] == ' ' {
				li++
			}
			fi++
			continue
		}
		li++
		fi++
	}
	return line[:li]
}

// upperFirst capitalizes the first letter, so a stripped line still reads as a
// sentence ("glad to hear…" -> "Glad to hear…").
func upperFirst(s string) string {
	for i, r := range s {
		if r >= 'a' && r <= 'z' {
			return s[:i] + strings.ToUpper(string(r)) + s[i+len(string(r)):]
		}
		// Already uppercase or non-letter (a quote, say): leave it alone.
		return s
	}
	return s
}
