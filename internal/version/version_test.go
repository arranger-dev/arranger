package version

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	defer func(v, c, b string) { Version, Commit, Built = v, c, b }(Version, Commit, Built)

	Version, Commit, Built = "v0.1.1", "a6a7af3d98370945f3952a6c6a29dd2d0db09ce5", "2026-09-24T10:00:00Z"
	i := Get()
	if i.Version != "v0.1.1" || i.Short() != "a6a7af3" || i.CommitURL != Repo+"/commit/"+Commit {
		t.Fatalf("release build: %+v", i)
	}
	if !i.Dirty && i.ReleaseURL != Repo+"/releases/tag/v0.1.1" {
		t.Fatalf("a clean tagged build links its release: %+v", i)
	}
	if s := i.String(); !strings.HasPrefix(s, "arranger v0.1.1 (a6a7af3") || !strings.Contains(s, "built 2026-09-24") {
		t.Fatalf("String: %q", s)
	}

	// between releases: a describe string has no release page; -dirty marks local changes
	Version = "v0.1.1-3-gabc1234-dirty"
	if i := Get(); i.ReleaseURL != "" || !i.Dirty {
		t.Fatalf("dev build: %+v", i)
	}

	Version, Commit, Built = "dev", "", ""
	if i := Get(); i.Version == "" || i.Go == "" {
		t.Fatalf("plain build: %+v", i)
	}
}
