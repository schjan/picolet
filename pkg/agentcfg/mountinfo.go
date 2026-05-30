package agentcfg

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

var detectHostDataDirFunc = detectHostDataDir

type mountInfoEntry struct {
	mountID    int
	root       string
	mountPoint string
	fstype     string
	source     string
}

func detectHostDataDir(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		entry, err := parseMountinfoLine(sc.Text())
		if err != nil {
			continue
		}
		if entry.mountPoint == dataDir {
			return entry.root
		}
	}
	return ""
}

func parseMountinfoLine(line string) (mountInfoEntry, error) {
	before, after, ok := strings.Cut(line, " - ")
	if !ok {
		return mountInfoEntry{}, fmt.Errorf("missing separator")
	}
	pre := strings.Split(before, " ")
	post := strings.Split(after, " ")
	if len(pre) < 5 || len(post) < 2 {
		return mountInfoEntry{}, fmt.Errorf("short mountinfo line")
	}
	mountID, err := strconv.Atoi(pre[0])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("mount id: %w", err)
	}
	root, err := unescapeMountinfoPath(pre[3])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("root: %w", err)
	}
	mountPoint, err := unescapeMountinfoPath(pre[4])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("mount point: %w", err)
	}
	source, err := unescapeMountinfoPath(post[1])
	if err != nil {
		return mountInfoEntry{}, fmt.Errorf("source: %w", err)
	}
	return mountInfoEntry{
		mountID:    mountID,
		root:       root,
		mountPoint: mountPoint,
		fstype:     post[0],
		source:     source,
	}, nil
}

func unescapeMountinfoPath(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", fmt.Errorf("truncated escape")
		}
		v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid escape %q: %w", s[i:i+4], err)
		}
		b.WriteByte(byte(v))
		i += 3
	}
	return b.String(), nil
}
