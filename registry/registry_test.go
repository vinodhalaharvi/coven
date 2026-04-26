package registry

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct{}

func (fakeRunner) Run(ctx context.Context) error { return nil }

func TestRegister_AddsAndRetrieves(t *testing.T) {
	Reset()
	defer Reset()

	Register(AgentSpec{
		Name:        "alpha",
		Description: "test",
		Detect:      func(string, string) bool { return true },
		Build:       func(BuildDeps) Runner { return fakeRunner{} },
	})

	if got := len(All()); got != 1 {
		t.Errorf("len(All()) = %d, want 1", got)
	}
	if s := ByName("alpha"); s == nil {
		t.Error("ByName(alpha) = nil")
	}
	if s := ByName("nonexistent"); s != nil {
		t.Error("ByName(nonexistent) should be nil")
	}
}

func TestRegister_Idempotent(t *testing.T) {
	Reset()
	defer Reset()

	Register(AgentSpec{Name: "x", Description: "v1"})
	Register(AgentSpec{Name: "x", Description: "v2"})

	if got := len(All()); got != 1 {
		t.Errorf("duplicate Name should replace, got %d entries", got)
	}
	if s := ByName("x"); s == nil || s.Description != "v2" {
		t.Errorf("expected v2, got %+v", s)
	}
}

func TestDetect_FiltersBySpecPredicate(t *testing.T) {
	Reset()
	defer Reset()

	Register(AgentSpec{
		Name:   "always",
		Detect: func(string, string) bool { return true },
	})
	Register(AgentSpec{
		Name: "if-gin",
		Detect: func(goMod, _ string) bool {
			return strings.Contains(goMod, "gin-gonic/gin")
		},
	})
	Register(AgentSpec{
		Name:   "never",
		Detect: func(string, string) bool { return false },
	})

	matched := Detect("module x\nrequire (\n  github.com/gin-gonic/gin v1.0\n)", "/tmp")
	if len(matched) != 2 {
		t.Errorf("expected 2 matches, got %d: %+v", len(matched), namesOf(matched))
	}
	expectNames(t, matched, []string{"always", "if-gin"})

	matched = Detect("module x\n", "/tmp")
	if len(matched) != 1 {
		t.Errorf("expected 1 match (always-on only), got %d: %+v", len(matched), namesOf(matched))
	}
	expectNames(t, matched, []string{"always"})
}

func TestHasDep(t *testing.T) {
	cases := map[string]struct {
		goMod string
		dep   string
		want  bool
	}{
		"present": {
			goMod: "require (\n  github.com/gin-gonic/gin v1.0\n)",
			dep:   "github.com/gin-gonic/gin",
			want:  true,
		},
		"absent": {
			goMod: "require (\n  github.com/echo/echo v1.0\n)",
			dep:   "github.com/gin-gonic/gin",
			want:  false,
		},
		"empty go.mod": {
			goMod: "",
			dep:   "anything",
			want:  false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := HasDep(tc.goMod, tc.dep); got != tc.want {
				t.Errorf("HasDep = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuild_PassesDeps(t *testing.T) {
	Reset()
	defer Reset()

	var seen BuildDeps
	Register(AgentSpec{
		Name:   "captures",
		Detect: func(string, string) bool { return true },
		Build: func(d BuildDeps) Runner {
			seen = d
			return fakeRunner{}
		},
	})

	matched := Detect("anything", "/some/root")
	if len(matched) != 1 {
		t.Fatal("expected match")
	}

	deps := BuildDeps{ModuleRoot: "/some/root"}
	matched[0].Build(deps)

	if seen.ModuleRoot != "/some/root" {
		t.Errorf("Build did not receive deps: got %+v", seen)
	}
}

func namesOf(specs []AgentSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func expectNames(t *testing.T, got []AgentSpec, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("len mismatch: got %v, want %v", namesOf(got), want)
		return
	}
	for i, s := range got {
		if s.Name != want[i] {
			t.Errorf("position %d: got %s, want %s", i, s.Name, want[i])
		}
	}
}
