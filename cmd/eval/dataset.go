package main

import "voicesurvey/internal/llm"

// evalCase is one labeled (question, reply) pair. `want` is the expected intent;
// `clarity` is the expected clarity axis and is only scored when non-empty (we
// score clarity on answer-type cases, since that's where the agent's "repair"
// turn triggers). Non-answer cases leave clarity empty (not scored).
type evalCase struct {
	q       string
	reply   string
	want    llm.Intent
	clarity llm.Clarity
}

const (
	clear   = llm.ClarityClear
	unclear = llm.ClarityUnclear
	na      = llm.Clarity("") // don't score clarity for this case
)

// dataset is a broad, hand-labeled corpus spanning both axes across several
// product types, with heavy phrasing variation (brief, vague, uncertain, quirky,
// negative, rambling, "no suggestion", calque/ESL, and noise). It exists to
// catch regressions like "valid answer flagged off_topic → re-ask" and
// "'nothing comes to mind' read as wants_stop → whole survey bails".
var dataset = []evalCase{
	// ---- answer: clear / on-point ----
	{"What do you think of our scented candles?", "I really love the lavender one, it's so relaxing in the evenings.", llm.IntentAnswer, clear},
	{"What's your favorite scent?", "Vanilla, definitely.", llm.IntentAnswer, clear},
	{"How often do you burn candles at home?", "Maybe three or four times a week, usually at night.", llm.IntentAnswer, clear},
	{"Would you recommend our candles to a friend?", "Yeah, for sure, I've already told a couple of people about them.", llm.IntentAnswer, clear},
	{"What could we improve about our candles?", "They could burn a little longer for the price, honestly.", llm.IntentAnswer, clear},
	{"How likely are you to recommend our coffee shop to friends?", "A hundred percent, I love this place.", llm.IntentAnswer, clear},
	{"How likely are you to recommend us, from one to ten?", "I'd say a solid eight.", llm.IntentAnswer, clear},
	{"What do you think of our new app's onboarding?", "It was pretty smooth, I got set up in a couple of minutes.", llm.IntentAnswer, clear},
	{"What feature would you most like us to add?", "Dark mode, please.", llm.IntentAnswer, clear},
	{"How was the fit of the jacket you ordered?", "It runs a little large but I like it that way.", llm.IntentAnswer, clear},
	{"What did you think of the service at our restaurant?", "The waiter was super friendly and fast.", llm.IntentAnswer, clear},

	// ---- answer: brief / one-word / uncertain / vague (still clear English) ----
	{"What's your favorite scent?", "Lavender.", llm.IntentAnswer, clear},
	{"Do you think the price is fair?", "Yeah.", llm.IntentAnswer, clear},
	{"What's one thing you'd improve at our coffee shop?", "I don't know, maybe better chairs I guess.", llm.IntentAnswer, clear},
	{"What's one thing you'd improve at our coffee shop?", "Hmm, not sure really... the music's a bit loud maybe.", llm.IntentAnswer, clear},
	{"How do you feel about the checkout process?", "It's fine I suppose.", llm.IntentAnswer, clear},
	{"What would make you visit more often?", "Honestly no idea, maybe more seating.", llm.IntentAnswer, clear},
	{"How likely are you to recommend us?", "Eh, probably.", llm.IntentAnswer, clear},

	// ---- answer: "no suggestion" — declining to add anything is a valid answer,
	// NOT a request to end the survey (the screenshot bug: bailed after Q1). ----
	{"What's one thing you'd like to see improved at our coffee shop?", "Nothing that comes to my mind actually.", llm.IntentAnswer, clear},
	{"What could we do better?", "Honestly nothing, it's all great as it is.", llm.IntentAnswer, clear},
	{"Is there anything you'd like us to add?", "No, I can't think of anything right now.", llm.IntentAnswer, clear},
	{"What would you change about our candles?", "Not really, everything's fine for me.", llm.IntentAnswer, clear},
	{"Anything we could improve?", "Nope, all good.", llm.IntentAnswer, clear},

	// ---- answer: quirky / unexpected but clear English ----
	{"What scent should we launch next?", "Something weird like fresh cut grass or tomato leaf.", llm.IntentAnswer, clear},
	{"What feature should we build next?", "A button that makes it play jazz while it loads, ha.", llm.IntentAnswer, clear},
	{"What's your favorite thing about our shop?", "The smell when you walk in, it's like a hug.", llm.IntentAnswer, clear},

	// ---- answer: negative / critical (clear) ----
	{"What do you think of our candles?", "Honestly not great, the scent fades way too fast.", llm.IntentAnswer, clear},
	{"How was your last visit?", "Pretty disappointing, the line was out the door and staff seemed stressed.", llm.IntentAnswer, clear},
	{"Would you buy from us again?", "Probably not, it was too expensive for what it was.", llm.IntentAnswer, clear},

	// ---- answer: rambling / filler-laden (clear, just wordy) ----
	{"What could we improve?", "So like, I mean, the thing is, you know, the app is fine but sometimes it just kind of freezes when I open it in the morning.", llm.IntentAnswer, clear},
	{"How do you use the product?", "Well I usually, um, put it on in the evening, like after dinner, when I'm winding down and reading a bit.", llm.IntentAnswer, clear},

	// ---- answer: broken / calque / ESL English → clarity=unclear ("defiant") ----
	// Non-native phrasings & literal translations that read as odd English but
	// clearly ARE answers. Intent must stay 'answer'; clarity should be 'unclear'
	// so the agent confirms once (natural repair) instead of silently recording.
	{"Is there a specific type of drink you'd like us to offer more often?", "A banana vitamin would be awesome.", llm.IntentAnswer, unclear},                        // vitamina de banana = smoothie
	{"Is there a specific type of drink you'd like us to offer more often?", "Put a banana vitamin and more natural juices, please.", llm.IntentAnswer, unclear},
	{"What do you think of our prices?", "The price is a little salty for me, honestly.", llm.IntentAnswer, unclear},                                                 // salgado = expensive
	{"How was the service at our restaurant?", "The waiter was very educated and gentle with us.", llm.IntentAnswer, unclear},                                        // educado = polite
	{"Will you visit us again?", "Yes, I pretend to come back next week for sure.", llm.IntentAnswer, unclear},                                                       // pretendo = intend
	{"How can we reach more customers?", "You should divulge more your promotions on the internet.", llm.IntentAnswer, unclear},                                      // divulgar = advertise
	{"What did you think of the coffee?", "For me the taste is more or less, nothing special.", llm.IntentAnswer, unclear},                                           // mais ou menos = so-so
	{"Would you recommend us?", "Sure, I go to recommend for all my friends.", llm.IntentAnswer, unclear},                                                            // vou recomendar
	{"How do you feel about our shop's atmosphere?", "The ambient inside is very cozy and calm, I like.", llm.IntentAnswer, unclear},                                 // ambiente = atmosphere
	{"Have you seen our ads?", "No, you need to make more publicity in the television, I never see your announces.", llm.IntentAnswer, unclear},                      // publicidade / anúncios
	{"How often do you use the product?", "Actually I don't consume much your candles, only sometimes.", llm.IntentAnswer, unclear},                                  // atualmente/consumir
	{"What's your impression of the vanilla candle?", "I liking it too much, is very perfumed and stay long time.", llm.IntentAnswer, unclear},                       // gosto muito / dura muito
	{"What would you change about the app?", "The application is travando a lot when I open, very slow.", llm.IntentAnswer, unclear},                                 // travar = to freeze

	// ---- answer: ambiguous reference → unclear ----
	{"What did you think of the app?", "Yeah, it was doing that thing again, you know.", llm.IntentAnswer, unclear},
	{"How was your experience?", "It was the usual, same as that other time.", llm.IntentAnswer, unclear},

	// ---- wants_stop: end the WHOLE survey ----
	{"What do you think of our candles?", "I have to go now, sorry, I don't have time for this.", llm.IntentWantsStop, na},
	{"What's your favorite scent?", "Stop, I'm done, no more questions please.", llm.IntentWantsStop, na},
	{"How often do you shop with us?", "Yeah I really need to run, can we wrap this up?", llm.IntentWantsStop, na},
	{"How likely are you to recommend us?", "I'm not interested in doing this survey, thanks.", llm.IntentWantsStop, na},
	{"What could we improve?", "Enough questions, I gotta go.", llm.IntentWantsStop, na},
	{"How was your visit?", "Let's just end it here, okay?", llm.IntentWantsStop, na},
	{"What do you think of the app?", "Nah I'm good, I don't want to continue.", llm.IntentWantsStop, na},
	{"How was your visit?", "Sorry, I have that to go now, my bus is coming.", llm.IntentWantsStop, na}, // calque of "tenho que ir"

	// ---- repeat: didn't HEAR it (audio), wants it read again as-is ----
	{"What do you think of our scented candles?", "Sorry, what was the question?", llm.IntentRepeat, na},
	{"How often do you burn candles?", "Can you say that again? I didn't catch it.", llm.IntentRepeat, na},
	{"What's your favorite scent?", "Huh? Come again?", llm.IntentRepeat, na},
	{"What feature would you add?", "Wait, could you repeat that please?", llm.IntentRepeat, na},
	{"How was the service?", "I didn't quite hear you, what did you ask?", llm.IntentRepeat, na},
	{"What would you improve?", "What? I missed that.", llm.IntentRepeat, na},
	{"What's your favorite scent?", "Repeat please, I no understand well the question.", llm.IntentRepeat, na}, // ESL grammar

	// ---- needs_help: heard it, but unsure how to answer / asks us to clarify ----
	// The respondent isn't quitting and hasn't answered — they need guidance. The
	// agent should reassure + hint how to answer, then re-pose (not just re-read).
	{"How would you rate the quality of our coffee?", "Do you expect some score or something?", llm.IntentNeedsHelp, na},
	{"What's one thing you'd improve about our candles?", "Hmm, I'm not really sure what you're looking for here.", llm.IntentNeedsHelp, na},
	{"How likely are you to recommend us?", "What do you mean exactly?", llm.IntentNeedsHelp, na},
	{"How would you rate the scent, from one to ten?", "Like a number, or should I just describe it?", llm.IntentNeedsHelp, na},
	{"What could we improve about the app?", "I'm not sure how to answer that, to be honest.", llm.IntentNeedsHelp, na},
	{"Would you recommend us?", "Sorry, I don't understand the question.", llm.IntentNeedsHelp, na},
	{"What did you think of the service?", "Hmm, what kind of thing are you looking for?", llm.IntentNeedsHelp, na},

	// ---- off_topic: genuinely unrelated ----
	{"What's your favorite scent?", "What time do you close today?", llm.IntentOffTopic, na},
	{"How do you like our candles?", "Do you know if it's going to rain later?", llm.IntentOffTopic, na},
	{"How likely are you to recommend us?", "Hey, can you turn the lights down in here?", llm.IntentOffTopic, na},
	{"What could we improve?", "Sorry, hold on — no, not you, I was talking to my dog.", llm.IntentOffTopic, na},
	{"What's your favorite feature?", "By the way, where's the nearest parking lot?", llm.IntentOffTopic, na},
	// Unrelated chit-chat as a STATEMENT (not a question) — a common tangent the
	// model must not mistake for an "answer". From the coffee-shop transcript
	// where the respondent kept talking about the World Cup.
	{"What's one thing you'd like to see improved at our coffee shop?", "Did you catch that World Cup game yesterday?", llm.IntentOffTopic, na},
	{"What's one thing you'd like to see improved at our coffee shop?", "Honestly I think Spain is going to win the whole tournament.", llm.IntentOffTopic, na},
	{"How often do you burn candles at home?", "Man, that referee last night was a disaster, did you see it?", llm.IntentOffTopic, na},

	// ---- unintelligible: noise / garbled STT output ----
	{"What do you think of our candles?", "(buzzing) (buzzing)", llm.IntentUnintellig, na},
	{"What's your favorite scent?", "uh... mm... [inaudible]", llm.IntentUnintellig, na},
	{"How often do you shop with us?", "sffft krrr ffff", llm.IntentUnintellig, na},
	// Parenthesized NON-SPEECH sounds from STT: a real word inside, but not an
	// answer. The agent must not say "Got it" and advance on a cough.
	{"How satisfied are you with the quality of our furniture?", "(coughing)", llm.IntentUnintellig, na},
	{"What could we improve about our candles?", "(clears throat)", llm.IntentUnintellig, na},
	{"How was the service?", "(laughs)", llm.IntentUnintellig, na},
}
