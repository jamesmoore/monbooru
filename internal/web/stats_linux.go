//go:build linux

package web

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func readVmRSS() uint64 { return readSelfStatusKB("VmRSS:") }

func readVmHWM() uint64 { return readSelfStatusKB("VmHWM:") }

func readSelfStatusKB(prefix string) uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, prefix) {
			return parseStatusKB(line[len(prefix):])
		}
	}
	return 0
}

func procRSSAt(pid int) (rssBreakdown, bool) {
	procDir := fmt.Sprintf("/proc/%d", pid)
	if total, anon, file, db, ok := sumSmapsPssAt(procDir); ok {
		return rssBreakdown{total: total, anon: anon, file: file, db: db}, true
	}
	return readStatusRssBreakdown(procDir)
}

func readStatusRssBreakdown(procDir string) (rssBreakdown, bool) {
	f, err := os.Open(procDir + "/status")
	if err != nil {
		return rssBreakdown{}, false
	}
	defer func() { _ = f.Close() }()
	var out rssBreakdown
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			out.total = parseStatusKB(line[len("VmRSS:"):])
		case strings.HasPrefix(line, "RssAnon:"):
			out.anon = parseStatusKB(line[len("RssAnon:"):])
		case strings.HasPrefix(line, "RssFile:"):
			out.file = parseStatusKB(line[len("RssFile:"):])
		}
	}
	if out.total == 0 {
		return rssBreakdown{}, false
	}
	return out, true
}

// Pss from smaps, not Rss: each gallery's SQLite connections map the same
// DB file, and Rss counts a shared page once per mapping. RssAnon is used
// as is, since anonymous pages are not shared.
func procRSS() (rssBreakdown, bool) {
	out, ok := readStatusRssBreakdown("/proc/self")
	if !ok {
		return rssBreakdown{}, false
	}
	if total, _, file, db, ok := sumSmapsPssAt("/proc/self"); ok {
		out.total = total
		out.file = file
		out.db = db
	}
	return out, true
}

func sumSmapsPssAt(procDir string) (totalPss, anonPss, filePss, dbPss uint64, ok bool) {
	f, err := os.Open(procDir + "/smaps")
	if err != nil {
		return 0, 0, 0, 0, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var inFile, inDB bool
	for scanner.Scan() {
		line := scanner.Text()
		if isSmapsHeader(line) {
			path := extractSmapsPath(line)
			inFile = path != "" && !strings.HasPrefix(path, "[")
			inDB = inFile && isDBPath(path)
			continue
		}
		if strings.HasPrefix(line, "Pss:") {
			v := parseStatusKB(line[len("Pss:"):])
			totalPss += v
			if inFile {
				filePss += v
				if inDB {
					dbPss += v
				}
			} else {
				anonPss += v
			}
		}
	}
	return totalPss, anonPss, filePss, dbPss, true
}

// Header lines start with a hex address; data lines with a capitalised key.
func isSmapsHeader(line string) bool {
	if line == "" {
		return false
	}
	c := line[0]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// Header: ADDR-ADDR perms offset dev inode path; the path may contain spaces.
func extractSmapsPath(header string) string {
	fields := strings.Fields(header)
	if len(fields) < 6 {
		return ""
	}
	return strings.Join(fields[5:], " ")
}

func isDBPath(p string) bool {
	return strings.HasSuffix(p, ".db") ||
		strings.HasSuffix(p, ".db-wal") ||
		strings.HasSuffix(p, ".db-shm")
}

func parseStatusKB(rest string) uint64 {
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

func mountUsage(path string) (mountStats, uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return mountStats{}, 0, false
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	// Bavail, as df reports it: Bfree also counts the root-reserved blocks.
	free := st.Bavail * bsize
	used := uint64(0)
	if total > free {
		used = total - free
	}
	pct := 0
	if total > 0 {
		pct = int((used * 100) / total)
	}
	fsid := uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
	return mountStats{
		TotalSize: int64(total),
		FreeSize:  int64(free),
		UsedSize:  int64(used),
		UsedPct:   pct,
	}, fsid, true
}
