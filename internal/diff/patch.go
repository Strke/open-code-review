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
	runner  *gitcmd.Runner
}

func NewPatchProvider(repoDir, diffDir string, runner *gitcmd.Runner) *PatchProvider {
	return &PatchProvider{repoDir: repoDir, diffDir: diffDir, runner: runner}
}

func (p *PatchProvider) ResolveInput(context.Context) InputResolution { return InputResolution{} }

func (p *PatchProvider) RemoteIdentity(context.Context) string { return "" }

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
	parsed, err := ParseDiffText(ctx, combined.String(), p.repoDir, "", p.runner)
	if err != nil {
		return nil, fmt.Errorf("parse patches: %w", err)
	}
	return parsed, nil
}
