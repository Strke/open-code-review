// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package diff

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
	provider := NewPatchProvider(repo, patchDir, nil)
	diffs, err := provider.GetDiff(context.Background())
	if err != nil {
		t.Fatalf("GetDiff: %v", err)
	}
	if len(diffs) != 1 || diffs[0].NewPath != "a.go" || diffs[0].Insertions != 1 {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
}

func TestPatchProviderRejectsEmptyDirectory(t *testing.T) {
	provider := NewPatchProvider(t.TempDir(), t.TempDir(), nil)
	if _, err := provider.GetDiff(context.Background()); err == nil {
		t.Fatal("expected empty patch directory error")
	}
}
