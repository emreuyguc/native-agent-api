package proctree_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emreuyguc/native-agent-api/internal/proctree"
)

// TestHelperProcess is the process tree: "parent" starts a "child" in a process group of its
// own (as Claude Code does with its shells), prints the child's pid and waits.
func TestHelperProcess(_ *testing.T) {
	switch os.Getenv("PROCTREE_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		child.Env = append(os.Environ(), "PROCTREE_HELPER=child")
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString(strconv.Itoa(child.Process.Pid) + "\n")
		_ = child.Wait()
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestCancelKillsDescendantsInOtherGroups(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd := proctree.Command(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "PROCTREE_HELPER=parent")
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(line[:len(line)-1])
	if pgid, _ := syscall.Getpgid(child); pgid == cmd.Process.Pid {
		t.Fatal("the child must run in a group of its own")
	}
	cancel()
	_ = cmd.Wait()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if exited(child) {
			return
		}
	}
	_ = syscall.Kill(child, syscall.SIGKILL)
	t.Fatal("the child in another process group survived the cancellation")
}

// exited reports whether pid has stopped. A killed process that its parent has not reaped yet
// stays a zombie (state Z on Linux), for example in a container without an init process.
func exited(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// "pid (comm) state ...": comm may contain spaces and parentheses.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))

	return len(fields) > 0 && fields[0] == "Z"
}
