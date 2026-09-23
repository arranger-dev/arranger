package orch

import (
	"fmt"
	"strings"

	"arranger/internal/agents"
	"arranger/internal/store"
)

func goalText(b *strings.Builder, g store.Goal, checks []string) {
	fmt.Fprintf(b, "\nGOAL: %s\n", g.Title)
	if g.Body != "" {
		b.WriteString(g.Body + "\n")
	}
	if g.Criteria != "" {
		b.WriteString("\nACCEPTANCE CRITERIA:\n" + g.Criteria + "\n")
	}
	b.WriteString("\nThese checks run in the worktree afterwards and must all pass:\n")
	for _, c := range checks {
		b.WriteString("- " + c + "\n")
	}
}

func workerPrompt(a store.Agent, g store.Goal, dir string, checks []string, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent. You work in the git worktree %s; only change files there.\n", a.Name, a.Role, dir)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	goalText(&b, g, checks)
	b.WriteString("\nStay within the goal. Don't claim something works unless you verified it. Don't commit; the orchestrator does.\n")
	if g.Notes != "" {
		b.WriteString("\nTHE USER REMOVED THESE CHANGES OF YOURS. Do not re-add them:\n" + g.Notes + "\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS ATTEMPT WAS NOT ACCEPTED. Fix this:\n" + feedback)
	}
	return b.String()
}

func planPrompt(a store.Agent, g store.Goal, kids []store.Agent, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager agent. You delegate and review; you do not edit files.\n", a.Name)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	goalText(&b, g, lines(g.Checks))
	b.WriteString("\nYOUR TEAM:\n")
	for _, k := range kids {
		fmt.Fprintf(&b, "- id %q: %s, role %s", k.ID, k.Name, k.Role)
		if k.Prompt != "" {
			b.WriteString(" (" + agents.Clip(k.Prompt, 200) + ")")
		}
		b.WriteString("\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS RESULT WAS NOT ACCEPTED. Plan to fix this:\n" + feedback + "\n")
	}
	b.WriteString(`
Read the repository if you need to, then split the goal into at most one subgoal per team member (skip members you don't need).
Each subgoal must be small, clearly scoped, and have at least one shell check that proves it's done; checks run from the repository root.
Members work in parallel on separate branches, so avoid giving two members the same files.
Reply with ONLY a JSON object, no prose:
{"subgoals":[{"agent":"<id>","title":"...","body":"...","criteria":"...","checks":["..."]}]}`)
	return b.String()
}
