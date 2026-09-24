// Package version says which build of arranger is running: its release, commit and build time.
// Releases set Version, Commit and Built with -ldflags "-X arranger/internal/version.Version=v1.2.3 ..."
// (see the Makefile); a plain `go build` in a git checkout still reports its commit from the VCS
// information Go records in the binary.
package version

import (
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Repo is arranger's source repository.
const Repo = "https://github.com/arranger-dev/arranger"

// Set at build time; see the package comment.
var (
	Version = "dev"
	Commit  = "" // full commit hash
	Built   = "" // RFC 3339 build time
)

type Info struct {
	Version    string `json:"version"`    // v1.2.3, a git describe string, or "dev"
	Commit     string `json:"commit"`     // full hash, "" when unknown
	Dirty      bool   `json:"dirty"`      // built with uncommitted changes
	Built      string `json:"built"`      // when the binary was built, RFC 3339; "" when unknown
	Committed  string `json:"committed"`  // when the commit was made, RFC 3339; "" when unknown
	Go         string `json:"go"`         // Go toolchain version
	ReleaseURL string `json:"releaseUrl"` // release notes, only for tagged releases
	CommitURL  string `json:"commitUrl"`
}

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// Get returns the running build's version info.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Built: Built, Go: runtime.Version()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if i.Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			i.Version = bi.Main.Version // go install ...@v1.2.3
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				i.Committed = s.Value
			case "vcs.modified":
				i.Dirty = s.Value == "true"
			}
		}
	}
	if strings.HasSuffix(i.Version, "-dirty") {
		i.Dirty = true
	}
	if releaseTag.MatchString(i.Version) && !i.Dirty {
		i.ReleaseURL = Repo + "/releases/tag/" + i.Version
	}
	if i.Commit != "" {
		i.CommitURL = Repo + "/commit/" + i.Commit
	}
	return i
}

// Short is the commit's first 7 characters, or "".
func (i Info) Short() string {
	if len(i.Commit) > 7 {
		return i.Commit[:7]
	}
	return i.Commit
}

// String is a one-line description, e.g. "arranger v0.1.1 (a6a7af3, built 2026-09-24, go1.26.4)".
func (i Info) String() string {
	var parts []string
	if c := i.Short(); c != "" {
		if i.Dirty {
			c += "+dirty"
		}
		parts = append(parts, c)
	}
	if w := i.When(); w != "" {
		parts = append(parts, w)
	}
	parts = append(parts, i.Go)
	return fmt.Sprintf("arranger %s (%s)", i.Version, strings.Join(parts, ", "))
}

// When is "built 2026-09-24", or "committed 2026-09-24" when the build time is unknown, or "".
func (i Info) When() string {
	if d := day(i.Built); d != "" {
		return "built " + d
	}
	if d := day(i.Committed); d != "" {
		return "committed " + d
	}
	return ""
}

// day turns an RFC 3339 time into its date, or "".
func day(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}
