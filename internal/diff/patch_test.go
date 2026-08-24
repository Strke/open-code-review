// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package diff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/gitcmd"
)

func TestPatchProviderReadsPatchDirectoryInOrder(t *testing.T) {
	repo := t.TempDir()
	patchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/a.go b/a.go\nindex 123..456 100644\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+func A() {}\n"
	if err := os.WriteFile(filepath.Join(patchDir, "002.patch"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := NewPatchProvider(repo, patchDir, "", "", nil)
	diffs, err := provider.GetDiff(context.Background())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(diffs) != 1 || diffs[0].NewPath != "a.go" || diffs[0].Insertions != 1 {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
}

func TestPatchProviderRejectsEmptyDirectory(t *testing.T) {
	provider := NewPatchProvider(t.TempDir(), t.TempDir(), "", "", nil)
	if _, err := provider.GetDiff(context.Background()); err == nil {
		t.Fatal("expected empty patch directory error")
	}
}

func TestMaterializePatchSnapshotCreatesPostImage(t *testing.T) {
	repo := t.TempDir()
	patchDir := t.TempDir()
	runner := gitcmd.New(4)
	run := func(args ...string) string {
		t.Helper()
		out, err := runner.Run(context.Background(), repo, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.go")
	run("commit", "-m", "base")
	base := run("rev-parse", "HEAD")
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1,2 @@\n package a\n+func A() {}\n" +
		"diff --git a/new.go b/new.go\nnew file mode 100644\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1 @@\n+package added\n"
	if err := os.WriteFile(filepath.Join(patchDir, "change.patch"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}

	resolution, err := MaterializePatchSnapshot(context.Background(), repo, patchDir, runner)
	if err != nil {
		t.Fatalf("MaterializePatchSnapshot: %v", err)
	}
	if resolution.ResolvedBase != base || resolution.ResolvedHead == "" || resolution.ResolvedHead == base {
		t.Fatalf("unexpected resolution: %+v", resolution)
	}
	if got := run("show", resolution.ResolvedHead+":a.go"); got != "package a\nfunc A() {}" {
		t.Fatalf("post-image a.go = %q", got)
	}
	if got := run("show", resolution.ResolvedHead+":new.go"); got != "package added" {
		t.Fatalf("post-image new.go = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(repo, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package a\n" {
		t.Fatalf("source checkout was modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("new.go unexpectedly exists in source checkout: %v", err)
	}

	provider := NewPatchProvider(repo, patchDir, resolution.ResolvedBase, resolution.ResolvedHead, runner)
	diffs, err := provider.GetDiff(context.Background())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(diffs) != 2 || diffs[0].NewFileContent != "package a\nfunc A() {}\n" || diffs[1].NewFileContent != "package added\n" {
		t.Fatalf("diffs do not use post-image: %+v", diffs)
	}
}

func TestMaterializePatchSnapshotForcesConflictingHunk(t *testing.T) {
	repo := t.TempDir()
	patchDir := t.TempDir()
	runner := gitcmd.New(4)
	run := func(args ...string) string {
		t.Helper()
		out, err := runner.Run(context.Background(), repo, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("first\nlocal conflict\nlast\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "base")
	patch := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n first\n-expected old\n+patch wins\n last\n"
	if err := os.WriteFile(filepath.Join(patchDir, "conflict.patch"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}

	resolution, err := MaterializePatchSnapshot(context.Background(), repo, patchDir, runner)
	if err != nil {
		t.Fatalf("MaterializePatchSnapshot: %v", err)
	}
	if got := run("show", resolution.ResolvedHead+":a.txt"); got != "first\npatch wins\nlast" {
		t.Fatalf("forced post-image = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nlocal conflict\nlast\n" {
		t.Fatalf("source checkout was modified: %q", data)
	}
}

func TestNearestSequenceChoosesClosestMatch(t *testing.T) {
	lines := []string{"same", "x", "same", "y"}
	if got := nearestSequence(lines, []string{"same"}, 2); got != 2 {
		t.Fatalf("nearestSequence = %d, want 2", got)
	}
}
