package web

import (
	"strings"
	"testing"
)

// TestOptionsMenu checks that Clone and Merge live in the inspector's "..." menu, not the header.
func TestOptionsMenu(t *testing.T) {
	b, err := FS.ReadFile("arrange.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)

	start := strings.Index(html, `<details id="i-more"`)
	if start < 0 {
		t.Fatal(`no <details id="i-more"> in arrange.html`)
	}
	end := strings.Index(html[start:], "</details>")
	if end < 0 {
		t.Fatal(`<details id="i-more"> is not closed`)
	}
	end += start
	more := html[start:end]

	sum := strings.Index(more, "<summary")
	if sum < 0 {
		t.Fatal(`#i-more has no <summary>`)
	}
	if sumEnd := strings.Index(more[sum:], "</summary>"); sumEnd < 0 || !strings.Contains(more[sum:sum+sumEnd], "...") {
		t.Error(`#i-more <summary> does not contain "..."`)
	}

	for _, id := range []string{`id="i-clone"`, `id="i-merge"`} {
		if !strings.Contains(more, id) {
			t.Errorf("%s is not inside #i-more", id)
		}
		if n := strings.Count(html, id); n != 1 {
			t.Errorf("%s appears %d times, want 1", id, n)
		}
	}

	// Find the .ihead div and its matching </div>, counting nested divs.
	head := strings.Index(html, `<div class="ihead">`)
	if head < 0 {
		t.Fatal(`no <div class="ihead"> in arrange.html`)
	}
	depth, i := 0, head
	for {
		open := strings.Index(html[i:], "<div")
		shut := strings.Index(html[i:], "</div>")
		if shut < 0 {
			t.Fatal(".ihead div is not closed")
		}
		if open >= 0 && open < shut {
			depth++
			i += open + len("<div")
			continue
		}
		depth--
		i += shut + len("</div>")
		if depth == 0 {
			break
		}
	}
	if start < head || end > i {
		t.Error("#i-more is not inside the .ihead div")
	}
}
