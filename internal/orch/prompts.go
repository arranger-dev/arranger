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

func workerPrompt(a store.Agent, g store.Goal, dir string, checks []string, feedback, change string, fix bool) string {
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
	if change != "" && fix {
		b.WriteString("\nYOU ALREADY WORKED ON THIS GOAL. YOUR MANAGER MERGED THE TEAM'S WORK AND ITS CHECKS FAIL; IT NEEDS THIS FIXED. Keep the rest of your work; fix only this:\n" + change + "\n")
	} else if change != "" {
		b.WriteString("\nYOU ALREADY WORKED ON THIS GOAL, AND THE USER ASKS FOR THESE CHANGES. Keep the rest of your work; change only this:\n" + change + "\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS ATTEMPT WAS NOT ACCEPTED. Fix this:\n" + feedback)
	}
	return b.String()
}

func planPrompt(a store.Agent, g store.Goal, kids []store.Agent, feedback, change string) string {
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
	if change != "" {
		b.WriteString("\nTHE USER ALSO ASKS FOR THESE CHANGES. Plan for them:\n" + change + "\n")
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

// revisePrompt asks a manager whose team already worked which reports must change what, so only
// they re-run. Each keeps its goal and work; it gets just its part of the user's request.
func revisePrompt(a store.Agent, g store.Goal, kids []store.Agent, goals map[string]store.Goal, change, feedback string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager agent. You delegate and review; you do not edit files.\n", a.Name)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	b.WriteString("\nYour team already worked toward your goal, and the result was merged. The user reviewed it and asks for these changes:\n" + change + "\n")
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS RESULT WAS NOT ACCEPTED. Account for this too:\n" + feedback + "\n")
	}
	goalText(&b, g, lines(g.Checks))
	b.WriteString("\nYOUR TEAM AND WHAT EACH ONE ALREADY DID:\n")
	for _, k := range kids {
		kg := goals[k.ID]
		fmt.Fprintf(&b, "- id %q: %s, role %s", k.ID, k.Name, k.Role)
		if kg.Title == "" {
			b.WriteString(": no subgoal yet, can't take a change\n")
			continue
		}
		fmt.Fprintf(&b, ": subgoal %q (%s, +%d -%d lines; checks: %s)\n", kg.Title, kg.Status, kg.Adds, kg.Dels, strings.Join(lines(kg.Checks), "; "))
	}
	b.WriteString(`
Read the repository if you need to. Decide which members must change something to satisfy the request, and give each one
only the part of the request that concerns it, as a concrete instruction. They keep their current work and goal.
Leave out members that need no change. When a shell check can prove a change, add it (checks run from the repository root).
Reply with ONLY a JSON object, no prose:
{"changes":[{"agent":"<id>","change":"...","checks":["optional extra check"]}]}`)
	return b.String()
}

// fixPrompt asks a manager whose team's merged work fails its checks which reports must fix what.
// Like revisePrompt, the reports keep their goal and work and get only their part of the fix.
func fixPrompt(a store.Agent, g store.Goal, kids []store.Agent, goals map[string]store.Goal, failure string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a manager agent. You delegate and review; you do not edit files.\n", a.Name)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	b.WriteString("\nYour team's work was merged into your branch, but your own checks fail on the merged result:\n" + failure + "\n")
	goalText(&b, g, lines(g.Checks))
	b.WriteString("\nYOUR TEAM AND THE SUBGOAL EACH ONE WORKED ON:\n")
	for _, k := range kids {
		kg := goals[k.ID]
		fmt.Fprintf(&b, "- id %q: %s, role %s", k.ID, k.Name, k.Role)
		if kg.Title == "" {
			b.WriteString(": no subgoal, can't take a fix\n")
			continue
		}
		fmt.Fprintf(&b, ": subgoal %q (checks: %s)\n", kg.Title, strings.Join(lines(kg.Checks), "; "))
	}
	b.WriteString(`
Read the repository and the failure output. The parts may each work alone but not together, e.g. one member calls a
function another named differently. Decide which members must fix what, and give each one a concrete instruction.
Leave out members that need no change. When a shell check can prove the fix, add it (checks run from the repository root).
Reply with ONLY a JSON object, no prose:
{"changes":[{"agent":"<id>","change":"...","checks":["optional extra check"]}]}`)
	return b.String()
}
