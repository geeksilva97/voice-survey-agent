// Test-set leak guard.
//
// The classifier prompt's few-shot anchors and this eval's corpus must stay
// disjoint. An anchor that repeats a corpus sentence hands the model the answer to
// a question it is about to be graded on: accuracy goes up, the classifier does not
// get better, and the number silently stops meaning anything.
//
// EVAL.md has carried this rule since we hit it once, but it used to be enforced by
// a human reading a diff. Now that a prompt can be authored in the Langfuse UI, an
// anchor can reach production without ever passing through code review — so the
// rule needs to be executable. This file is that rule.
package main

import (
	"fmt"
	"regexp"
	"strings"

	"voicesurvey/internal/llm"
)

// leak is one overlap between a few-shot anchor and a corpus case.
type leak struct {
	corpus string // the dataset reply
	anchor string // the prompt anchor's reply
	exact  bool   // identical after normalization, vs. one containing the other
}

// minContained is the shortest normalized string we'll accept as evidence of a
// containment leak. Short replies ("Yeah.", "Vanilla.") legitimately recur across
// a corpus about opinions; flagging those would be noise that trains people to
// ignore the check. Exact matches are reported at any length.
const minContained = 20

// apostrophes are deleted rather than replaced with a space, so "I'd" normalizes to
// "id" instead of splitting into two tokens. Everything else that isn't a letter,
// digit or space becomes a space, so punctuation and casing can't disguise a reused
// sentence.
var (
	apostrophes = regexp.MustCompile(`['’]`)
	nonAlnum    = regexp.MustCompile(`[^\p{L}\p{N} ]+`)
)

// normalize renders a reply comparable: lowercased, punctuation removed, runs of
// whitespace collapsed.
func normalize(s string) string {
	s = apostrophes.ReplaceAllString(strings.ToLower(s), "")
	s = nonAlnum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// fewShotLeaks compares every corpus reply against every anchor reply in the ACTIVE
// classifier prompt. Runs against whichever prompt is installed, so it covers a
// Langfuse-authored anchor as well as a compiled-in one.
func fewShotLeaks(cases []evalCase) []leak {
	anchors := llm.ClassifyShotExamples()
	var out []leak
	for _, c := range cases {
		cn := normalize(c.reply)
		if cn == "" {
			continue
		}
		for _, a := range anchors {
			an := normalize(a[1])
			if an == "" {
				continue
			}
			switch {
			case cn == an:
				out = append(out, leak{corpus: c.reply, anchor: a[1], exact: true})
			case len(an) >= minContained && strings.Contains(cn, an),
				len(cn) >= minContained && strings.Contains(an, cn):
				out = append(out, leak{corpus: c.reply, anchor: a[1]})
			}
		}
	}
	return out
}

// reportLeaks renders the failure. It names both sides and says which to change,
// because the fix is never obvious from the message alone: the corpus case is
// usually the one worth keeping (it may be a regression anchor for a real bug),
// so the prompt's example is what should be rewritten.
func reportLeaks(ls []leak) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TEST-SET LEAK: %d classifier few-shot anchor(s) reuse a corpus sentence.\n", len(ls))
	for _, l := range ls {
		kind := "contains"
		if l.exact {
			kind = "identical"
		}
		fmt.Fprintf(&b, "  [%s]\n    corpus: %q\n    anchor: %q\n", kind, l.corpus, l.anchor)
	}
	b.WriteString("\nThe anchor hands the model the answer to a case it is graded on, so accuracy\n")
	b.WriteString("rises without the classifier improving. Rewrite the PROMPT's example as a novel\n")
	b.WriteString("sentence — corpus cases are often regression anchors for real bugs and should stay.\n")
	return b.String()
}
