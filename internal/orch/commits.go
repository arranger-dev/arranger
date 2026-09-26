package orch

import (
	"fmt"
	"regexp"
	"strings"

	"arranger/internal/agents"
	"arranger/internal/git"
)

// A commit's message is one line saying what changed: the agent's own "Commit:" line when it gave
// one, else what it was asked to do. A manager's merge reuses the line of the work it merges.

var commitLine = regexp.MustCompile(`(?im)^\s*\**commit\**:\**\s*(.+?)\s*$`)

// oneLine is a commit message cut to one readable line.
func oneLine(s string) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	s = strings.Trim(s, "`\"' ")
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return ""
	}
	return agents.Clip(strings.ToUpper(s[:1])+s[1:], 72)
}

// checkpointMessage is the one line for a worker's attempt: its own description of the change,
// else the change or goal it worked on.
func checkpointMessage(summary, change, goal string) string {
	if m := commitLine.FindAllStringSubmatch(summary, -1); len(m) > 0 {
		if s := oneLine(m[len(m)-1][1]); s != "" {
			return s
		}
	}
	if s := oneLine(change); s != "" {
		return s
	}
	return oneLine(goal)
}

// MergeMessage is the message for merging several changes at once: the one change's line, or a
// subject naming how many with every change on its own line below it. fallback is used when no
// change has a line of its own.
func MergeMessage(changes []string, fallback string) string {
	switch len(changes) {
	case 0:
		return oneLine(fallback)
	case 1:
		return changes[0]
	}
	subj := agents.Clip(fmt.Sprintf("%d changes: %s", len(changes), strings.Join(changes, "; ")), 72)
	return subj + "\n\n- " + strings.Join(changes, "\n- ") + "\n"
}

// mergeMessage is the message for a manager merging a report's branch: all the report's new
// changes, else its subgoal.
func mergeMessage(dir, branch, subgoal string) string {
	return MergeMessage(git.ChangeLines(dir, "HEAD", branch, nil), subgoal)
}
