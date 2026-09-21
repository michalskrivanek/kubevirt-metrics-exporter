// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type memoryStat struct {
	activeAnon   uint64
	inactiveAnon uint64
	activeFile   uint64
	inactiveFile uint64
	anonTHP      uint64
	shmemTHP     uint64
	fileTHP      uint64
}

// readCgroupV2Path reads the cgroup v2 path for a process from
// /proc/<pid>/cgroup. The returned path may be namespace-relative (e.g.
// "/../../<slice>/<scope>") when the reader is in a cgroup namespace;
// use resolveCgroupPath to obtain the absolute host path.
func readCgroupV2Path(pid int, procPath string) (string, error) {
	data, err := os.ReadFile(filepath.Join(procPath, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/%d/cgroup", pid)
}

// findSelfCgroup discovers this process's absolute cgroup path by searching
// cgroupRoot for our PID. Called once at startup.
func findSelfCgroup(cgroupRoot string) (string, error) {
	pidStr := strconv.Itoa(os.Getpid())
	var result string
	walkErr := filepath.WalkDir(cgroupRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != "cgroup.procs" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == pidStr {
				rel, _ := filepath.Rel(cgroupRoot, filepath.Dir(p))
				result = "/" + rel
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("walking cgroup hierarchy: %w", walkErr)
	}
	if result == "" {
		return "", fmt.Errorf("PID %s not found in cgroup hierarchy", pidStr)
	}
	return result, nil
}

// resolveCgroupPath converts a namespace-relative cgroup path to an absolute
// one by joining it with the caller's own absolute cgroup path (nsRoot).
func resolveCgroupPath(nsRelative, nsRoot string) string {
	if nsRoot == "" {
		return nsRelative
	}
	return filepath.Clean(filepath.Join(nsRoot, nsRelative))
}

// readMemoryStat reads per-VMI memory metrics from a cgroup v2 memory.stat file.
func readMemoryStat(cgroupRoot, cgroupPath string) (*memoryStat, error) {
	statPath := filepath.Join(cgroupRoot, cgroupPath, "memory.stat")
	f, err := os.Open(statPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", statPath, err)
	}
	defer f.Close()

	values := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) != 2 {
			continue
		}
		v, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		values[parts[0]] = v
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", statPath, err)
	}

	return &memoryStat{
		activeAnon:   values["active_anon"],
		inactiveAnon: values["inactive_anon"],
		activeFile:   values["active_file"],
		inactiveFile: values["inactive_file"],
		anonTHP:      values["anon_thp"],
		shmemTHP:     values["shmem_thp"],
		fileTHP:      values["file_thp"],
	}, nil
}
