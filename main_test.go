package main

import "testing"

func TestValidate(t *testing.T) {
	if err := validate(demoAgents); err != nil {
		t.Fatal("demo data:", err)
	}
	bad := map[string][]Agent{
		"cycle":    {{ID: "a", Name: "A", Parent: "b"}, {ID: "b", Name: "B", Parent: "a"}},
		"self":     {{ID: "a", Name: "A", Parent: "a"}},
		"unknown":  {{ID: "a", Name: "A", Parent: "zz"}},
		"dup":      {{ID: "a", Name: "A"}, {ID: "a", Name: "A"}},
		"empty":    {{ID: "", Name: "A"}},
		"noname":   {{ID: "a", Name: " "}},
		"traverse": {{ID: "../x", Name: "A"}},
	}
	for name, as := range bad {
		if validate(as) == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildTree(t *testing.T) {
	roots := buildTree(demoAgents)
	if len(roots) != 1 || roots[0].ID != "lead" || len(roots[0].Children) != 3 {
		t.Fatalf("unexpected tree: %+v", roots)
	}
}
