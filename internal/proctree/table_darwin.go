package proctree

import "golang.org/x/sys/unix"

func processes() (map[int]process, error) {
	list, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	table := make(map[int]process, len(list))
	for i := range list {
		table[int(list[i].Proc.P_pid)] = process{ppid: int(list[i].Eproc.Ppid), pgid: int(list[i].Eproc.Pgid)}
	}

	return table, nil
}
