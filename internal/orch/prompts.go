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

// team is what the worker's manager is after and what its teammates do meanwhile; "" for a
// top-level worker. It keeps parallel workers in their own lane.
func workerPrompt(a store.Agent, g store.Goal, dir string, checks []string, feedback, change string, fix bool, team string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s, a %s agent. You work in the git worktree %s; only change files there.\n", a.Name, a.Role, dir)
	if a.Prompt != "" {
		b.WriteString("\n" + a.Prompt + "\n")
	}
	goalText(&b, g, checks)
	if team != "" {
		b.WriteString("\n" + team)
	}
	b.WriteString(`
How to work:
- Read the code your goal touches before you change it, and follow the conventions you find there.
- Stay within the goal: change only what it needs, and nothing a teammate is doing.
- Before you finish, run every check above yourself and fix what fails. You may run them without asking.
- Don't claim something works unless you verified it. If a check can't pass for a reason outside your goal, say so plainly.
- Don't commit; the orchestrator does.
`)
	b.WriteString("End your reply with one line describing what you changed, used as the commit message, in this form:\n" +
		"Commit: <what changed, in the imperative, under 72 characters, e.g. Add retry with backoff to webhook sender>\n")
	if g.Notes != "" {
		b.WriteString("\nTHE USER REMOVED THESE CHANGES OF YOURS. Do not re-add them:\n" + g.Notes + "\n")
	}
	if change != "" && fix {
		b.WriteString("\nYOU ALREADY WORKED ON THIS GOAL. YOUR MANAGER MERGED THE TEAM'S WORK AND ITS CHECKS FAIL; IT NEEDS THIS FIXED. Keep the rest of your work; fix only this:\n" + change + "\n")
	} else if change != "" {
		b.WriteString("\nYOU ALREADY WORKED ON THIS GOAL, AND THE USER ASKS FOR THESE CHANGES. Keep the rest of your work; change only this:\n" + change + "\n")
	}
	if feedback != "" {
		b.WriteString("\nYOUR PREVIOUS ATTEMPT WAS NOT ACCEPTED. Your changes from it are still in the worktree: build on them, don't start over. " +
			"Find the cause of the failure below before you change anything, then fix it and run the checks again:\n" + feedback)
	}
	return b.String()
}

// repoText is what a manager already knows about the repository from its last plan, and what
// changed since, so it doesn't read the whole repository again for every goal. "" when it has no notes.
func repoText(notes, sha, changed string) string {
	if strings.TrimSpace(notes) == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nWHAT YOU ALREADY KNOW ABOUT THE REPOSITORY (your notes from your last plan, at commit %s):\n%s\n", shortSHA(sha), strings.TrimSpace(notes))
	if changed == "" {
		b.WriteString("Nothing changed since then.\n")
	} else {
		b.WriteString("Files changed since then:\n" + changed + "\n")
	}
	b.WriteString("Trust these notes. Read files only for what they don't cover, or what changed.\n")
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// checkAdvice is how a manager should write a subgoal's checks: ones that prove this subgoal's work,
// not ones that already pass on the code as it is.
const checkAdvice = `A good check fails on the code as it is now and passes only once the subgoal is done: a test named for the
new behaviour run so it can't pass by matching nothing (e.g. go test -run '^TestInviteExpiry$' -v ./invites | grep -q -- '--- PASS'),
a grep for the new route, flag or text, or a command that exercises the feature. A whole existing test suite only proves nothing broke;
add it next to a specific check, not instead of one.`

func planPrompt(a store.Agent, g store.Goal, kids []store.Agent, feedback, change, repo string) string {
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
	b.WriteString(repo)
	b.WriteString(`
Read the repository if you need to, then split the goal into at most one subgoal per team member. Give work only to the members
the goal needs; leave the others out, they don't run.
Each subgoal must be small, clearly scoped, and have at least one shell check that proves it's done; checks run from the repository root.
In the body, name the files or packages the member should work in and anything it must agree on with a teammate (a function name,
an API shape), since members can't see each other's work.
` + checkAdvice + `
Members work in parallel on separate branches, so avoid giving two members the same files.
In "repo", write short notes on the repository for your next plan, so it doesn't have to read the repository again: layout,
conventions, how to build and test, and the files that matter (under 300 words). Update your earlier notes if you have them.
Reply with ONLY a JSON object, no prose:
{"subgoals":[{"agent":"<id>","title":"...","body":"...","criteria":"...","checks":["..."]}],"repo":"..."}`)
	return b.String()
}

// revisePrompt asks a manager whose team already worked which reports must change what, so only
// they re-run. Each keeps its goal and work; it gets just its part of the user's request.
func revisePrompt(a store.Agent, g store.Goal, kids []store.Agent, goals map[string]store.Goal, change, feedback, repo string) string {
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
	b.WriteString(repo)
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
func fixPrompt(a store.Agent, g store.Goal, kids []store.Agent, goals map[string]store.Goal, failure, repo string) string {
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
	b.WriteString(repo)
	b.WriteString(`
Read the repository and the failure output. The parts may each work alone but not together, e.g. one member calls a
function another named differently. Decide which members must fix what, and give each one a concrete instruction.
Leave out members that need no change. When a shell check can prove the fix, add it (checks run from the repository root).
Reply with ONLY a JSON object, no prose:
{"changes":[{"agent":"<id>","change":"...","checks":["optional extra check"]}]}`)
	return b.String()
}
