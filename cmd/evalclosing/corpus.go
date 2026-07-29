package main

// The corpus is hand-written, like cmd/eval's — engineered to stress the specific
// failure rather than sampled from traffic. Every case is the same product
// (hand-poured scented soy candles, matching the project's QA rule); what varies
// is the RESPONDENT's voice, because the defect is about the agent absorbing the
// respondent's phrasing.
//
// Cases come in two kinds:
//   - tic cases: the respondent opens every answer with a filler ("Sure thing,",
//     "Honestly,"). These are where the agent has been caught parroting it back.
//   - control cases: no filler, or filler mid-sentence. If these ever start
//     failing the clean-opener check, the guard is over-reaching.
//
// The `tic` field is documentation, not an input: the scorer looks only at the
// closing line, so a case cannot pass by the scorer knowing what to expect.

const qaProduct = "hand-poured scented soy candles for the home"

type closingCase struct {
	name string
	tic  string // the respondent's filler, or "" for a control case
	qa   []qaPair
}

type qaPair struct{ q, a string }

var corpus = []closingCase{
	// ---- tic cases: respondent opens with a discourse marker ----
	{"sure-thing", "Sure thing", []qaPair{
		{"What scent do you love the most?", "Sure thing, I love a blend of coastal vibes with some tropical touches."},
		{"Would more exotic scents make you buy again?", "Sure thing, more exotic scents would pique my interest."},
	}},
	{"honestly", "Honestly", []qaPair{
		{"What scent do you love the most?", "Honestly, lavender is the one I keep rebuying for the bedroom."},
		{"What would you improve about our candles?", "Honestly, they could burn a couple hours longer for the price."},
	}},
	{"i-mean", "I mean", []qaPair{
		{"How do the candles fit into your home?", "I mean, the jars look great on my shelf, very cosy."},
		{"Would you recommend them to a friend?", "I mean, yeah, I've already told two friends about them."},
	}},
	{"to-be-fair", "To be fair", []qaPair{
		{"What do you think of the scent throw?", "To be fair, it fills the room faster than I expected."},
		{"Anything you'd change?", "To be fair, the price is a bit steep for the size."},
	}},
	{"you-know", "You know", []qaPair{
		{"How would you describe the vibe they create?", "You know, it's that spa feeling, really calming after work."},
		{"Would you buy again?", "You know, definitely, it's part of my evening routine now."},
	}},
	{"well", "Well", []qaPair{
		{"When do you usually burn them?", "Well, mostly Sunday evenings when the house is quiet."},
		{"What scent suits that best?", "Well, something soft, maybe vanilla or sandalwood."},
	}},
	{"like-tic", "Like", []qaPair{
		{"When do you burn them?", "Like, mostly in the evening when I'm winding down with a book."},
		{"What scent works best then?", "Like, something warm, maybe vanilla."},
	}},
	{"okay-so", "Okay so", []qaPair{
		{"What made you try our candles?", "Okay so, a friend gave me one as a housewarming gift."},
		{"Did it change how you shop for candles?", "Okay so, now I check whether they're soy first."},
	}},

	// ---- control cases: clean respondent voice, nothing to echo ----
	{"clean-specific", "", []qaPair{
		{"What scent do you love the most?", "Lavender, without question — it's the one I rebuy."},
		{"What would you improve?", "A slightly wider jar so the wax burns evenly."},
	}},
	{"clean-terse", "", []qaPair{
		{"What's your favourite scent?", "Vanilla."},
		{"Would you buy again?", "Yes, probably every couple of months."},
	}},
	{"clean-rambling", "", []qaPair{
		{"How do the candles fit into your home?", "They sit on the windowsill in the kitchen and my partner lights one while cooking, so the whole downstairs ends up smelling of it, which we both like a lot."},
		{"Anything you'd change?", "Maybe a lid, because dust gets in between uses."},
	}},
	{"clean-critical", "", []qaPair{
		{"What do you think of the scent throw?", "Weaker than I hoped — I can only smell it in one room."},
		{"Would you recommend them?", "Probably not at this price, no."},
	}},
	// ---- regression anchors from live conversations ----
	//
	// Added after a browser QA run (poll 94243ba6d5) where the closing line copied
	// SIXTEEN consecutive words from this answer — "Price might be a bit steep, but
	// if they had a loyalty program or discounts, I'd definitely buy again." The
	// offline corpus missed the whole class because no case had a long, fluent,
	// quotable final answer: the tic cases are short and the controls are terse.
	// A long final answer is the shape that tempts wholesale repetition.
	{"quotable-long-answer", "", []qaPair{
		{"Which scent do you love the most?", "I really love the lavender, it's soothing and calming."},
		{"What would make you buy our candles again?", "Price might be a bit steep but if they had a loyalty program or discounts I'd buy again."},
	}},
	{"quotable-two-clause", "", []qaPair{
		{"How do you use the candles at home?", "I light one in the kitchen while I cook and another in the bath afterwards, so the whole flat ends up smelling of it."},
		{"Would you change anything?", "A bigger wick would help, because the middle tunnels down and the edges never melt properly."},
	}},

	// Filler present but MID-sentence: the closing has a tempting phrase to copy,
	// yet nothing filler-shaped to open with. Guards against a scorer that keys on
	// the filler appearing anywhere rather than in the opening position.
	{"filler-midsentence", "", []qaPair{
		{"What scent do you love the most?", "The fig one, honestly, it surprised me how much I liked it."},
		{"Would you buy again?", "I'd say yes, you know, it's become a habit."},
	}},
}

// answers returns just the respondent's replies, for the copied-span measure.
func (c closingCase) answers() []string {
	out := make([]string, 0, len(c.qa))
	for _, p := range c.qa {
		out = append(out, p.a)
	}
	return out
}
