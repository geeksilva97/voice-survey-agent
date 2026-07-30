// Command prompts moves this binary's prompts in and out of Langfuse Prompt
// Management.
//
//	go run ./cmd/prompts list          what Langfuse serves vs. what's compiled in
//	go run ./cmd/prompts push          make the code's version the labeled one
//	go run ./cmd/prompts show <name>   print one resolved prompt
//
// Push is idempotent by content: a prompt whose text already matches the label's
// current version does not mint a new one, so this is safe to run repeatedly.
//
// The prompt registry is populated by package init, so this command imports every
// package that declares a prompt — llm, ws and insight — for that side effect.
// Adding a prompt anywhere else means adding its package here too, or `list` will
// quietly under-report; the -expect flag below is the guard against that.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"voicesurvey/internal/insight"
	"voicesurvey/internal/llm"
	"voicesurvey/internal/obs"
	"voicesurvey/internal/prompt"
	"voicesurvey/internal/ws"
)

// Force the prompt declarations in each package to register. The blank
// assignments make the dependency explicit rather than leaving it to a reader to
// notice why these imports exist.
var _ = []string{
	llm.ClassifyPrompt.Name(),
	llm.SurveyPrompt.Name(),
	ws.ClosingPrompt.Name(),
	ws.GreetingReplyPrompt.Name(),
	insight.ScorePrompt.Name(),
}

func main() {
	label := flag.String("label", obs.DefaultPromptLabel, "Langfuse label to read/write (production, staging, ...)")
	expect := flag.Int("expect", 5, "fail if the registry doesn't hold exactly this many prompts — catches a new prompt whose package this command forgot to import")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: prompts [flags] <list|push|show NAME>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	defs := prompt.All()
	if *expect > 0 && len(defs) != *expect {
		fmt.Fprintf(os.Stderr, "registry holds %d prompts, expected %d — a package declaring a prompt is probably not imported by cmd/prompts (pass -expect=%d if the count legitimately changed)\n",
			len(defs), *expect, len(defs))
		os.Exit(1)
	}

	cmd := strings.TrimSpace(flag.Arg(0))
	if cmd == "" {
		flag.Usage()
		os.Exit(2)
	}
	if cmd == "show" {
		showLocal(flag.Arg(1))
		return
	}

	client, ok := obs.NewClient()
	if !ok {
		fmt.Fprintln(os.Stderr, "LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY are not set — nothing to talk to")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch cmd {
	case "push":
		results, err := obs.PushPrompts(ctx, client, *label)
		for _, r := range results {
			state := "unchanged"
			if r.Created {
				state = "NEW VERSION"
			}
			fmt.Printf("%-34s v%-3d %s\n", r.Name, r.Version, state)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "\npush failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\npushed %d prompt(s) to label %q -> %s\n", len(results), *label, obs.Host())
	case "list":
		if err := list(ctx, client, *label, defs); err != nil {
			fmt.Fprintf(os.Stderr, "list failed: %v\n", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// list is the drift report: for each declared prompt, whether Langfuse has a
// version at this label and whether its text matches the compiled-in default.
func list(ctx context.Context, c *obs.Client, label string, defs []prompt.Def) error {
	fmt.Printf("label %q @ %s\n\n", label, obs.Host())
	fmt.Printf("%-34s %-8s %-10s %s\n", "PROMPT", "REMOTE", "CODE FP", "STATE")
	for _, d := range defs {
		local := prompt.Resolved{Name: d.Name, Messages: d.Messages}
		p, found, err := c.GetPrompt(ctx, d.Name, label)
		if err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
		remote, state := "-", "not in langfuse"
		if found {
			remote = fmt.Sprintf("v%d", p.Version)
			state = "in sync"
			if (prompt.Resolved{Messages: p.Prompt}).Fingerprint() != local.Fingerprint() {
				state = "DIFFERS from code"
			}
		}
		fmt.Printf("%-34s %-8s %-10s %s\n", d.Name, remote, local.Fingerprint(), state)
	}
	return nil
}

// showLocal prints one compiled-in prompt, so its exact text can be read without
// a round-trip (and diffed against the UI by eye).
func showLocal(name string) {
	if name == "" {
		fmt.Fprintln(os.Stderr, "show needs a prompt name; run `prompts show` after `prompts list` for the names")
		os.Exit(2)
	}
	for _, d := range prompt.All() {
		if d.Name != name {
			continue
		}
		r := prompt.Resolved{Name: d.Name, Messages: d.Messages}
		fmt.Printf("# %s  (fingerprint %s, vars: %s)\n", d.Name, r.Fingerprint(), strings.Join(d.Vars, ", "))
		if d.Description != "" {
			fmt.Printf("# %s\n", d.Description)
		}
		for _, m := range d.Messages {
			fmt.Printf("\n--- %s ---\n%s\n", strings.ToUpper(m.Role), m.Content)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "no prompt named %q\n", name)
	os.Exit(1)
}
