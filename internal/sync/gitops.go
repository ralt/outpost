package sync

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// inspectWorkingTree returns (isGit, currentBranch, headSHA). If cwd is not a
// git working tree, returns (false, "", "").
func inspectWorkingTree(cwd string) (bool, string, string) {
	if out, err := runGit(cwd, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(string(out)) != "true" {
		return false, "", ""
	}
	branch := ""
	if out, err := runGit(cwd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		branch = strings.TrimSpace(string(out))
		if branch == "HEAD" {
			branch = "" // detached
		}
	}
	sha := ""
	if out, err := runGit(cwd, "rev-parse", "HEAD"); err == nil {
		sha = strings.TrimSpace(string(out))
	}
	return true, branch, sha
}

// hasCommit reports whether HEAD resolves on the current branch.
func hasCommit(cwd string) bool {
	_, err := runGit(cwd, "rev-parse", "--verify", "HEAD")
	return err == nil
}

// inProgressOp returns a short tag if cwd is mid rebase/merge/bisect, else "".
func inProgressOp(cwd string) string {
	type marker struct{ Name, Path string }
	for _, m := range []marker{
		{"rebase-apply", ".git/rebase-apply"},
		{"rebase-merge", ".git/rebase-merge"},
		{"merge", ".git/MERGE_HEAD"},
		{"bisect", ".git/BISECT_LOG"},
		{"cherry-pick", ".git/CHERRY_PICK_HEAD"},
		{"revert", ".git/REVERT_HEAD"},
	} {
		if _, err := os.Stat(filepath.Join(cwd, m.Path)); err == nil {
			return m.Name
		}
	}
	return ""
}

// localClean reports whether `git status --porcelain` is empty.
func localClean(cwd string) (bool, error) {
	out, err := runGit(cwd, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) == 0, nil
}

// runGit shells out to git -C cwd <args...>.
func runGit(cwd string, args ...string) ([]byte, error) {
	full := append([]string{"-C", cwd}, args...)
	cmd := exec.Command("git", full...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		return so.Bytes(), fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(se.String()))
	}
	return so.Bytes(), nil
}

