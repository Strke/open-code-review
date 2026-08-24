// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package diff

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alibaba/open-code-review/internal/gitcmd"
	"github.com/alibaba/open-code-review/internal/model"
)

// PatchProvider reads unified diff files from an external directory.
type PatchProvider struct {
	repoDir string
	diffDir string
	baseRef string
	headRef string
	runner  *gitcmd.Runner
}

func NewPatchProvider(repoDir, diffDir, baseRef, headRef string, runner *gitcmd.Runner) *PatchProvider {
	return &PatchProvider{repoDir: repoDir, diffDir: diffDir, baseRef: baseRef, headRef: headRef, runner: runner}
}

func (p *PatchProvider) ResolveInput(context.Context) InputResolution {
	return InputResolution{
		ResolvedBase: p.baseRef,
		ResolvedHead: p.headRef,
		ExactRange:   p.baseRef + ".." + p.headRef,
	}
}

func (p *PatchProvider) RemoteIdentity(ctx context.Context) string {
	return NewWorkspaceProvider(p.repoDir, p.runner).RemoteIdentity(ctx)
}

// GetDiff reads .patch and .diff files in lexical order and parses their
// contents using the same unified-diff parser as git-backed review modes.
func (p *PatchProvider) GetDiff(ctx context.Context) ([]model.Diff, error) {
	info, err := os.Stat(p.diffDir)
	if err != nil {
		return nil, fmt.Errorf("stat diff directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("diff path %q is not a directory", p.diffDir)
	}

	var paths []string
	err = filepath.WalkDir(p.diffDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext == ".patch" || ext == ".diff" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk diff directory: %w", err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("diff directory %q contains no .patch or .diff files", p.diffDir)
	}

	var combined strings.Builder
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read patch %q: %w", path, readErr)
		}
		combined.Write(data)
		combined.WriteString("\n")
	}
	parsed, err := ParseDiffText(ctx, combined.String(), p.repoDir, p.headRef, p.runner)
	if err != nil {
		return nil, fmt.Errorf("parse patches: %w", err)
	}
	return parsed, nil
}

// MaterializePatchSnapshot applies patch files to HEAD in an isolated worktree
// and stores the resulting post-image as an unreferenced commit in the source
// repository. The returned commit remains readable for the lifetime of the
// review without changing the caller's checkout or refs.
func MaterializePatchSnapshot(ctx context.Context, repoDir, diffDir string, runner *gitcmd.Runner) (InputResolution, error) {
	paths, err := patchPaths(diffDir)
	if err != nil {
		return InputResolution{}, err
	}
	baseBytes, err := runner.Output(ctx, repoDir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return InputResolution{}, fmt.Errorf("resolve patch base HEAD: %w", err)
	}
	base := strings.TrimSpace(string(baseBytes))

	worktree, err := os.MkdirTemp("", "ocr-patch-worktree-")
	if err != nil {
		return InputResolution{}, fmt.Errorf("create patch worktree directory: %w", err)
	}
	added := false
	defer func() {
		if added {
			_, _ = runner.Run(context.Background(), repoDir, "worktree", "remove", "--force", worktree)
		}
		_ = os.RemoveAll(worktree)
	}()

	if out, addErr := runner.Run(ctx, repoDir, "worktree", "add", "--detach", worktree, base); addErr != nil {
		return InputResolution{}, fmt.Errorf("create patch worktree: %w: %s", addErr, strings.TrimSpace(out))
	}
	added = true
	for _, path := range paths {
		if err := applyPatchLeniently(ctx, worktree, path, runner); err != nil {
			return InputResolution{}, err
		}
	}
	treeBytes, err := runner.Output(ctx, worktree, "write-tree")
	if err != nil {
		return InputResolution{}, fmt.Errorf("write patch post-image tree: %w", err)
	}
	tree := strings.TrimSpace(string(treeBytes))
	headBytes, err := runner.Output(ctx, repoDir,
		"-c", "user.name=OpenCodeReview", "-c", "user.email=ocr@localhost",
		"commit-tree", tree, "-p", base, "-m", "OpenCodeReview patch post-image")
	if err != nil {
		return InputResolution{}, fmt.Errorf("store patch post-image commit: %w", err)
	}
	head := strings.TrimSpace(string(headBytes))
	return InputResolution{ResolvedBase: base, ResolvedHead: head, ExactRange: base + ".." + head}, nil
}

func applyPatchLeniently(ctx context.Context, worktree, patchPath string, runner *gitcmd.Runner) error {
	if _, err := runner.Run(ctx, worktree, "apply", "--check", "--", patchPath); err == nil {
		if out, applyErr := runner.Run(ctx, worktree, "apply", "--index", "--", patchPath); applyErr != nil {
			return fmt.Errorf("apply patch %q: %w: %s", patchPath, applyErr, strings.TrimSpace(out))
		}
		return nil
	}

	out, rejectErr := runner.Run(ctx, worktree, "apply", "--reject", "--whitespace=nowarn", "--", patchPath)
	rejects, err := rejectedPatchPaths(worktree)
	if err != nil {
		return fmt.Errorf("find rejected hunks for patch %q: %w", patchPath, err)
	}
	if len(rejects) == 0 && rejectErr != nil {
		return fmt.Errorf("patch %q cannot be parsed for lenient application: %w: %s", patchPath, rejectErr, strings.TrimSpace(out))
	}
	for _, rejectPath := range rejects {
		if err := forceRejectedHunks(worktree, rejectPath); err != nil {
			return fmt.Errorf("force rejected hunks from %q: %w", patchPath, err)
		}
	}
	if stageOut, stageErr := runner.Run(ctx, worktree, "add", "-A", "--"); stageErr != nil {
		return fmt.Errorf("stage lenient patch %q: %w: %s", patchPath, stageErr, strings.TrimSpace(stageOut))
	}
	return nil
}

func rejectedPatchPaths(worktree string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(worktree, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".rej") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func forceRejectedHunks(worktree, rejectPath string) error {
	rejectData, err := os.ReadFile(rejectPath)
	if err != nil {
		return err
	}
	targetPath := strings.TrimSuffix(rejectPath, ".rej")
	targetData, err := os.ReadFile(targetPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	trailingNewline := len(targetData) > 0 && targetData[len(targetData)-1] == '\n'
	lines := splitPatchFileLines(string(targetData))
	offset := 0
	for _, hunk := range ParseHunks(string(rejectData)) {
		oldLines, newLines := hunkSides(hunk)
		expected := hunk.OldStart - 1 + offset
		position := nearestSequence(lines, oldLines, expected)
		removeCount := len(oldLines)
		if position < 0 {
			position = max(0, min(expected, len(lines)))
			removeCount = min(hunk.OldCount, len(lines)-position)
		}
		lines = replaceLines(lines, position, removeCount, newLines)
		offset += len(newLines) - removeCount
	}
	content := strings.Join(lines, "\n")
	if trailingNewline || len(lines) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Remove(rejectPath)
}

func splitPatchFileLines(content string) []string {
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func hunkSides(hunk Hunk) ([]string, []string) {
	var oldLines, newLines []string
	for _, line := range hunk.Lines {
		if line.Type != HunkAdded {
			oldLines = append(oldLines, line.Content)
		}
		if line.Type != HunkDeleted {
			newLines = append(newLines, line.Content)
		}
	}
	return oldLines, newLines
}

func nearestSequence(lines, sequence []string, expected int) int {
	if len(sequence) == 0 {
		return max(0, min(expected, len(lines)))
	}
	best, bestDistance := -1, 0
	for i := 0; i+len(sequence) <= len(lines); i++ {
		match := true
		for j := range sequence {
			if lines[i+j] != sequence[j] {
				match = false
				break
			}
		}
		if match {
			distance := i - expected
			if distance < 0 {
				distance = -distance
			}
			if best < 0 || distance < bestDistance {
				best, bestDistance = i, distance
			}
		}
	}
	return best
}

func replaceLines(lines []string, position, removeCount int, replacement []string) []string {
	result := make([]string, 0, len(lines)-removeCount+len(replacement))
	result = append(result, lines[:position]...)
	result = append(result, replacement...)
	result = append(result, lines[position+removeCount:]...)
	return result
}

func patchPaths(diffDir string) ([]string, error) {
	info, err := os.Stat(diffDir)
	if err != nil {
		return nil, fmt.Errorf("stat diff directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("diff path %q is not a directory", diffDir)
	}
	var paths []string
	err = filepath.WalkDir(diffDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext == ".patch" || ext == ".diff" {
				absolute, absErr := filepath.Abs(path)
				if absErr != nil {
					return absErr
				}
				paths = append(paths, absolute)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk diff directory: %w", err)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("diff directory %q contains no .patch or .diff files", diffDir)
	}
	return paths, nil
}
