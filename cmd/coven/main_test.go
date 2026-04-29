package main

import (
	"testing"

	"github.com/vinodhalaharvi/coven/registry"
)

// TestAgentsAreRegistered verifies that the cmd/coven binary has all
// expected agents registered by virtue of its blank imports.
//
// This test exists because of a real bug we hit: the original v2 binary
// had no blank imports for agent packages, so their init() functions
// never ran, and the registry was empty at startup. The router then
// returned "no agents registered" for every diff.
//
// Symptom: 'coven -root .' runs, fsnotify detects file changes, the
// router gets called, and prints:
//
//	[coven v2] router: no agents needed (no agents registered)
//
// Fix: add blank imports for each agent package in cmd/coven/main.go.
//
// This test runs as part of the cmd/coven package's tests, so it sees
// exactly what the binary sees at startup. If someone removes a blank
// import (or adds a new agent without registering its package), this
// test fails.
func TestAgentsAreRegistered(t *testing.T) {
	// Every agent we expect cmd/coven to know about. Keep this in sync
	// with the blank imports in main.go. Adding an agent requires:
	//   1. Adding the blank import in main.go
	//   2. Adding the agent's name to this list
	expected := []string{
		"build",
		"connect",
		"docker",
		"gin",
		"gogenerate",
		"main",
		"make",
		"proto",
		"sqlc",
		"test",
		"wire",
	}

	all := registry.All()
	got := make(map[string]bool, len(all))
	for _, spec := range all {
		got[spec.Name] = true
	}

	for _, name := range expected {
		if !got[name] {
			t.Errorf("agent %q is not registered — check that cmd/coven imports its package", name)
		}
	}

	// Also flag if there are unexpected agents — protects against
	// silent additions that aren't documented in this test.
	for name := range got {
		known := false
		for _, expectedName := range expected {
			if expectedName == name {
				known = true
				break
			}
		}
		if !known {
			t.Logf("note: agent %q is registered but not in the expected list (test will pass)", name)
		}
	}
}

// TestEveryRegisteredAgentHasRoleString catches another real bug
// class: an agent registers with the registry but forgets to set
// AgentSpec.Role. The v2 task adapter requires Role to dispatch to
// the agent — without it, the router can pick the agent but the
// task runner will fail with "agent X has no Role string registered".
//
// We catch this at build/test time rather than at runtime.
func TestEveryRegisteredAgentHasRoleString(t *testing.T) {
	for _, spec := range registry.All() {
		if spec.Role == "" {
			t.Errorf("agent %q has empty Role string — v2 task adapter will fail to dispatch to it", spec.Name)
		}
	}
}

// TestEveryRegisteredAgentHasRouterContext catches a more subtle
// case: an agent is registered but lacks the v2 router context
// fields (TypicalTriggers, DomainFiles, etc.). Without these, the
// router has only Name + Description to work with, and routing
// quality suffers.
//
// We DON'T fail the test if these are missing — they're optional
// from a correctness standpoint. We just log so the developer knows.
func TestEveryRegisteredAgentHasRouterContext(t *testing.T) {
	for _, spec := range registry.All() {
		if spec.TypicalTriggers == "" {
			t.Logf("agent %q: TypicalTriggers is empty (router will work but routing quality may suffer)", spec.Name)
		}
		if spec.DomainFiles == "" {
			t.Logf("agent %q: DomainFiles is empty", spec.Name)
		}
	}
}
