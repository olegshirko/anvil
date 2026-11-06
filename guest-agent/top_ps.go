package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// docker top: ps_args support. The route used to always answer with the
// fixed `ps -ef` column set regardless of what the user asked for
// (`docker top <id> aux` is the common one). Now ps_args is parsed into a
// column list; anything beyond -ef/-aux/-o formats is refused, because
// inventing output for flags like --forest would be a lie.

const psHZ = 100 // USER_HZ

// procSnapshot is one process's /proc data, collected once and formatted by
// pure functions, so the rendering is unit-testable off Linux.
type procSnapshot struct {
	pid        int
	ppid       int
	comm       string
	cmd        string
	uid        string // "root" for 0, numeric otherwise
	state      string // stat's state char
	tty        string // "pts/0", or "?" when none
	cpuTicks   uint64 // utime+stime in USER_HZ units
	startTicks uint64 // since boot, USER_HZ units
	vszKB      int64
	rssKB      int64
}

// psCells is a snapshot plus everything derived from the clock.
type psCells struct {
	snap    procSnapshot
	started time.Time
	elapsed time.Duration
	cpuPct  float64 // average %CPU since start, like ps
	memPct  float64 // rss / MemTotal
}

// psColumn renders one cell of a `docker top` row.
type psColumn struct {
	name string
	cell func(c *psCells) string
}

// psFormat renders the GET /containers/{id}/top response for ps_args:
// titles and one row per process, or an error for ps_args it cannot honor.
func psFormat(procs []procSnapshot, now, bootTime time.Time, memTotalKB int64, psArgs string) ([]string, [][]string, error) {
	cols, err := parsePSArgs(psArgs)
	if err != nil {
		return nil, nil, err
	}
	cells := make([]psCells, 0, len(procs))
	for _, p := range procs {
		started := bootTime.Add(time.Duration(p.startTicks) * time.Second / psHZ)
		elapsed := now.Sub(started)
		if elapsed < 0 {
			elapsed = 0
		}
		cpuPct := 0.0
		if elapsed > 0 {
			cpuPct = float64(p.cpuTicks) / psHZ / elapsed.Seconds() * 100
		}
		memPct := 0.0
		if memTotalKB > 0 {
			memPct = float64(p.rssKB) / float64(memTotalKB) * 100
		}
		cells = append(cells, psCells{snap: p, started: started, elapsed: elapsed, cpuPct: cpuPct, memPct: memPct})
	}
	titles := make([]string, len(cols))
	for i, col := range cols {
		titles[i] = col.name
	}
	rows := make([][]string, 0, len(cells))
	for i := range cells {
		row := make([]string, len(cols))
		for j, col := range cols {
			row[j] = col.cell(&cells[i])
		}
		rows = append(rows, row)
	}
	return titles, rows, nil
}

// parsePSArgs maps a ps_args string to a column list; with no ps_args it
// returns the default `ps -ef` view. Recognizes BSD/POSIX flag spellings
// of -ef and aux, and -o field lists (comma- or space-separated).
func parsePSArgs(args string) ([]psColumn, error) {
	tokens := strings.Fields(args)
	sawAux := false
	var format []string
	for i := 0; i < len(tokens); i++ {
		flag := strings.TrimLeft(tokens[i], "-")
		switch {
		case flag == "o":
			// Everything up to the next flag is the field list; ps also
			// accepts commas inside a single token.
			for i++; i < len(tokens); i++ {
				next := tokens[i]
				if len(next) > 1 && next[0] == '-' {
					i--
					break
				}
				format = append(format, strings.Split(next, ",")...)
			}
		case charsAllIn(flag, 'a', 'u', 'x'):
			sawAux = true
		case charsAllIn(flag, 'e', 'f'):
			// the default view; nothing to record
		default:
			return nil, fmt.Errorf("ps_args %q is not supported: only -ef, aux and -o <fields> work", tokens[i])
		}
	}
	if len(format) > 0 {
		cols := make([]psColumn, 0, len(format))
		for _, f := range format {
			col, ok := psFields[strings.ToLower(f)]
			if !ok {
				return nil, fmt.Errorf("ps_args: unknown -o field %q (supported: %s)", f, psFieldNames)
			}
			cols = append(cols, col)
		}
		return cols, nil
	}
	if sawAux {
		return psAuxColumns, nil
	}
	return psEFColumns, nil // the default -ef view
}

func charsAllIn(s string, chars ...byte) bool {
	if s == "" {
		return false
	}
	set := map[byte]bool{}
	for _, c := range chars {
		set[c] = true
	}
	for i := 0; i < len(s); i++ {
		if !set[s[i]] {
			return false
		}
	}
	return true
}

// psColumns builds the default `ps -ef` view.
var psEFColumns = []psColumn{
	{"UID", func(c *psCells) string { return c.snap.uid }},
	{"PID", func(c *psCells) string { return strconv.Itoa(c.snap.pid) }},
	{"PPID", func(c *psCells) string { return strconv.Itoa(c.snap.ppid) }},
	{"C", func(c *psCells) string { return strconv.Itoa(int(c.cpuPct)) }},
	{"STIME", psStimeCell},
	{"TTY", func(c *psCells) string { return c.snap.tty }},
	{"TIME", psCPUTimeCell},
	{"CMD", psCmdCell},
}

// psAuxColumns is the `ps aux` view.
var psAuxColumns = []psColumn{
	{"USER", func(c *psCells) string { return c.snap.uid }},
	{"PID", func(c *psCells) string { return strconv.Itoa(c.snap.pid) }},
	{"%CPU", func(c *psCells) string { return strconv.FormatFloat(c.cpuPct, 'f', 1, 64) }},
	{"%MEM", func(c *psCells) string { return strconv.FormatFloat(c.memPct, 'f', 1, 64) }},
	{"VSZ", func(c *psCells) string { return strconv.FormatInt(c.snap.vszKB, 10) }},
	{"RSS", func(c *psCells) string { return strconv.FormatInt(c.snap.rssKB, 10) }},
	{"TTY", func(c *psCells) string { return c.snap.tty }},
	{"STAT", func(c *psCells) string { return c.snap.state }},
	{"START", psStimeCell},
	{"TIME", psCPUTimeCell},
	{"COMMAND", psCmdCell},
}

// psFields maps `ps -o` field names (lowercase) to columns. Aliases cover
// the spellings procps and busybox accept.
var psFields = map[string]psColumn{
	"pid":     psEFColumns[1],
	"ppid":    psEFColumns[2],
	"user":    psAuxColumns[0],
	"uid":     psEFColumns[0],
	"comm":    {"COMM", func(c *psCells) string { return c.snap.comm }},
	"args":    {"ARGS", psCmdCell},
	"command": {"COMMAND", psCmdCell},
	"cmdline": {"CMDLINE", psCmdCell},
	"cmd":     {"CMD", psCmdCell},
	"%cpu":    psAuxColumns[2],
	"pcpu":    psAuxColumns[2],
	"%mem":    psAuxColumns[3],
	"pmem":    psAuxColumns[3],
	"vsz":     psAuxColumns[4],
	"rss":     psAuxColumns[5],
	"tty":     psEFColumns[5],
	"tt":      psEFColumns[5],
	"stat":    psAuxColumns[7],
	"state":   psAuxColumns[7],
	"s":       psAuxColumns[7],
	"start":   {"START", psStimeCell},
	"stime":   {"STIME", psStimeCell},
	"time":    {"TIME", psCPUTimeCell},
	"cputime": {"CPUTIME", psCPUTimeCell},
	"etime":   {"ELAPSED", psEtimeCell},
	"elapsed": {"ELAPSED", psEtimeCell},
	"c":       psEFColumns[3],
}

// psFieldNames names the supported -o fields in the error message.
const psFieldNames = "pid ppid user uid comm args command cmdline cmd %cpu pcpu %mem pmem vsz rss tty tt stat state s start stime time cputime etime elapsed c"

func psStimeCell(c *psCells) string {
	if c.elapsed > 24*time.Hour {
		return c.started.Format("Jan02")
	}
	return c.started.Format("15:04")
}

func psCPUTimeCell(c *psCells) string {
	secs := c.snap.cpuTicks / psHZ
	return fmt.Sprintf("%02d:%02d:%02d", secs/3600, secs/60%60, secs%60)
}

// psEtimeCell renders elapsed time procps-style: [[dd-]hh:]mm:ss.
func psEtimeCell(c *psCells) string {
	secs := int64(c.elapsed.Seconds())
	days := secs / 86400
	hours := secs % 86400 / 3600
	mins := secs % 3600 / 60
	sec := secs % 60
	var b strings.Builder
	if days > 0 {
		fmt.Fprintf(&b, "%02d-", days)
	}
	if days > 0 || hours > 0 {
		fmt.Fprintf(&b, "%02d:", hours)
	}
	fmt.Fprintf(&b, "%02d:%02d", mins, sec)
	return b.String()
}

func psCmdCell(c *psCells) string {
	if c.snap.cmd != "" {
		return c.snap.cmd
	}
	return "[" + c.snap.comm + "]"
}

// collectProcSnapshots reads /proc for the given pids. Skips processes
// that exited between the pid listing and the read.
func collectProcSnapshots(pids []int, bootTime time.Time) []procSnapshot {
	pageKB := int64(os.Getpagesize() / 1024)
	out := make([]procSnapshot, 0, len(pids))
	for _, pid := range pids {
		stat := readProc(pid, "stat")
		end := strings.LastIndex(stat, ")")
		if end < 0 {
			continue // exited meanwhile
		}
		comm := stat[strings.IndexByte(stat, '(')+1 : end]
		f := strings.Fields(stat[end+1:])
		if len(f) < 20 {
			continue
		}
		// f[0]=state f[1]=ppid f[4]=tty_nr f[11]=utime f[12]=stime f[19]=starttime
		p := procSnapshot{
			pid:        pid,
			ppid:       int(parseUint(f[1])),
			comm:       comm,
			state:      f[0],
			cpuTicks:   parseUint(f[11]) + parseUint(f[12]),
			startTicks: parseUint(f[19]),
			tty:        "?",
		}
		if nr := parseUint(f[4]); nr != 0 {
			major, minor := (nr>>8)&0xfff, (nr&0xff)|((nr>>12)&0xfff00)
			if major == 136 {
				p.tty = fmt.Sprintf("pts/%d", minor)
			}
		}
		for _, line := range strings.Split(readProc(pid, "status"), "\n") {
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if fs := strings.Fields(v); len(fs) > 0 {
					p.uid = fs[0]
				}
			}
		}
		if p.uid == "0" {
			p.uid = "root"
		}
		if sm := strings.Fields(readProc(pid, "statm")); len(sm) >= 2 {
			p.vszKB = int64(parseUint(sm[0])) * pageKB
			p.rssKB = int64(parseUint(sm[1])) * pageKB
		}
		p.cmd = strings.TrimSpace(strings.ReplaceAll(readProc(pid, "cmdline"), "\x00", " "))
		out = append(out, p)
	}
	return out
}

// bootTimeOf reads btime from /proc/stat so start ticks can be turned
// into wall-clock times; returns a zero time when the kernel hides it.
func bootTimeOf(now time.Time) time.Time {
	data, _ := os.ReadFile("/proc/stat")
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if sec, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(sec, 0)
			}
		}
	}
	return time.Time{}
}

// memTotalKB reads MemTotal from /proc/meminfo (the %MEM denominator).
func memTotalKB() int64 {
	data, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			if fs := strings.Fields(v); len(fs) >= 2 {
				if n, err := strconv.ParseInt(fs[0], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}
