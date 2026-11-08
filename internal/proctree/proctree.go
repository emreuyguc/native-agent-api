// Package proctree starts native CLI processes and stops them with everything they started.
//
// A process group is not enough: the CLIs run tools in groups of their own (Claude Code starts
// every shell in a new process group, verified with a background task), so killing the CLI's
// group leaves those tools running after the run. KillTree therefore also kills the group of
// every descendant found in the process table while the CLI still runs. A process that has
// already detached (its parent exited, e.g. `nohup cmd &` from a shell that ended) is outside
// the tree; only the container boundary contains it.
package proctree

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// Command builds a process in its own process group; cancelling ctx kills its tree.
func Command(ctx context.Context, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillTree(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second

	return cmd
}

// KillTree sends SIGKILL to the process group of pid and to the group of each descendant.
func KillTree(pid int) error {
	groups := map[int]bool{pid: true}
	if table, err := processes(); err == nil {
		for _, child := range descendants(table, pid) {
			groups[table[child].pgid] = true
		}
	}
	self := syscall.Getpgrp()
	var first error
	for group := range groups {
		if group <= 1 || group == self {
			continue // Never the gateway's own group, init or "every process".
		}
		if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && err != syscall.ESRCH && first == nil {
			first = err
		}
	}

	return first
}

// Reap kills what is left in the group of an exited process (its own children that stayed in it).
func Reap(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }

type process struct{ ppid, pgid int }

func descendants(table map[int]process, root int) []int {
	children := map[int][]int{}
	for pid, p := range table {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	var found []int
	queue := []int{root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			found = append(found, child)
			queue = append(queue, child)
		}
	}

	return found
}
