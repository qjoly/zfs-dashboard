package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	zpoolCmd   = envOr("ZPOOL_CMD", "/usr/local/sbin/zpool")
	zfsCmd     = envOr("ZFS_CMD", "/usr/local/sbin/zfs")
	smartCmd   = envOr("SMART_CMD", "/usr/sbin/smartctl")
	diskByID   = envOr("DISK_BY_ID", "/dev/disk/by-id")
	listenAddr = envOr("LISTEN", ":8080")
	// HOST_LD: if set, exec binaries via the host dynamic linker to use host libs.
	// e.g. HOST_LD=/hostlib64/ld-linux-x86-64.so.2 HOST_LIBPATH=/hostlib64:/usr/local/lib
	hostLD      = os.Getenv("HOST_LD")
	hostLibPath = envOr("HOST_LIBPATH", "/hostlib64:/usr/local/lib")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(cmd string, args ...string) (string, error) {
	var c *exec.Cmd
	if hostLD != "" {
		// Use the host dynamic linker so host-compiled binaries work in the container.
		// e.g.: /hostlib64/ld-linux-x86-64.so.2 --library-path /hostlib64:/usr/local/lib /usr/local/sbin/zpool list
		ldArgs := []string{"--library-path", hostLibPath, cmd}
		ldArgs = append(ldArgs, args...)
		c = exec.Command(hostLD, ldArgs...)
	} else {
		c = exec.Command(cmd, args...)
	}
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	err := c.Run()
	return out.String(), err
}

func splitLines(s string) []string {
	var result []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			result = append(result, l)
		}
	}
	return result
}

func humanBytes(s string) string {
	s = strings.TrimSpace(s)
	if s == "-" || s == "" {
		return "-"
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return s
	}
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
		PB = TB * 1024
	)
	switch {
	case n >= PB:
		return fmt.Sprintf("%.2f PB", float64(n)/PB)
	case n >= TB:
		return fmt.Sprintf("%.2f TB", float64(n)/TB)
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/GB)
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.2f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ─── Pool ────────────────────────────────────────────────────────────────────

type Pool struct {
	Name          string `json:"name"`
	Health        string `json:"health"`
	Size          string `json:"size"`
	Allocated     string `json:"allocated"`
	Free          string `json:"free"`
	Fragmentation string `json:"fragmentation"`
	Capacity      int    `json:"capacity"`
	Dedup         string `json:"dedup"`
	Errors        string `json:"errors"`
	Vdevs         []Vdev `json:"vdevs"`
	IOReadOps     string    `json:"io_read_ops"`
	IOWriteOps    string    `json:"io_write_ops"`
	BandRead      string    `json:"band_read"`
	BandWrite     string    `json:"band_write"`
	Scrub         ScrubInfo `json:"scrub"`
}

type Vdev struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Read   string `json:"read"`
	Write  string `json:"write"`
	Cksum  string `json:"cksum"`
	Indent int    `json:"indent"`
}

func getPools() ([]Pool, error) {
	out, err := run(zpoolCmd, "list", "-H", "-p", "-o",
		"name,size,allocated,free,fragmentation,capacity,dedup,health")
	if err != nil {
		return nil, fmt.Errorf("zpool list: %w", err)
	}

	var pools []Pool
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 8 {
			continue
		}
		cap, _ := strconv.Atoi(f[5])
		dedup := f[6]
		if dedup != "-" {
			dedup = dedup + "x"
		}
		frag := f[4]
		if frag != "-" {
			frag = frag + "%"
		}
		p := Pool{
			Name:          f[0],
			Size:          humanBytes(f[1]),
			Allocated:     humanBytes(f[2]),
			Free:          humanBytes(f[3]),
			Fragmentation: frag,
			Capacity:      cap,
			Dedup:         dedup,
			Health:        f[7],
		}
		pools = append(pools, p)
	}

	for i := range pools {
		statusOut, serr := run(zpoolCmd, "status", "-v", pools[i].Name)
		if serr == nil {
			pools[i].Vdevs, pools[i].Errors = parsePoolStatus(statusOut)
			pools[i].Scrub = parseScrubInfo(statusOut)
		}
	}

	ioOut, _ := run(zpoolCmd, "iostat", "-H", "-p")
	if ioOut != "" {
		applyIOStat(ioOut, pools)
	}

	return pools, nil
}

func parsePoolStatus(out string) ([]Vdev, string) {
	var vdevs []Vdev
	errors := "No known data errors"
	inConfig := false

	for _, line := range splitLines(out) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "errors:") {
			errors = strings.TrimSpace(strings.TrimPrefix(trimmed, "errors:"))
			continue
		}
		if strings.HasPrefix(trimmed, "config:") {
			inConfig = true
			continue
		}
		if !inConfig {
			continue
		}
		if strings.Contains(line, "NAME") && strings.Contains(line, "STATE") {
			continue
		}
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, "\t "))
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		v := Vdev{Name: fields[0], State: fields[1], Indent: indent}
		if len(fields) >= 5 {
			v.Read = fields[2]
			v.Write = fields[3]
			v.Cksum = fields[4]
		}
		vdevs = append(vdevs, v)
	}
	return vdevs, errors
}

func applyIOStat(out string, pools []Pool) {
	// format: name\talloc\tfree\tread_ops\twrite_ops\tread_bw\twrite_bw
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		// skip vdev lines (they start with whitespace before tab split leaves name with spaces)
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		name := strings.TrimSpace(f[0])
		for i := range pools {
			if pools[i].Name == name {
				pools[i].IOReadOps = f[3]
				pools[i].IOWriteOps = f[4]
				pools[i].BandRead = humanBytes(f[5]) + "/s"
				pools[i].BandWrite = humanBytes(f[6]) + "/s"
			}
		}
	}
}

// ─── Datasets ────────────────────────────────────────────────────────────────

type Dataset struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Used        string `json:"used"`
	Available   string `json:"available"`
	Referenced  string `json:"referenced"`
	Mountpoint  string `json:"mountpoint"`
	Compression string `json:"compression"`
	Ratio       string `json:"ratio"`
	UsedRaw     int64  `json:"used_raw"`
	AvailRaw    int64  `json:"avail_raw"`
}

func getDatasets() ([]Dataset, error) {
	out, err := run(zfsCmd, "list", "-H", "-p", "-o",
		"name,used,available,referenced,mountpoint,type")
	if err != nil {
		return nil, fmt.Errorf("zfs list: %w", err)
	}

	var datasets []Dataset
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		usedRaw, _ := strconv.ParseInt(f[1], 10, 64)
		availRaw, _ := strconv.ParseInt(f[2], 10, 64)
		datasets = append(datasets, Dataset{
			Name:       f[0],
			Used:       humanBytes(f[1]),
			Available:  humanBytes(f[2]),
			Referenced: humanBytes(f[3]),
			Mountpoint: f[4],
			Type:       f[5],
			UsedRaw:    usedRaw,
			AvailRaw:   availRaw,
		})
	}

	propOut, _ := run(zfsCmd, "get", "-H", "-p", "-o", "name,property,value",
		"compression,compressratio")
	if propOut != "" {
		type kv struct{ comp, ratio string }
		m := make(map[string]*kv)
		for _, line := range splitLines(propOut) {
			f := strings.Split(line, "\t")
			if len(f) < 3 {
				continue
			}
			if m[f[0]] == nil {
				m[f[0]] = &kv{}
			}
			switch f[1] {
			case "compression":
				m[f[0]].comp = f[2]
			case "compressratio":
				m[f[0]].ratio = f[2]
			}
		}
		for i := range datasets {
			if kv, ok := m[datasets[i].Name]; ok {
				datasets[i].Compression = kv.comp
				datasets[i].Ratio = kv.ratio
			}
		}
	}

	return datasets, nil
}

// ─── Snapshots ───────────────────────────────────────────────────────────────

type Snapshot struct {
	Name        string `json:"name"`
	Dataset     string `json:"dataset"`
	ShortName   string `json:"short_name"`
	Used        string `json:"used"`
	Referenced  string `json:"referenced"`
	Creation    string `json:"creation"`
	CreationTS  int64  `json:"creation_ts"`
	UsedRaw     int64  `json:"used_raw"`
	RefRaw      int64  `json:"ref_raw"`
}

func getSnapshots() ([]Snapshot, error) {
	out, err := run(zfsCmd, "list", "-H", "-p", "-t", "snapshot",
		"-o", "name,used,referenced,creation", "-s", "creation")
	if err != nil {
		return nil, fmt.Errorf("zfs list snapshots: %w", err)
	}

	var snaps []Snapshot
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		parts := strings.SplitN(f[0], "@", 2)
		ds, short := f[0], f[0]
		if len(parts) == 2 {
			ds, short = parts[0], parts[1]
		}
		creation := f[3]
		var ts int64
		if t, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			ts = t
			creation = time.Unix(t, 0).Format("2006-01-02 15:04")
		}
		usedRaw, _ := strconv.ParseInt(f[1], 10, 64)
		refRaw, _ := strconv.ParseInt(f[2], 10, 64)
		snaps = append(snaps, Snapshot{
			Name:       f[0],
			Dataset:    ds,
			ShortName:  short,
			Used:       humanBytes(f[1]),
			Referenced: humanBytes(f[2]),
			Creation:   creation,
			CreationTS: ts,
			UsedRaw:    usedRaw,
			RefRaw:     refRaw,
		})
	}
	return snaps, nil
}

// ─── Drives / SMART ──────────────────────────────────────────────────────────

type Drive struct {
	ID       string      `json:"id"`
	Device   string      `json:"device"`
	Model    string      `json:"model"`
	Serial   string      `json:"serial"`
	Firmware string      `json:"firmware"`
	Capacity string      `json:"capacity"`
	Health   string      `json:"health"`
	PowerOn  string      `json:"power_on"`
	Temp     string      `json:"temp"`
	IsNVMe   bool        `json:"is_nvme"`
	Attrs    []SmartAttr `json:"attrs"`
	Error    string      `json:"error,omitempty"`
}

type SmartAttr struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Worst  string `json:"worst"`
	Thresh string `json:"thresh"`
	Raw    string `json:"raw"`
	Warn   bool   `json:"warn"`
}

func getDrives() []Drive {
	patterns := []string{
		diskByID + "/ata-*",
		diskByID + "/nvme-eui.*",
		diskByID + "/nvme-WD*",
		diskByID + "/nvme-Samsung*",
		diskByID + "/nvme-CT*",
	}
	var entries []string
	seen := make(map[string]bool)

	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		entries = append(entries, matches...)
	}

	var drives []Drive
	for _, entry := range entries {
		if strings.Contains(entry, "-part") {
			continue
		}
		realPath, err := filepath.EvalSymlinks(entry)
		if err != nil {
			continue
		}
		if seen[realPath] {
			continue
		}
		seen[realPath] = true

		id := filepath.Base(entry)
		d := Drive{
			ID:     id,
			Device: realPath,
			IsNVMe: strings.Contains(realPath, "nvme"),
		}
		d = fetchSMART(d)
		drives = append(drives, d)
	}

	sort.Slice(drives, func(i, j int) bool {
		return drives[i].Device < drives[j].Device
	})
	return drives
}

func fetchSMART(d Drive) Drive {
	out, _ := run(smartCmd, "-a", d.Device)
	if out == "" {
		// try with -x for NVMe
		out, _ = run(smartCmd, "-x", d.Device)
	}
	if out == "" {
		d.Error = "smartctl returned no output"
		return d
	}

	inAttrs := false
	for _, line := range splitLines(out) {
		line2 := strings.TrimSpace(line)

		if strings.HasPrefix(line2, "Device Model:") || strings.HasPrefix(line2, "Model Number:") {
			d.Model = val(line2)
		} else if strings.HasPrefix(line2, "Serial Number:") {
			d.Serial = val(line2)
		} else if strings.HasPrefix(line2, "Firmware Version:") {
			d.Firmware = val(line2)
		} else if strings.HasPrefix(line2, "User Capacity:") {
			// "User Capacity: 4,000,787,030,016 bytes [4.00 TB]"
			if i := strings.Index(line2, "["); i >= 0 {
				d.Capacity = strings.TrimSuffix(line2[i+1:], "]")
			}
		} else if strings.HasPrefix(line2, "Total NVM Capacity:") {
			if i := strings.Index(line2, "["); i >= 0 {
				d.Capacity = strings.TrimSuffix(line2[i+1:], "]")
			}
		} else if strings.HasPrefix(line2, "SMART overall-health") {
			parts := strings.SplitN(line2, ":", 2)
			if len(parts) == 2 {
				d.Health = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line2, "Temperature:") || strings.HasPrefix(line2, "Temperature Sensor 1:") {
			parts := strings.SplitN(line2, ":", 2)
			if len(parts) == 2 && d.Temp == "" {
				raw := strings.TrimSpace(parts[1])
				// normalize "44 Celsius" → "44 °C"
				raw = strings.ReplaceAll(raw, " Celsius", " °C")
				raw = strings.ReplaceAll(raw, " celsius", " °C")
				// keep only the leading number + unit, drop trailing noise
				if f := strings.Fields(raw); len(f) >= 1 {
					if _, err := strconv.Atoi(f[0]); err == nil {
						d.Temp = f[0] + " °C"
					} else {
						d.Temp = raw
					}
				}
			}
		} else if strings.HasPrefix(line2, "Power On Hours:") {
			raw := strings.ReplaceAll(val(line2), ",", "")
			if h, err := strconv.Atoi(strings.Fields(raw)[0]); err == nil && h > 0 {
				d.PowerOn = fmt.Sprintf("%d h  (%d d)", h, h/24)
			}
		}

		if strings.Contains(line2, "ID#") && strings.Contains(line2, "ATTRIBUTE_NAME") {
			inAttrs = true
			continue
		}
		if inAttrs {
			if line2 == "" || strings.HasPrefix(line2, "=") ||
				strings.HasPrefix(line2, "SMART Error") || strings.HasPrefix(line2, "SMART Self") {
				inAttrs = false
				continue
			}
			fields := strings.Fields(line2)
			if len(fields) < 10 {
				continue
			}
			attr := SmartAttr{
				ID:     fields[0],
				Name:   fields[1],
				Value:  fields[3],
				Worst:  fields[4],
				Thresh: fields[5],
				Raw:    fields[9],
			}
			attrVal, _ := strconv.Atoi(attr.Value)
			attrThr, _ := strconv.Atoi(attr.Thresh)
			if attrThr > 0 && attrVal <= attrThr {
				attr.Warn = true
			}
			switch attr.Name {
			case "Reallocated_Sector_Ct", "Current_Pending_Sector", "Offline_Uncorrectable",
				"Reallocated_Event_Count", "Reported_Uncorrect":
				if raw, _ := strconv.ParseInt(attr.Raw, 10, 64); raw > 0 {
					attr.Warn = true
				}
			case "Temperature_Celsius":
				parts := strings.Fields(attr.Raw)
				if len(parts) > 0 && d.Temp == "" {
					d.Temp = parts[0] + " °C"
				}
			case "Power_On_Hours":
				h, _ := strconv.Atoi(attr.Raw)
				if h > 0 && d.PowerOn == "" {
					d.PowerOn = fmt.Sprintf("%d h  (%d d)", h, h/24)
				}
			}
			d.Attrs = append(d.Attrs, attr)
		}
	}
	return d
}

func val(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// ─── Scrub ───────────────────────────────────────────────────────────────────

type ScrubInfo struct {
	Status    string  `json:"status"`    // completed | in_progress | resilver | resilver_done | none
	Date      string  `json:"date"`
	Duration  string  `json:"duration"`
	Repaired  string  `json:"repaired"`
	Errors    int     `json:"errors"`
	Scanned   string  `json:"scanned,omitempty"`
	Issued    string  `json:"issued,omitempty"`
	Total     string  `json:"total,omitempty"`
	ScanRate  string  `json:"scan_rate,omitempty"`
	IssueRate string  `json:"issue_rate,omitempty"`
	Progress  float64 `json:"progress,omitempty"`  // percent done (in-progress only)
	ETA       string  `json:"eta,omitempty"`       // "X days HH:MM:SS to go"
}

func parseScrubInfo(statusOut string) ScrubInfo {
	lines := splitLines(statusOut)
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "scan:") {
			continue
		}
		t = strings.TrimPrefix(t, "scan:")
		t = strings.TrimSpace(t)

		// scrub completed: "scrub repaired 0B in 00:42:17 with 0 errors on Sun Apr  6 00:26:39 2026"
		if strings.HasPrefix(t, "scrub repaired") || strings.HasPrefix(t, "resilver repaired") {
			status := "completed"
			if strings.HasPrefix(t, "resilver") {
				status = "resilver_done"
			}
			s := ScrubInfo{Status: status}
			// repaired X in DURATION with N errors on DATE
			if after, ok := cutAfter(t, "repaired "); ok {
				if parts := strings.SplitN(after, " in ", 2); len(parts) == 2 {
					s.Repaired = strings.TrimSpace(parts[0])
					rest := parts[1]
					if p2 := strings.SplitN(rest, " with ", 2); len(p2) == 2 {
						s.Duration = strings.TrimSpace(p2[0])
						rest2 := p2[1]
						if p3 := strings.SplitN(rest2, " errors on ", 2); len(p3) == 2 {
							s.Errors, _ = strconv.Atoi(strings.TrimSpace(p3[0]))
							s.Date = strings.TrimSpace(p3[1])
						}
					}
				}
			}
			return s
		}

		// in progress: "scrub in progress since DATE" or "resilver in progress since DATE"
		if strings.Contains(t, "in progress since") {
			status := "in_progress"
			if strings.HasPrefix(t, "resilver") {
				status = "resilver"
			}
			s := ScrubInfo{Status: status}
			if after, ok := cutAfter(t, "since "); ok {
				s.Date = strings.TrimSpace(after)
			}
			// next line: "X scanned at Y/s, Z issued at W/s, T total"
			if i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				fields := strings.Split(next, ",")
				for _, f := range fields {
					f = strings.TrimSpace(f)
					if p := strings.SplitN(f, " scanned at ", 2); len(p) == 2 {
						s.Scanned = strings.TrimSpace(p[0])
						s.ScanRate = strings.TrimSpace(p[1])
					} else if p := strings.SplitN(f, " issued at ", 2); len(p) == 2 {
						s.Issued = strings.TrimSpace(p[0])
						s.IssueRate = strings.TrimSpace(p[1])
					} else if strings.HasSuffix(f, " total") {
						s.Total = strings.TrimSuffix(f, " total")
					}
				}
			}
			// line after that: "XB repaired, Y% done, Z to go"
			if i+2 < len(lines) {
				prog := strings.TrimSpace(lines[i+2])
				for _, f := range strings.Split(prog, ",") {
					f = strings.TrimSpace(f)
					if strings.HasSuffix(f, "% done") {
						pctStr := strings.TrimSuffix(f, "% done")
						// may have "XB repaired" prefix if all on one line — take last token
						parts := strings.Fields(pctStr)
						if len(parts) > 0 {
							s.Progress, _ = strconv.ParseFloat(parts[len(parts)-1], 64)
						}
					} else if strings.HasSuffix(f, " to go") {
						s.ETA = strings.TrimSuffix(f, " to go")
					}
				}
			}
			return s
		}

		// "none requested" or "scrub canceled"
		return ScrubInfo{Status: "none"}
	}
	return ScrubInfo{Status: "none"}
}

func cutAfter(s, sep string) (string, bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[i+len(sep):], true
	}
	return "", false
}

// ─── ARC ─────────────────────────────────────────────────────────────────────

type ARCStats struct {
	Size          string  `json:"size"`
	SizeRaw       int64   `json:"size_raw"`
	MaxSize       string  `json:"max_size"`
	MaxSizeRaw    int64   `json:"max_size_raw"`
	MinSize       string  `json:"min_size"`
	HitRate       float64 `json:"hit_rate"`
	Hits          int64   `json:"hits"`
	Misses        int64   `json:"misses"`
	MFUSize       string  `json:"mfu_size"`
	MRUSize       string  `json:"mru_size"`
	MFURaw        int64   `json:"mfu_raw"`
	MRURaw        int64   `json:"mru_raw"`
	MetaUsed      string  `json:"meta_used"`
	MetaLimit     string  `json:"meta_limit"`
	MetaRaw       int64   `json:"meta_raw"`
	MetaLimitRaw  int64   `json:"meta_limit_raw"`
	DemandHits    int64   `json:"demand_hits"`
	DemandMisses  int64   `json:"demand_misses"`
	PrefetchHits  int64   `json:"prefetch_hits"`
	PrefetchMisses int64  `json:"prefetch_misses"`
	DemandRate    float64 `json:"demand_rate"`
	PrefetchRate  float64 `json:"prefetch_rate"`
	Error         string  `json:"error,omitempty"`
}

func getARCStats() ARCStats {
	data, err := os.ReadFile("/proc/spl/kstat/zfs/arcstats")
	if err != nil {
		return ARCStats{Error: err.Error()}
	}
	m := make(map[string]int64)
	for _, line := range splitLines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 3 {
			if v, err := strconv.ParseInt(f[2], 10, 64); err == nil {
				m[f[0]] = v
			}
		}
	}

	hits := m["hits"]
	misses := m["misses"]
	total := hits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = math.Round(float64(hits)/float64(total)*10000) / 100
	}

	dh := m["demand_data_hits"]
	dm := m["demand_data_misses"]
	ph := m["prefetch_data_hits"]
	pm := m["prefetch_data_misses"]

	demandRate := 0.0
	if dh+dm > 0 {
		demandRate = math.Round(float64(dh)/float64(dh+dm)*10000) / 100
	}
	prefetchRate := 0.0
	if ph+pm > 0 {
		prefetchRate = math.Round(float64(ph)/float64(ph+pm)*10000) / 100
	}

	return ARCStats{
		Size:          humanBytes(strconv.FormatInt(m["size"], 10)),
		SizeRaw:       m["size"],
		MaxSize:       humanBytes(strconv.FormatInt(m["c_max"], 10)),
		MaxSizeRaw:    m["c_max"],
		MinSize:       humanBytes(strconv.FormatInt(m["c_min"], 10)),
		HitRate:       hitRate,
		Hits:          hits,
		Misses:        misses,
		MFUSize:       humanBytes(strconv.FormatInt(m["mfu_size"], 10)),
		MRUSize:       humanBytes(strconv.FormatInt(m["mru_size"], 10)),
		MFURaw:        m["mfu_size"],
		MRURaw:        m["mru_size"],
		MetaUsed:      humanBytes(strconv.FormatInt(m["arc_meta_used"], 10)),
		MetaLimit:     humanBytes(strconv.FormatInt(m["arc_meta_limit"], 10)),
		MetaRaw:       m["arc_meta_used"],
		MetaLimitRaw:  m["arc_meta_limit"],
		DemandHits:    dh,
		DemandMisses:  dm,
		PrefetchHits:  ph,
		PrefetchMisses: pm,
		DemandRate:    demandRate,
		PrefetchRate:  prefetchRate,
	}
}

// ─── API + HTTP ───────────────────────────────────────────────────────────────

type DashData struct {
	Updated   string     `json:"updated"`
	Hostname  string     `json:"hostname"`
	Pools     []Pool     `json:"pools"`
	Datasets  []Dataset  `json:"datasets"`
	Snapshots []Snapshot `json:"snapshots"`
	Drives    []Drive    `json:"drives"`
	ARC       ARCStats   `json:"arc"`
}

func apiData(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()

	pools, perr := getPools()
	if perr != nil {
		log.Printf("getPools: %v", perr)
	}
	datasets, derr := getDatasets()
	if derr != nil {
		log.Printf("getDatasets: %v", derr)
	}
	snaps, serr := getSnapshots()
	if serr != nil {
		log.Printf("getSnapshots: %v", serr)
	}
	drives := getDrives()

	data := DashData{
		Updated:   time.Now().Format("2006-01-02 15:04:05"),
		Hostname:  hostname,
		Pools:     pools,
		Datasets:  datasets,
		Snapshots: snaps,
		Drives:    drives,
		ARC:       getARCStats(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(data)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, htmlPage)
}

func main() {
	log.Printf("ZFS Dashboard listening on %s", listenAddr)
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/api/data", apiData)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

// ─── Embedded HTML ───────────────────────────────────────────────────────────


const htmlPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>ZFS Dashboard</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@300;400;500;600;700&family=Space+Mono:wght@400;700&display=swap" rel="stylesheet">
<script>
// Apply theme before CSS renders to prevent flash
(function(){
  var s=localStorage.getItem('theme');
  var d=window.matchMedia&&window.matchMedia('(prefers-color-scheme: dark)').matches;
  document.documentElement.setAttribute('data-theme',s||(d?'dark':'light'));
})();
</script>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0a0a0a;--surface:#111;--surface2:#161616;
  --border:#222;--border2:#2a2a2a;
  --text:#f5f5f5;--muted:#666;--muted2:#444;
  --red:#ff3c3c;--red-dim:rgba(255,60,60,.12);--red-border:rgba(255,60,60,.25);
  --warn:#ff9500;--warn-dim:rgba(255,149,0,.12);--warn-border:rgba(255,149,0,.25);
  --ok-dim:rgba(255,255,255,.07);--accent:#ff3c3c;
  --hover-overlay:rgba(255,255,255,.025);
  --dot-color:#1e1e1e;
  --donut-ring:#1c1c1c;--donut-tick:#1a1a1a;--donut-sub:#555;--donut-alloc:#383838;
  --donut-text:#f5f5f5;
  --font:'Space Grotesk','Helvetica Neue',Arial,sans-serif;
  --mono:'Space Mono','Courier New',monospace;
}
/* ── Light theme ── */
[data-theme="light"]{
  --bg:#ffffff;--surface:#f7f7f7;--surface2:#efefef;
  --border:#e0e0e0;--border2:#d0d0d0;
  --text:#111111;--muted:#888888;--muted2:#bbbbbb;
  --red-dim:rgba(255,60,60,.08);--red-border:rgba(255,60,60,.2);
  --warn-dim:rgba(255,149,0,.08);--warn-border:rgba(255,149,0,.2);
  --ok-dim:rgba(0,0,0,.04);
  --hover-overlay:rgba(0,0,0,.025);
  --dot-color:#e0e0e0;
  --donut-ring:#ececec;--donut-tick:#d8d8d8;--donut-sub:#aaa;--donut-alloc:#bbb;
  --donut-text:#111111;
}
html{scroll-behavior:smooth}
body{
  background:var(--bg);
  background-image:radial-gradient(circle,var(--dot-color) 1px,transparent 1px);
  background-size:20px 20px;
  color:var(--text);font-family:var(--font);font-size:13px;line-height:1.5;
  min-height:100vh;
  transition:background-color .2s,color .2s;
}
/* ── Wrapper ── */
#wrap{max-width:1440px;margin:0 auto;padding:0 20px 48px}
/* ── Nav ── */
nav{
  display:flex;align-items:center;justify-content:space-between;
  padding:16px 0;border-bottom:1px solid var(--border);margin-bottom:0;
  gap:12px;flex-wrap:wrap;
}
.logo{display:flex;align-items:center;gap:12px}
.logo-mark{
  width:28px;height:28px;background:var(--accent);
  display:grid;grid-template-columns:repeat(3,1fr);gap:2px;padding:4px;flex-shrink:0;
}
.logo-dot{background:#000;border-radius:50%}
.logo-dot.off{background:rgba(0,0,0,.3)}
.logo-text{font-size:14px;font-weight:700;letter-spacing:-0.02em}
.logo-sub{font-size:10px;color:var(--muted);letter-spacing:.08em;text-transform:uppercase;margin-top:1px}
.nav-right{font-size:11px;color:var(--muted);font-family:var(--mono);text-align:right;flex-shrink:0}
.nav-right b{color:var(--text);font-weight:400}
/* ── Tab bar ── */
.tabs{
  display:flex;gap:0;border-bottom:1px solid var(--border);
  margin-bottom:28px;overflow-x:auto;-webkit-overflow-scrolling:touch;
}
.tab{
  padding:12px 20px;font-size:11px;font-weight:700;letter-spacing:.12em;
  text-transform:uppercase;font-family:var(--font);
  background:none;border:none;color:var(--muted);cursor:pointer;
  border-bottom:2px solid transparent;margin-bottom:-1px;white-space:nowrap;
  transition:color .15s;
}
.tab:hover{color:var(--text)}
.tab.active{color:var(--text);border-bottom-color:var(--accent)}
.tab-badge{
  display:inline-block;margin-left:6px;padding:1px 5px;
  background:var(--border2);color:var(--muted);
  font-size:9px;font-family:var(--mono);
}
/* ── Tab pages ── */
.page{display:none}.page.active{display:block}
/* ── Section ── */
section{margin-bottom:36px}
.sec-head{display:flex;align-items:baseline;gap:10px;margin-bottom:14px}
.sec-title{font-size:9px;font-weight:700;letter-spacing:.18em;text-transform:uppercase;color:var(--muted)}
.sec-count{font-size:9px;font-family:var(--mono);color:var(--muted2)}
.sec-line{flex:1;height:1px;background:var(--border)}
/* ── Pool two-column layout ── */
.pool-layout{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border)}
.pool-layout .cards{background:transparent;gap:0;grid-template-columns:1fr}
.pool-aside{background:var(--bg);padding:18px}
.aside-title{font-size:9px;font-weight:700;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin-bottom:14px;font-family:var(--font)}
.ds-bar-row{margin-bottom:12px}
.ds-bar-name{font-size:11px;font-family:var(--mono);color:var(--muted);margin-bottom:3px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.ds-bar-name b{color:var(--text);font-weight:400}
.ds-bar-track{height:3px;background:var(--border2);position:relative;margin-bottom:3px}
.ds-bar-fill{height:100%;position:absolute;top:0;left:0;transition:width .4s}
.ds-bar-meta{display:flex;justify-content:space-between;font-size:9px;font-family:var(--mono);color:var(--muted2)}
/* ── Cards ── */
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:1px;background:var(--border)}
.card{background:var(--bg);padding:18px;position:relative}
.card::before{content:'';position:absolute;top:0;left:0;right:0;height:2px;background:var(--border2)}
.card.card-ok::before{background:var(--text);opacity:.12}
.card.card-warn::before{background:var(--warn)}
.card.card-err::before{background:var(--accent)}
.card-head{display:flex;align-items:flex-start;justify-content:space-between;margin-bottom:14px}
.card-name{font-size:17px;font-weight:700;letter-spacing:-.03em;line-height:1}
.card-sub{font-size:10px;color:var(--muted);font-family:var(--mono);margin-top:3px}
/* ── Pool card inner layout ── */
.card-body{display:flex;gap:0;align-items:flex-start}
.card-left{flex:1;min-width:0}
.card-right{flex-shrink:0;display:flex;flex-direction:column;align-items:center;padding-left:14px;padding-top:2px}
.chart-label{font-size:8px;font-weight:700;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin-top:5px;font-family:var(--mono)}
.chart-sub{font-size:9px;color:var(--muted2);font-family:var(--mono);margin-top:2px;text-align:center}
/* ── Badge ── */
.badge{display:inline-block;padding:2px 7px;font-size:9px;font-weight:700;letter-spacing:.12em;text-transform:uppercase;font-family:var(--mono);border:1px solid}
.badge-ok{color:var(--text);border-color:var(--border2);background:var(--ok-dim)}
.badge-warn{color:var(--warn);border-color:var(--warn-border);background:var(--warn-dim)}
.badge-err{color:var(--accent);border-color:var(--red-border);background:var(--red-dim)}
.badge-dim{color:var(--muted);border-color:var(--border);background:transparent}
/* ── Progress ── */
.cap-row{display:flex;align-items:center;gap:8px;margin:12px 0 8px}
.cap-bar-wrap{flex:1;height:3px;background:var(--border2)}
.cap-bar{height:100%;transition:width .3s}
.cap-ok{background:var(--text)}.cap-warn{background:var(--warn)}.cap-err{background:var(--accent)}
.cap-label{font-size:10px;font-family:var(--mono);color:var(--muted);white-space:nowrap}
/* ── Stats grid ── */
.stats{display:grid;grid-template-columns:repeat(3,1fr);gap:1px;background:var(--border);margin:10px 0}
.stat{background:var(--bg);padding:9px 11px}
.stat-label{font-size:9px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);font-weight:600}
.stat-val{font-size:13px;font-weight:600;margin-top:2px;font-family:var(--mono)}
/* ── IO grid ── */
.io-grid{display:grid;grid-template-columns:repeat(4,1fr);gap:1px;background:var(--border);margin-top:10px}
.io-cell{background:var(--surface);padding:7px 9px}
/* ── Vdev tree ── */
.vdev-list{margin-top:12px;border-top:1px solid var(--border);padding-top:10px}
.vdev-row{display:flex;align-items:center;gap:7px;padding:2px 0;font-size:11px;font-family:var(--mono)}
.vdev-name{flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted)}
.vdev-state{font-size:9px;letter-spacing:.08em;font-weight:700}
.vs-online{color:var(--text)}.vs-degraded{color:var(--warn)}.vs-faulted,.vs-unavail,.vs-removed{color:var(--accent)}.vs-offline{color:var(--muted2)}
.vdev-pip{width:6px;height:6px;border-radius:50%;flex-shrink:0}
.pip-online{background:var(--text)}.pip-degraded{background:var(--warn)}.pip-err{background:var(--accent)}.pip-off{background:var(--muted2)}
/* ── Tables ── */
.tbl-wrap{overflow-x:auto;border:1px solid var(--border);-webkit-overflow-scrolling:touch}
table{width:100%;border-collapse:collapse;font-size:12px}
thead tr{border-bottom:1px solid var(--border2)}
th{padding:8px 12px;text-align:left;font-size:9px;font-weight:700;letter-spacing:.12em;text-transform:uppercase;color:var(--muted);white-space:nowrap}
td{padding:7px 12px;border-bottom:1px solid var(--border);color:var(--text);font-size:12px}
tr:last-child td{border-bottom:none}
tr:hover td{background:var(--hover-overlay)}
.tc-mono{font-family:var(--mono);font-size:11px}
.tc-dim{color:var(--muted)}.tc-warn{color:var(--warn)}.tc-err{color:var(--accent)}
/* ── Snap accordion (dashboard) ── */
.snap-group{border:1px solid var(--border);margin-bottom:2px}
.snap-hd{display:flex;align-items:center;justify-content:space-between;padding:11px 14px;cursor:pointer;user-select:none;background:var(--bg)}
.snap-hd:hover{background:var(--surface)}
.snap-hd-left{display:flex;align-items:center;gap:10px;font-size:12px;font-family:var(--mono)}
.snap-arrow{font-size:9px;color:var(--muted);transition:transform .15s}
.snap-bd{display:none}.snap-bd.open{display:block}
/* ── Drives layout ── */
.drives-layout{display:grid;grid-template-columns:2fr 1fr;gap:1px;background:var(--border)}
.drives-aside{background:var(--bg);padding:18px}
.aside-row{margin-bottom:12px}
.aside-row-head{display:flex;justify-content:space-between;align-items:baseline;margin-bottom:3px}
.aside-row-name{font-size:11px;font-family:var(--mono);color:var(--text)}
.aside-row-val{font-size:10px;font-family:var(--mono);color:var(--muted)}
.aside-bar-track{height:3px;background:var(--border2);position:relative}
.aside-bar-fill{height:100%;position:absolute;top:0;left:0;transition:width .4s}
.aside-health-row{display:flex;align-items:center;gap:8px;padding:5px 0;border-bottom:1px solid var(--border);font-size:11px;font-family:var(--mono)}
.aside-health-row:last-child{border-bottom:none}
.aside-pip{width:6px;height:6px;border-radius:50%;flex-shrink:0}
/* ── Drive cards ── */
.drive-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(420px,1fr));gap:1px;background:var(--border)}
.drive-card{background:var(--bg);overflow:hidden}
.drive-top{display:flex;align-items:flex-start;justify-content:space-between;padding:14px 16px;border-bottom:1px solid var(--border)}
.drive-name{font-size:15px;font-weight:700;letter-spacing:-.02em;font-family:var(--mono)}
.drive-id{font-size:10px;color:var(--muted);margin-top:3px;font-family:var(--mono);max-width:260px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.drive-meta{display:grid;grid-template-columns:repeat(4,1fr);gap:1px;background:var(--border);border-bottom:1px solid var(--border)}
.drive-meta-cell{background:var(--bg);padding:9px 12px}
.dm-label{font-size:9px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);font-weight:600}
.dm-val{font-size:11px;margin-top:2px;font-family:var(--mono)}
.warn-row td{color:var(--warn)!important}
/* ── Errors ── */
.err-box{padding:9px 12px;border-left:3px solid var(--accent);background:var(--red-dim);color:var(--accent);font-size:11px;font-family:var(--mono)}
/* ── Snapshots page ── */
.snap-stats{display:grid;grid-template-columns:repeat(4,1fr);gap:1px;background:var(--border);margin-bottom:24px}
.snap-stat{background:var(--bg);padding:14px 16px}
.snap-stat-label{font-size:9px;font-weight:700;letter-spacing:.14em;text-transform:uppercase;color:var(--muted)}
.snap-stat-val{font-size:20px;font-weight:700;font-family:var(--mono);letter-spacing:-.03em;margin-top:4px}
.snap-stat-sub{font-size:10px;color:var(--muted);font-family:var(--mono);margin-top:2px}
.snap-filters{display:flex;gap:6px;margin-bottom:16px;flex-wrap:wrap;align-items:center}
.snap-filter-btn{padding:5px 12px;border:1px solid var(--border2);background:transparent;color:var(--muted);cursor:pointer;font-family:var(--mono);font-size:10px;letter-spacing:.06em;white-space:nowrap}
.snap-filter-btn:hover{border-color:var(--muted);color:var(--text)}
.snap-filter-btn.active{border-color:var(--text);color:var(--text);background:var(--ok-dim)}
.snap-sort{margin-left:auto;display:flex;gap:6px}
.snap-list{border:1px solid var(--border)}
.snap-item{
  display:grid;
  grid-template-columns:2fr 1.4fr 80px 80px 90px;
  gap:0;border-bottom:1px solid var(--border);
  align-items:center;
}
.snap-item:last-child{border-bottom:none}
.snap-item:hover{background:var(--hover-overlay)}
.snap-item-head{background:var(--surface2);font-size:9px;font-weight:700;letter-spacing:.12em;text-transform:uppercase;color:var(--muted)}
.snap-cell{padding:9px 12px;font-size:12px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.snap-name-ds{font-size:10px;color:var(--muted);font-family:var(--mono)}
.snap-name-short{font-family:var(--mono);font-size:11px;color:var(--text)}
.snap-age-bar{height:2px;background:var(--border2);margin-top:4px;position:relative}
.snap-age-fill{height:100%;position:absolute;top:0;left:0;background:var(--muted2)}
/* ── I/O sparklines ── */
.sparkline-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(220px,1fr));gap:1px;background:var(--border)}
.sparkline-cell{background:var(--bg);padding:14px 16px}
.sparkline-label{font-size:9px;letter-spacing:.1em;text-transform:uppercase;color:var(--muted);font-weight:600;margin-bottom:3px}
.sparkline-val{font-size:15px;font-weight:700;font-family:var(--mono);margin-bottom:8px}
.sparkline-svg{width:100%;height:42px;display:block;overflow:visible}
.sparkline-hint{font-size:9px;font-family:var(--mono);color:var(--muted2);margin-top:4px}
/* ── ARC stacked bar ── */
.arc-stack{display:flex;height:10px;gap:1px;background:var(--border);margin:12px 0 5px}
.arc-stack-seg{height:100%;transition:width .4s}
.arc-stack-legend{display:flex;gap:14px;flex-wrap:wrap}
.arc-leg{display:flex;align-items:center;gap:5px;font-size:9px;font-family:var(--mono);color:var(--muted)}
.arc-leg-swatch{width:10px;height:3px;flex-shrink:0}
/* ── Snapshot bar chart ── */
.snap-chart{border:1px solid var(--border);padding:16px;margin-bottom:20px}
.snap-chart-title{font-size:9px;font-weight:700;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin-bottom:14px}
.snap-brow{display:flex;align-items:center;gap:10px;margin-bottom:5px}
.snap-blabel{width:150px;text-align:right;font-size:10px;font-family:var(--mono);color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;flex-shrink:0}
.snap-btrack{flex:1;height:5px;background:var(--border2);position:relative}
.snap-bfill{height:100%;position:absolute;top:0;left:0;background:var(--text)}
.snap-bval{font-size:9px;font-family:var(--mono);color:var(--muted2);width:60px;flex-shrink:0}
/* ── Loading bar ── */
.lbar{height:2px;background:var(--accent);position:fixed;top:0;left:0;width:100%;animation:lbpulse 1.2s ease-in-out infinite;display:none;z-index:100}
@keyframes lbpulse{0%,100%{opacity:.3}50%{opacity:1}}
/* ── Btn ── */
.btn{padding:5px 12px;border:1px solid var(--border2);background:transparent;color:var(--muted);cursor:pointer;font-family:var(--mono);font-size:10px;letter-spacing:.08em;text-transform:uppercase}
.btn:hover{border-color:var(--muted);color:var(--text)}
/* ═══════════════════════════════════════════════
   RESPONSIVE
   ═══════════════════════════════════════════════ */
/* Tablet ~900px */
@media(max-width:900px){
  .pool-layout{grid-template-columns:1fr}
  .drives-layout{grid-template-columns:1fr}
  .snap-stats{grid-template-columns:repeat(2,1fr)}
  .io-grid{grid-template-columns:repeat(2,1fr)}
}
/* Mobile ~640px */
@media(max-width:640px){
  #wrap{padding:0 12px 40px}
  nav{padding:12px 0}
  .logo-text{font-size:13px}
  .logo-sub{display:none}
  .nav-right{font-size:10px}
  .tab{padding:10px 14px;font-size:10px}
  .cards{grid-template-columns:1fr}
  .drive-grid{grid-template-columns:1fr}
  .stats{grid-template-columns:repeat(2,1fr)}
  .drive-meta{grid-template-columns:repeat(2,1fr)}
  .io-grid{grid-template-columns:repeat(2,1fr)}
  .card-right{display:none} /* hide donut on small screens */
  .snap-stats{grid-template-columns:repeat(2,1fr)}
  .snap-item{grid-template-columns:1fr 80px 80px}
  .snap-cell.snap-ds,.snap-cell.snap-ref{display:none}
  th.col-ds,td.col-ds,th.col-ref,td.col-ref{display:none}
  .snap-filters{gap:4px}
  .snap-filter-btn{padding:4px 8px;font-size:9px}
  section{margin-bottom:28px}
}
/* Small phones ~400px */
@media(max-width:400px){
  .snap-stats{grid-template-columns:1fr 1fr}
  .snap-stat-val{font-size:16px}
  .tab-badge{display:none}
  .io-grid{grid-template-columns:1fr 1fr}
}
/* ── Health tab ── */
.health-scrub-layout{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border)}
.health-card{background:var(--bg);padding:20px}
.hc-title{font-size:9px;font-weight:700;letter-spacing:.18em;text-transform:uppercase;color:var(--muted);margin-bottom:14px}
.hc-pool-name{font-size:19px;font-weight:700;letter-spacing:-.03em;margin-bottom:4px}
.hc-rows{margin-top:14px;display:grid;gap:1px;background:var(--border)}
.hc-row{background:var(--bg);display:flex;justify-content:space-between;align-items:center;padding:8px 12px;font-size:11px}
.hc-row-label{color:var(--muted);text-transform:uppercase;font-size:9px;letter-spacing:.1em;font-weight:600}
.hc-row-val{font-family:var(--mono);font-size:11px}
.scrub-progress-track{height:3px;background:var(--border2);margin:12px 0 4px}
.scrub-progress-fill{height:100%;background:var(--text);transition:width .4s}
.scrub-rates{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border);margin-top:12px}
.scrub-rate-cell{background:var(--surface);padding:9px 12px}
/* ── ARC panel ── */
.arc-layout{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border)}
.arc-main{background:var(--bg);padding:20px}
.arc-gauge-row{display:flex;align-items:center;gap:20px;margin:16px 0}
.arc-gauge-num{font-size:52px;font-weight:700;font-family:var(--mono);letter-spacing:-.04em;line-height:1}
.arc-gauge-label{font-size:9px;font-weight:700;letter-spacing:.16em;text-transform:uppercase;color:var(--muted);margin-top:3px}
.arc-hit-track{height:6px;background:var(--border2);margin:6px 0 2px}
.arc-hit-fill{height:100%;transition:width .4s}
.arc-side{background:var(--bg);padding:20px}
.arc-stats-grid{display:grid;grid-template-columns:1fr 1fr;gap:1px;background:var(--border);margin-top:0}
.arc-stat-cell{background:var(--bg);padding:10px 12px}
.arc-bar-row{margin-bottom:10px}
.arc-bar-label{display:flex;justify-content:space-between;font-size:9px;font-family:var(--mono);color:var(--muted);margin-bottom:3px}
.arc-bar-label b{color:var(--text);font-weight:400}
.arc-bar-track{height:3px;background:var(--border2);position:relative}
.arc-bar-fill{height:100%;position:absolute;top:0;left:0;transition:width .4s}
@media(max-width:900px){
  .arc-layout{grid-template-columns:1fr}
  .health-scrub-layout{grid-template-columns:1fr}
}
</style>
</head>
<body>
<div class="lbar" id="lbar"></div>
<div id="wrap">
  <nav>
    <div class="logo">
      <div class="logo-mark">
        <div class="logo-dot"></div><div class="logo-dot off"></div><div class="logo-dot"></div>
        <div class="logo-dot off"></div><div class="logo-dot"></div><div class="logo-dot off"></div>
        <div class="logo-dot"></div><div class="logo-dot off"></div><div class="logo-dot"></div>
      </div>
      <div>
        <div class="logo-text">ZFS Dashboard</div>
        <div class="logo-sub" id="hostname">—</div>
      </div>
    </div>
    <div class="nav-right">
      <div id="updated">—</div>
      <div style="margin-top:5px;display:flex;gap:6px;justify-content:flex-end">
        <button class="btn" id="theme-btn" onclick="toggleTheme()">☀ Light</button>
        <button class="btn" onclick="refresh()">↺ Refresh</button>
      </div>
    </div>
  </nav>

  <div class="tabs" id="tabs">
    <button class="tab active" onclick="switchTab('dashboard',this)">
      Dashboard
    </button>
    <button class="tab" onclick="switchTab('snapshots',this)">
      Snapshots <span class="tab-badge" id="snap-tab-count">—</span>
    </button>
    <button class="tab" onclick="switchTab('health',this)">
      Health
    </button>
  </div>

  <!-- Dashboard page -->
  <div class="page active" id="page-dashboard">
    <div style="padding:40px 0;text-align:center;color:var(--muted);font-family:var(--mono);font-size:11px;letter-spacing:.1em">
      LOADING<span id="dots">.</span>
    </div>
  </div>

  <!-- Snapshots page -->
  <div class="page" id="page-snapshots"></div>

  <!-- Health page -->
  <div class="page" id="page-health"></div>
</div>

<script>
// ── Theme ──
function initTheme(){
  const saved=localStorage.getItem('theme');
  const prefersDark=window.matchMedia('(prefers-color-scheme: dark)').matches;
  const theme=saved||(prefersDark?'dark':'light');
  applyTheme(theme);
}
function applyTheme(theme){
  document.documentElement.setAttribute('data-theme',theme);
  localStorage.setItem('theme',theme);
  const btn=document.getElementById('theme-btn');
  if(btn) btn.textContent=theme==='dark'?'☀ Light':'☾ Dark';
}
function toggleTheme(){
  const cur=document.documentElement.getAttribute('data-theme')||'dark';
  applyTheme(cur==='dark'?'light':'dark');
}
// ── dots animation while loading ──
(function(){let n=0;setInterval(()=>{const e=document.getElementById('dots');if(e){e.textContent='.'.repeat((n++%3)+1);}},400);})();

// ── Tab switching ──
function switchTab(name, btn) {
  document.querySelectorAll('.page').forEach(p=>p.classList.remove('active'));
  document.querySelectorAll('.tab').forEach(t=>t.classList.remove('active'));
  document.getElementById('page-'+name).classList.add('active');
  if(btn) btn.classList.add('active');
  // hash for direct linking
  history.replaceState(null,'','#'+name);
}

// ── Helpers ──
function healthBadge(h){
  if(!h) return '<span class="badge badge-dim">—</span>';
  const s=h.toUpperCase();
  if(s.includes('ONLINE')||s.includes('PASSED')||s.includes('OK')) return '<span class="badge badge-ok">'+h+'</span>';
  if(s.includes('DEGRADED')||s.includes('WARNING')) return '<span class="badge badge-warn">'+h+'</span>';
  if(s.includes('FAULTED')||s.includes('UNAVAIL')||s.includes('FAILED')||s.includes('REMOVED')) return '<span class="badge badge-err">'+h+'</span>';
  return '<span class="badge badge-dim">'+h+'</span>';
}
function capCls(p){return p>=80?'cap-err':p>=60?'cap-warn':'cap-ok'}
function cardCls(h){
  const s=(h||'').toUpperCase();
  if(s.includes('ONLINE')||s.includes('PASSED')) return 'card-ok';
  if(s.includes('DEGRADED')) return 'card-warn';
  return s.includes('FAULTED')||s.includes('UNAVAIL')?'card-err':'';
}
function pipCls(s){const u=(s||'').toUpperCase();if(u==='ONLINE')return'pip-online';if(u==='DEGRADED')return'pip-degraded';if(u==='FAULTED'||u==='UNAVAIL'||u==='REMOVED')return'pip-err';return'pip-off';}
function stateCls(s){const u=(s||'').toUpperCase();if(u==='ONLINE')return'vs-online';if(u==='DEGRADED')return'vs-degraded';if(u==='FAULTED'||u==='UNAVAIL'||u==='REMOVED')return'vs-faulted';return'vs-offline';}
function parseTemp(t){const m=(t||'').match(/(\d+)/);return m?parseInt(m[1]):0;}
function parsePowerH(p){const m=(p||'').match(/(\d+)\s*h/);return m?parseInt(m[1]):0;}
function fmtBytes(b){
  if(!b||b<0) return '0 B';
  const u=['B','KB','MB','GB','TB','PB'];let i=0;
  while(b>=1024&&i<u.length-1){b/=1024;i++;}
  return b.toFixed(i>0?2:0)+' '+u[i];
}
function timeAgo(ts){
  if(!ts) return '—';
  const s=Math.floor(Date.now()/1000)-ts;
  if(s<60) return s+'s ago';
  if(s<3600) return Math.floor(s/60)+'m ago';
  if(s<86400) return Math.floor(s/3600)+'h ago';
  const d=Math.floor(s/86400);
  if(d<30) return d+'d ago';
  if(d<365) return Math.floor(d/30)+'mo ago';
  return Math.floor(d/365)+'y ago';
}

// ── Donut chart ──
function cssVar(name){
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}
function donutChart(pct,alloc,free){
  const r=40,cx=55,cy=55,sw=9,circ=2*Math.PI*r;
  const used=(pct/100)*circ;
  const stroke=pct>=80?'#ff3c3c':pct>=60?'#ff9500':cssVar('--donut-text');
  const ring=cssVar('--donut-ring');
  const tick=cssVar('--donut-tick');
  const sub=cssVar('--donut-sub');
  const alloc_c=cssVar('--donut-alloc');
  const ticks=[0,25,50,75].map(t=>{
    const a=(t/100)*2*Math.PI-Math.PI/2;
    const x1=cx+(r-sw/2-3)*Math.cos(a),y1=cy+(r-sw/2-3)*Math.sin(a);
    const x2=cx+(r+sw/2+3)*Math.cos(a),y2=cy+(r+sw/2+3)*Math.sin(a);
    return '<line x1="'+x1.toFixed(1)+'" y1="'+y1.toFixed(1)+'" x2="'+x2.toFixed(1)+'" y2="'+y2.toFixed(1)+'" stroke="'+tick+'" stroke-width="1.5"/>';
  }).join('');
  return '<svg viewBox="0 0 110 110" width="100" height="100" style="display:block">'
    +'<circle cx="'+cx+'" cy="'+cy+'" r="'+r+'" fill="none" stroke="'+ring+'" stroke-width="'+sw+'"/>'
    +'<circle cx="'+cx+'" cy="'+cy+'" r="'+r+'" fill="none" stroke="'+stroke+'" stroke-width="'+sw+'"'
    +' stroke-dasharray="'+used.toFixed(2)+' '+(circ-used).toFixed(2)+'"'
    +' transform="rotate(-90 '+cx+' '+cy+')" stroke-linecap="butt"/>'
    +ticks
    +'<text x="'+cx+'" y="'+(cy-5)+'" text-anchor="middle" fill="'+stroke+'" font-size="20" font-weight="700" font-family="Space Mono,monospace">'+pct+'%</text>'
    +'<text x="'+cx+'" y="'+(cy+9)+'" text-anchor="middle" fill="'+sub+'" font-size="7" font-weight="700" font-family="Space Grotesk,sans-serif" letter-spacing="2">USED</text>'
    +'<text x="'+cx+'" y="'+(cy+20)+'" text-anchor="middle" fill="'+alloc_c+'" font-size="7" font-family="Space Mono,monospace">'+alloc+'</text>'
    +'</svg>';
}

// ── I/O history & sparklines ──
const IO_HIST_MAX=40;
let _ioHistory={};  // poolName -> {prev:{r,w}, hist:[{r_delta,w_delta}]}

function updateIOHistory(pools){
  (pools||[]).forEach(p=>{
    const r=parseInt(p.io_read_ops)||0, w=parseInt(p.io_write_ops)||0;
    if(!_ioHistory[p.name]) _ioHistory[p.name]={prev:null,hist:[]};
    const e=_ioHistory[p.name];
    if(e.prev!==null){
      e.hist.push({r:Math.max(0,r-e.prev.r), w:Math.max(0,w-e.prev.w)});
      if(e.hist.length>IO_HIST_MAX) e.hist.shift();
    }
    e.prev={r,w};
  });
}

function areaChart(data, color, fillOpacity=0.08){
  const W=200, H=42;
  if(!data||data.length<2)
    return '<svg class="sparkline-svg" viewBox="0 0 '+W+' '+H+'"></svg>';
  const max=Math.max(...data,1);
  const step=W/(data.length-1);
  const pts=data.map((v,i)=>[
    (i*step).toFixed(1),
    (H-2-(v/max)*(H-6)).toFixed(1)
  ]);
  const line=pts.map(([x,y],i)=>(i===0?'M':'L')+x+','+y).join(' ');
  const area=line+' L'+pts[pts.length-1][0]+','+H+' L0,'+H+' Z';
  return '<svg class="sparkline-svg" viewBox="0 0 '+W+' '+H+'" preserveAspectRatio="none">'
    +'<path d="'+area+'" fill="'+color+'" fill-opacity="'+fillOpacity+'" stroke="none"/>'
    +'<path d="'+line+'" fill="none" stroke="'+color+'" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"/>'
    +'</svg>';
}

function renderIOSection(pools){
  if(!pools||!pools.length) return '';
  let cells='';
  pools.forEach(p=>{
    const h=(_ioHistory[p.name]||{}).hist||[];
    const rData=h.map(x=>x.r), wData=h.map(x=>x.w);
    const curR=rData.length?rData[rData.length-1]:0;
    const curW=wData.length?wData[wData.length-1]:0;
    const hint=h.length<2?'accumulating…':'last '+h.length+' samples · 30s each';
    cells+=''
      +'<div class="sparkline-cell">'
      +'<div class="sparkline-label">'+p.name+' · Read ops</div>'
      +'<div class="sparkline-val">'+curR.toLocaleString()+' <span style="font-size:9px;color:var(--muted)">ops</span></div>'
      +areaChart(rData,'var(--text)')
      +'<div class="sparkline-hint">'+hint+'</div></div>'
      +'<div class="sparkline-cell">'
      +'<div class="sparkline-label">'+p.name+' · Write ops</div>'
      +'<div class="sparkline-val">'+curW.toLocaleString()+' <span style="font-size:9px;color:var(--muted)">ops</span></div>'
      +areaChart(wData,'var(--accent)',0.1)
      +'<div class="sparkline-hint">'+hint+'</div></div>'
      +'<div class="sparkline-cell">'
      +'<div class="sparkline-label">'+p.name+' · Read BW</div>'
      +'<div class="sparkline-val">'+p.band_read+'</div>'
      +areaChart(rData,'var(--text)',0.05)
      +'</div>'
      +'<div class="sparkline-cell">'
      +'<div class="sparkline-label">'+p.name+' · Write BW</div>'
      +'<div class="sparkline-val">'+p.band_write+'</div>'
      +areaChart(wData,'var(--accent)',0.07)
      +'</div>';
  });
  return '<section><div class="sec-head">'
    +'<span class="sec-title">I/O Activity</span>'
    +'<span class="sec-line"></span></div>'
    +'<div class="sparkline-grid">'+cells+'</div></section>';
}

// ── Pools ──
function renderPoolSection(pools,datasets,snapshots){
  return '<div class="pool-layout">'
    +'<div class="cards">'+renderPools(pools)+'</div>'
    +renderPoolAside(pools,datasets,snapshots)
    +'</div>';
}
function renderPools(pools){
  if(!pools||!pools.length) return '<div class="err-box">No pools available</div>';
  return pools.map(p=>{
    const vdevs=(p.vdevs||[]).map(v=>{
      const indent=Math.max(0,(v.indent-1)*11);
      const name=v.name.length>42?v.name.slice(0,12)+'…'+v.name.slice(-22):v.name;
      const errs=[];
      if(v.read&&v.read!=='0') errs.push('<span style="color:var(--accent)">R:'+v.read+'</span>');
      if(v.write&&v.write!=='0') errs.push('<span style="color:var(--warn)">W:'+v.write+'</span>');
      if(v.cksum&&v.cksum!=='0') errs.push('<span style="color:var(--accent)">C:'+v.cksum+'</span>');
      return '<div class="vdev-row" style="padding-left:'+indent+'px">'
        +'<span class="vdev-pip '+pipCls(v.state)+'"></span>'
        +'<span class="vdev-name" title="'+v.name+'">'+name+'</span>'
        +'<span class="vdev-state '+stateCls(v.state)+'">'+v.state+'</span>'
        +(errs.length?'<span style="margin-left:5px;font-size:10px">'+errs.join(' ')+'</span>':'')
        +'</div>';
    }).join('');
    const io=(p.io_read_ops!==undefined)?'<div class="io-grid">'
      +'<div class="io-cell"><div class="stat-label">R ops/s</div><div class="stat-val tc-mono">'+p.io_read_ops+'</div></div>'
      +'<div class="io-cell"><div class="stat-label">W ops/s</div><div class="stat-val tc-mono">'+p.io_write_ops+'</div></div>'
      +'<div class="io-cell"><div class="stat-label">Read BW</div><div class="stat-val tc-mono">'+p.band_read+'</div></div>'
      +'<div class="io-cell"><div class="stat-label">Write BW</div><div class="stat-val tc-mono">'+p.band_write+'</div></div>'
      +'</div>':'';
    const errBox=p.errors&&!p.errors.includes('No known')?'<div class="err-box" style="margin-top:10px">'+p.errors+'</div>':'';
    return '<div class="card '+cardCls(p.health)+'">'
      +'<div class="card-head"><div><div class="card-name">'+p.name+'</div>'
      +'<div class="card-sub">'+p.size+' · RAIDZ1</div></div>'+healthBadge(p.health)+'</div>'
      +'<div class="card-body"><div class="card-left">'
      +'<div class="cap-row"><div class="cap-bar-wrap"><div class="cap-bar '+capCls(p.capacity)+'" style="width:'+p.capacity+'%"></div></div>'
      +'<span class="cap-label">'+p.allocated+' / '+p.size+'</span></div>'
      +'<div class="stats">'
      +'<div class="stat"><div class="stat-label">Free</div><div class="stat-val">'+p.free+'</div></div>'
      +'<div class="stat"><div class="stat-label">Frag</div><div class="stat-val">'+p.fragmentation+'</div></div>'
      +'<div class="stat"><div class="stat-label">Dedup</div><div class="stat-val">'+p.dedup+'</div></div>'
      +'</div>'
      +(vdevs?'<div class="vdev-list">'+vdevs+'</div>':'')
      +io+errBox
      +'</div>'
      +'<div class="card-right">'+donutChart(p.capacity,p.allocated,p.free)
      +'<div class="chart-label">Capacity</div>'
      +'<div class="chart-sub">'+p.free+' free</div>'
      +'</div></div></div>';
  }).join('');
}
function renderPoolAside(pools,datasets,snapshots){
  const pool=pools&&pools[0];
  const topDs=(datasets||[]).filter(d=>{
    if(!pool) return false;
    const rel=d.name.slice(pool.name.length+1);
    return d.name.startsWith(pool.name+'/')&&rel.indexOf('/')===-1&&d.type==='filesystem';
  });
  const totalUsed=topDs.reduce((s,d)=>s+d.used_raw,0);
  const snapCounts={};
  (snapshots||[]).forEach(s=>{snapCounts[s.dataset]=(snapCounts[s.dataset]||0)+1;});
  let html='<div class="pool-aside"><div class="aside-title">Space breakdown</div>';
  topDs.forEach(d=>{
    const pct=totalUsed>0?Math.round(d.used_raw/totalUsed*100):0;
    const color=pct>=80?'var(--accent)':pct>=60?'var(--warn)':'var(--text)';
    const shortName=d.name.split('/').pop();
    const snaps=snapCounts[d.name]||0;
    html+='<div class="ds-bar-row">'
      +'<div class="ds-bar-name"><b>'+shortName+'</b>'
      +(snaps?'<span style="color:var(--muted2);margin-left:8px;font-size:10px">'+snaps+' snaps</span>':'')+'</div>'
      +'<div class="ds-bar-track"><div class="ds-bar-fill" style="width:'+pct+'%;background:'+color+'"></div></div>'
      +'<div class="ds-bar-meta"><span>'+d.used+'</span><span>'+pct+'% of used</span></div></div>';
  });
  if(pool){
    html+='<div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--border)">'
      +'<div style="display:flex;justify-content:space-between;font-size:10px;font-family:var(--mono);color:var(--muted);margin-bottom:5px"><span>Allocated</span><span style="color:var(--text)">'+pool.allocated+'</span></div>'
      +'<div style="display:flex;justify-content:space-between;font-size:10px;font-family:var(--mono);color:var(--muted);margin-bottom:5px"><span>Free</span><span style="color:var(--text)">'+pool.free+'</span></div>'
      +'<div style="display:flex;justify-content:space-between;font-size:10px;font-family:var(--mono);color:var(--muted)"><span>Total</span><span style="color:var(--text)">'+pool.size+'</span></div></div>';
  }
  return html+'</div>';
}

// ── Datasets ──
function renderDatasets(ds){
  if(!ds||!ds.length) return '<div class="err-box">No datasets found</div>';
  const rows=ds.map(d=>'<tr>'
    +'<td class="tc-mono">'+d.name+'</td>'
    +'<td class="tc-dim" style="font-size:9px;letter-spacing:.06em;text-transform:uppercase">'+d.type+'</td>'
    +'<td class="tc-mono">'+d.used+'</td>'
    +'<td class="tc-mono tc-dim">'+d.available+'</td>'
    +'<td class="tc-mono tc-dim">'+d.referenced+'</td>'
    +'<td class="tc-dim">'+d.compression+'</td>'
    +'<td class="tc-mono tc-dim">'+d.ratio+'</td>'
    +'<td class="tc-dim" style="font-size:11px">'+d.mountpoint+'</td>'
    +'</tr>').join('');
  return '<div class="tbl-wrap"><table>'
    +'<thead><tr><th>Name</th><th>Type</th><th>Used</th><th>Available</th><th>Referenced</th><th>Compression</th><th>Ratio</th><th>Mountpoint</th></tr></thead>'
    +'<tbody>'+rows+'</tbody></table></div>';
}

// ── Dashboard snapshots (accordion) ──
function renderSnapshotsAccordion(snapshots){
  if(!snapshots||!snapshots.length)
    return '<div style="color:var(--muted2);font-size:11px;font-family:var(--mono);letter-spacing:.08em">NO SNAPSHOTS</div>';
  const groups={};
  snapshots.forEach(s=>{if(!groups[s.dataset])groups[s.dataset]=[];groups[s.dataset].push(s);});
  return Object.entries(groups).map(([ds,list])=>{
    const rows=list.map(s=>'<tr>'
      +'<td class="tc-mono">'+s.short_name+'</td>'
      +'<td class="tc-dim tc-mono">'+s.creation+'</td>'
      +'<td class="tc-mono">'+s.used+'</td>'
      +'<td class="tc-mono tc-dim">'+s.referenced+'</td>'
      +'</tr>').join('');
    return '<div class="snap-group">'
      +'<div class="snap-hd" onclick="toggleSnap(this)">'
      +'<div class="snap-hd-left"><span>'+ds+'</span>'
      +'<span class="badge badge-dim">'+list.length+'</span></div>'
      +'<span class="snap-arrow">▼</span></div>'
      +'<div class="snap-bd"><div style="overflow-x:auto;-webkit-overflow-scrolling:touch"><table>'
      +'<thead><tr><th>Snapshot</th><th>Created</th><th>Used</th><th>Referenced</th></tr></thead>'
      +'<tbody>'+rows+'</tbody></table></div></div></div>';
  }).join('');
}
function toggleSnap(hd){
  const bd=hd.nextElementSibling,arr=hd.querySelector('.snap-arrow');
  arr.style.transform=bd.classList.toggle('open')?'rotate(180deg)':'';
}

// ── Drives ──
function renderDrivesSection(drives){
  if(!drives||!drives.length)
    return '<div style="color:var(--muted2);font-size:11px;font-family:var(--mono);letter-spacing:.08em">NO DRIVES DETECTED</div>';
  return '<div class="drives-layout">'
    +'<div class="drive-grid">'+renderDrivesGrid(drives)+'</div>'
    +renderDrivesSummary(drives)+'</div>';
}
function renderDrivesGrid(drives){
  return drives.map(d=>{
    const meta='<div class="drive-meta">'
      +'<div class="drive-meta-cell"><div class="dm-label">Model</div><div class="dm-val">'+d.model+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Serial</div><div class="dm-val">'+d.serial+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Capacity</div><div class="dm-val">'+d.capacity+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Firmware</div><div class="dm-val">'+d.firmware+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Power On</div><div class="dm-val">'+d.power_on+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Temperature</div><div class="dm-val">'
      +(d.temp?'<span style="color:var(--warn)">'+d.temp+'</span>':'—')+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Type</div><div class="dm-val">'+(d.is_nvme?'NVMe':'SATA/SAS')+'</div></div>'
      +'<div class="drive-meta-cell"><div class="dm-label">Device</div><div class="dm-val tc-dim">'+d.device+'</div></div>'
      +'</div>';
    const errHtml=d.error?'<div class="err-box">'+d.error+'</div>':'';
    let attrs='';
    if(d.attrs&&d.attrs.length){
      const rows=d.attrs.map(a=>'<tr class="'+(a.warn?'warn-row':'')+'">'
        +'<td class="tc-dim tc-mono">'+a.id+'</td><td>'+a.name+'</td>'
        +'<td class="tc-mono">'+a.value+'</td><td class="tc-mono tc-dim">'+a.worst+'</td>'
        +'<td class="tc-mono tc-dim">'+a.thresh+'</td><td class="tc-mono">'+a.raw+'</td></tr>').join('');
      attrs='<div style="overflow-x:auto;-webkit-overflow-scrolling:touch"><table>'
        +'<thead><tr><th>ID</th><th>Attribute</th><th>Value</th><th>Worst</th><th>Thresh</th><th>Raw</th></tr></thead>'
        +'<tbody>'+rows+'</tbody></table></div>';
    }
    const shortID=d.id.length>44?d.id.slice(0,14)+'…'+d.id.slice(-22):d.id;
    return '<div class="drive-card">'
      +'<div class="drive-top"><div>'
      +'<div class="drive-name">'+d.device+'</div>'
      +'<div class="drive-id" title="'+d.id+'">'+shortID+'</div>'
      +'</div>'+healthBadge(d.health)+'</div>'
      +meta+errHtml+attrs+'</div>';
  }).join('');
}
function renderDrivesSummary(drives){
  const temps=drives.map(d=>({dev:d.device.replace('/dev/',''),val:parseTemp(d.temp)}));
  const maxTemp=Math.max(60,...temps.map(t=>t.val));
  const tempRows=temps.map(t=>{
    const pct=maxTemp>0?Math.round(t.val/maxTemp*100):0;
    const color=t.val>=55?'var(--accent)':t.val>=45?'var(--warn)':'var(--text)';
    return '<div class="aside-row"><div class="aside-row-head">'
      +'<span class="aside-row-name">'+t.dev+'</span>'
      +'<span class="aside-row-val" style="color:'+color+'">'+t.val+'°C</span></div>'
      +'<div class="aside-bar-track"><div class="aside-bar-fill" style="width:'+pct+'%;background:'+color+'"></div></div></div>';
  }).join('');
  const hours=drives.map(d=>({dev:d.device.replace('/dev/',''),val:parsePowerH(d.power_on)}));
  const maxH=Math.max(1,...hours.map(h=>h.val));
  const hourRows=hours.map(h=>{
    const pct=Math.round(h.val/maxH*100);
    return '<div class="aside-row"><div class="aside-row-head">'
      +'<span class="aside-row-name">'+h.dev+'</span>'
      +'<span class="aside-row-val">'+Math.round(h.val/24)+'d</span></div>'
      +'<div class="aside-bar-track"><div class="aside-bar-fill" style="width:'+pct+'%;background:var(--muted2)"></div></div></div>';
  }).join('');
  const healthRows=drives.map(d=>{
    const s=(d.health||'').toUpperCase();
    const ok=s.includes('PASSED')||s.includes('OK');
    const warn=s.includes('WARNING')||s.includes('UNKNOWN');
    const color=ok?'var(--text)':warn?'var(--warn)':'var(--accent)';
    const warnAttrs=(d.attrs||[]).filter(a=>a.warn);
    return '<div class="aside-health-row">'
      +'<span class="aside-pip" style="background:'+color+'"></span>'
      +'<span style="flex:1">'+d.device.replace('/dev/','')+'</span>'
      +'<span style="color:'+color+';font-size:9px;letter-spacing:.08em">'+(d.health||'—')+'</span>'
      +(warnAttrs.length?'<span style="color:var(--warn);font-size:9px;margin-left:5px">⚠ '+warnAttrs.length+'</span>':'')
      +'</div>';
  }).join('');
  return '<div class="drives-aside">'
    +'<div class="aside-title">Temperature</div>'+tempRows
    +'<div class="aside-title" style="margin-top:18px">Power-on hours</div>'+hourRows
    +'<div class="aside-title" style="margin-top:18px">Health</div>'+healthRows
    +'</div>';
}

// ═══════════════════════════════════════════════════════════════
// ── Snapshots page ──
// ═══════════════════════════════════════════════════════════════
let _allSnaps=[], _snapFilter='all', _snapSort='date-desc';

function renderSnapshotsPage(snapshots){
  if(!snapshots||!snapshots.length)
    return '<div style="color:var(--muted2);font-size:11px;font-family:var(--mono);padding:20px 0">NO SNAPSHOTS FOUND</div>';

  _allSnaps=snapshots;

  // stats
  const totalUsed=snapshots.reduce((s,n)=>s+n.used_raw,0);
  const datasets=[...new Set(snapshots.map(s=>s.dataset))];
  const sorted=[...snapshots].sort((a,b)=>b.creation_ts-a.creation_ts);
  const newest=sorted[0], oldest=sorted[sorted.length-1];

  const stats='<div class="snap-stats">'
    +'<div class="snap-stat"><div class="snap-stat-label">Snapshots</div>'
    +'<div class="snap-stat-val">'+snapshots.length+'</div>'
    +'<div class="snap-stat-sub">across '+datasets.length+' datasets</div></div>'
    +'<div class="snap-stat"><div class="snap-stat-label">Space used</div>'
    +'<div class="snap-stat-val">'+fmtBytes(totalUsed)+'</div>'
    +'<div class="snap-stat-sub">by snapshots</div></div>'
    +'<div class="snap-stat"><div class="snap-stat-label">Most recent</div>'
    +'<div class="snap-stat-val" style="font-size:14px;margin-top:6px">'+newest.creation+'</div>'
    +'<div class="snap-stat-sub">'+timeAgo(newest.creation_ts)+'</div></div>'
    +'<div class="snap-stat"><div class="snap-stat-label">Oldest</div>'
    +'<div class="snap-stat-val" style="font-size:14px;margin-top:6px">'+oldest.creation+'</div>'
    +'<div class="snap-stat-sub">'+timeAgo(oldest.creation_ts)+'</div></div>'
    +'</div>';

  // snapshot size bar chart (top 20 by used_raw, non-zero)
  const chartSnaps=[...snapshots].filter(s=>s.used_raw>0).sort((a,b)=>b.used_raw-a.used_raw).slice(0,20);
  let chart='';
  if(chartSnaps.length){
    const maxRaw=chartSnaps[0].used_raw;
    const rows=chartSnaps.map(s=>{
      const pct=Math.round(s.used_raw/maxRaw*100);
      const label=s.dataset.split('/').pop()+'@'+s.short_name;
      return '<div class="snap-brow">'
        +'<div class="snap-blabel" title="'+s.name+'">'+label+'</div>'
        +'<div class="snap-btrack"><div class="snap-bfill" style="width:'+pct+'%"></div></div>'
        +'<div class="snap-bval">'+s.used+'</div>'
        +'</div>';
    }).join('');
    chart='<div class="snap-chart">'
      +'<div class="snap-chart-title">Space used — top snapshots</div>'
      +rows+'</div>';
  }

  // filter buttons
  const filterBtns='<div class="snap-filters">'
    +'<button class="snap-filter-btn'+((_snapFilter==='all')?' active':'')+'" onclick="snapSetFilter(\'all\',this)">All</button>'
    +datasets.map(ds=>'<button class="snap-filter-btn'+((_snapFilter===ds)?' active':'')+'" onclick="snapSetFilter(\''+ds+'\',this)">'+ds.split('/').pop()+'</button>').join('')
    +'<div class="snap-sort">'
    +'<button class="snap-filter-btn'+((_snapSort==='date-desc')?' active':'')+'" onclick="snapSetSort(\'date-desc\',this)">↓ Date</button>'
    +'<button class="snap-filter-btn'+((_snapSort==='date-asc')?' active':'')+'" onclick="snapSetSort(\'date-asc\',this)">↑ Date</button>'
    +'<button class="snap-filter-btn'+((_snapSort==='size')?' active':'')+'" onclick="snapSetSort(\'size\',this)">Size</button>'
    +'</div></div>';

  return stats+chart+filterBtns+'<div id="snap-list">'+buildSnapList()+'</div>';
}

function buildSnapList(){
  let list=[..._allSnaps];
  if(_snapFilter!=='all') list=list.filter(s=>s.dataset===_snapFilter);
  if(_snapSort==='date-desc') list.sort((a,b)=>b.creation_ts-a.creation_ts);
  else if(_snapSort==='date-asc') list.sort((a,b)=>a.creation_ts-b.creation_ts);
  else if(_snapSort==='size') list.sort((a,b)=>b.used_raw-a.used_raw);

  const now=Math.floor(Date.now()/1000);
  const maxAge=list.length?now-list[list.length-1].creation_ts:1;

  // header row
  let html='<div class="snap-list">'
    +'<div class="snap-item snap-item-head">'
    +'<div class="snap-cell">Snapshot</div>'
    +'<div class="snap-cell snap-ds">Dataset</div>'
    +'<div class="snap-cell">Created</div>'
    +'<div class="snap-cell">Used</div>'
    +'<div class="snap-cell snap-ref">Referenced</div>'
    +'</div>';

  list.forEach(s=>{
    const age=now-s.creation_ts;
    const agePct=maxAge>0?Math.min(100,Math.round(age/maxAge*100)):0;
    html+='<div class="snap-item">'
      +'<div class="snap-cell">'
      +'<div class="snap-name-short">@'+s.short_name+'</div>'
      +'<div class="snap-age-bar"><div class="snap-age-fill" style="width:'+agePct+'%"></div></div>'
      +'</div>'
      +'<div class="snap-cell snap-ds"><span style="color:var(--muted);font-family:var(--mono);font-size:11px">'+s.dataset+'</span></div>'
      +'<div class="snap-cell"><span style="font-family:var(--mono);font-size:11px">'+s.creation+'</span>'
      +'<div style="font-size:10px;color:var(--muted2);font-family:var(--mono)">'+timeAgo(s.creation_ts)+'</div></div>'
      +'<div class="snap-cell"><span class="tc-mono">'+(s.used_raw>0?s.used:'<span style="color:var(--muted2)">—</span>')+'</span></div>'
      +'<div class="snap-cell snap-ref"><span class="tc-mono tc-dim">'+s.referenced+'</span></div>'
      +'</div>';
  });
  return html+'</div>';
}

function snapSetFilter(val, btn){
  _snapFilter=val;
  document.querySelectorAll('.snap-filter-btn').forEach(b=>{
    if(!b.closest('.snap-sort')) b.classList.remove('active');
  });
  btn.classList.add('active');
  const el=document.getElementById('snap-list');
  if(el) el.innerHTML=buildSnapList();
}
function snapSetSort(val, btn){
  _snapSort=val;
  btn.closest('.snap-sort').querySelectorAll('.snap-filter-btn').forEach(b=>b.classList.remove('active'));
  btn.classList.add('active');
  const el=document.getElementById('snap-list');
  if(el) el.innerHTML=buildSnapList();
}

// ═══════════════════════════════════════════════════════════════
// ── Health page ──
// ═══════════════════════════════════════════════════════════════
function renderHealthPage(pools, arc){
  let html='';

  // ── Scrub section ──
  html+=secH('Scrub Status',pools&&pools.length?'<div class="health-scrub-layout">'+pools.map(p=>'<div class="health-card">'+renderScrubBody(p)+'</div>'+'<div class="health-card">'+renderVdevHealth(p)+'</div>').join('')+'</div>':'<div class="err-box">No pools</div>');

  // ── ARC section ──
  html+=secH('ARC — Adaptive Replacement Cache',renderARCPanel(arc));

  return html;
}

function secH(title,inner){
  return '<section><div class="sec-head">'
    +'<span class="sec-title">'+title+'</span>'
    +'<span class="sec-line"></span></div>'+inner+'</section>';
}

function renderScrubBody(p){
  const sc=p.scrub||{};
  const st=sc.status||'none';

  const statusColor={
    completed:'var(--text)',in_progress:'var(--warn)',
    resilver:'var(--warn)',resilver_done:'var(--text)',none:'var(--muted2)'
  }[st]||'var(--muted2)';
  const statusLabel={
    completed:'Completed',in_progress:'In Progress',
    resilver:'Resilver In Progress',resilver_done:'Resilver Done',none:'None'
  }[st]||st;

  let detail='';
  if(st==='in_progress'||st==='resilver'){
    const pct=sc.progress||0;
    detail+=''
      +'<div style="display:flex;justify-content:space-between;align-items:baseline;margin:12px 0 4px">'
      +'<span style="font-size:24px;font-weight:700;font-family:var(--mono);color:var(--warn)">'+pct.toFixed(2)+'%</span>'
      +'<span style="font-size:10px;font-family:var(--mono);color:var(--muted)">'+(sc.eta?'ETA: '+sc.eta:'')+'</span>'
      +'</div>'
      +'<div class="scrub-progress-track"><div class="scrub-progress-fill" style="width:'+pct+'%;background:var(--warn)"></div></div>'
      +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted);margin-bottom:10px">Since: '+sc.date+'</div>'
      +'<div class="scrub-rates">'
      +'<div class="scrub-rate-cell"><div class="stat-label">Scanned</div><div class="stat-val tc-mono" style="font-size:11px">'+(sc.scanned||'—')+'</div></div>'
      +'<div class="scrub-rate-cell"><div class="stat-label">Issued</div><div class="stat-val tc-mono" style="font-size:11px">'+(sc.issued||'—')+'</div></div>'
      +'<div class="scrub-rate-cell"><div class="stat-label">Scan Rate</div><div class="stat-val tc-mono" style="font-size:11px">'+(sc.scan_rate||'—')+'</div></div>'
      +'<div class="scrub-rate-cell"><div class="stat-label">Issue Rate</div><div class="stat-val tc-mono" style="font-size:11px">'+(sc.issue_rate||'—')+'</div></div>'
      +'</div>';
  } else if(st==='completed'||st==='resilver_done'){
    const errColor=sc.errors>0?'var(--accent)':'var(--text)';
    detail+='<div class="hc-rows">'
      +'<div class="hc-row"><span class="hc-row-label">Last scrub</span><span class="hc-row-val">'+sc.date+'</span></div>'
      +'<div class="hc-row"><span class="hc-row-label">Duration</span><span class="hc-row-val">'+sc.duration+'</span></div>'
      +'<div class="hc-row"><span class="hc-row-label">Repaired</span><span class="hc-row-val">'+sc.repaired+'</span></div>'
      +'<div class="hc-row"><span class="hc-row-label">Errors</span><span class="hc-row-val" style="color:'+errColor+'">'+sc.errors+'</span></div>'
      +'</div>';
  } else {
    detail+='<div style="margin-top:12px;font-size:11px;font-family:var(--mono);color:var(--muted2)">No scrub data available</div>';
  }

  return '<div class="hc-title">Scrub · '+p.name+'</div>'
    +'<div style="display:flex;align-items:flex-start;justify-content:space-between">'
    +'<div class="hc-pool-name">'+p.name+'</div>'
    +'<span class="badge" style="border-color:'+statusColor+';color:'+statusColor+';background:transparent">'+statusLabel+'</span>'
    +'</div>'
    +detail;
}

function renderVdevHealth(p){
  const vdevs=p.vdevs||[];
  let rows='';
  vdevs.forEach(v=>{
    const indent=Math.max(0,(v.indent-1)*14);
    const name=v.name.length>36?v.name.slice(0,10)+'…'+v.name.slice(-20):v.name;
    const hasErr=(v.read&&v.read!=='0')||(v.write&&v.write!=='0')||(v.cksum&&v.cksum!=='0');
    const errHtml=hasErr
      ?(v.read&&v.read!=='0'?'<span style="color:var(--accent);font-size:9px;margin-left:4px">R:'+v.read+'</span>':'')
      +(v.write&&v.write!=='0'?'<span style="color:var(--warn);font-size:9px;margin-left:4px">W:'+v.write+'</span>':'')
      +(v.cksum&&v.cksum!=='0'?'<span style="color:var(--accent);font-size:9px;margin-left:4px">C:'+v.cksum+'</span>':'')
      :'';
    rows+='<div class="vdev-row" style="padding-left:'+indent+'px">'
      +'<span class="vdev-pip '+pipCls(v.state)+'"></span>'
      +'<span class="vdev-name" title="'+v.name+'">'+name+'</span>'
      +'<span class="vdev-state '+stateCls(v.state)+'">'+v.state+'</span>'
      +errHtml+'</div>';
  });

  const errBox=p.errors&&!p.errors.includes('No known')
    ?'<div class="err-box" style="margin-top:12px">'+p.errors+'</div>':'';

  return '<div class="hc-title">Vdev Tree · '+p.name+'</div>'
    +'<div style="display:flex;align-items:flex-start;justify-content:space-between;margin-bottom:12px">'
    +'<div class="hc-pool-name">'+p.name+'</div>'+healthBadge(p.health)+'</div>'
    +'<div class="vdev-list" style="margin-top:0;border-top:none;padding-top:0">'+rows+'</div>'
    +errBox;
}

function renderARCPanel(arc){
  if(!arc||arc.error) return '<div class="err-box">'+(arc&&arc.error?arc.error:'ARC stats unavailable')+'</div>';

  const hitColor=arc.hit_rate>=95?'var(--text)':arc.hit_rate>=80?'var(--warn)':'var(--accent)';
  const usePct=arc.max_size_raw>0?Math.round(arc.size_raw/arc.max_size_raw*100):0;
  const mfuPct=arc.size_raw>0?Math.round(arc.mfu_raw/arc.size_raw*100):0;
  const mruPct=arc.size_raw>0?Math.round(arc.mru_raw/arc.size_raw*100):0;
  const metaPct=arc.meta_limit_raw>0?Math.round(arc.meta_raw/arc.meta_limit_raw*100):0;

  const main='<div class="arc-main">'
    +'<div class="hc-title">Hit Rate</div>'
    +'<div class="arc-gauge-row">'
    +'<div><div class="arc-gauge-num" style="color:'+hitColor+'">'+arc.hit_rate.toFixed(1)+'%</div>'
    +'<div class="arc-gauge-label">Overall</div></div>'
    +'<div style="flex:1">'
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted);margin-bottom:3px">Demand data</div>'
    +'<div class="arc-hit-track"><div class="arc-hit-fill" style="width:'+arc.demand_rate+'%;background:'+hitColor+'"></div></div>'
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted2)">'+arc.demand_rate.toFixed(1)+'%</div>'
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted);margin-top:8px;margin-bottom:3px">Prefetch</div>'
    +'<div class="arc-hit-track"><div class="arc-hit-fill" style="width:'+arc.prefetch_rate+'%;background:var(--muted2)"></div></div>'
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted2)">'+arc.prefetch_rate.toFixed(1)+'%</div>'
    +'</div></div>'
    +'<div class="hc-rows" style="margin-top:16px">'
    +'<div class="hc-row"><span class="hc-row-label">Total hits</span><span class="hc-row-val">'+arc.hits.toLocaleString()+'</span></div>'
    +'<div class="hc-row"><span class="hc-row-label">Total misses</span><span class="hc-row-val">'+arc.misses.toLocaleString()+'</span></div>'
    +'<div class="hc-row"><span class="hc-row-label">Demand hits</span><span class="hc-row-val">'+arc.demand_hits.toLocaleString()+'</span></div>'
    +'<div class="hc-row"><span class="hc-row-label">Prefetch hits</span><span class="hc-row-val">'+arc.prefetch_hits.toLocaleString()+'</span></div>'
    +'</div></div>';

  // stacked bar: MFU | MRU | Meta | Free (all relative to max_size)
  const base=arc.max_size_raw||1;
  const freePct=Math.max(0,100-usePct);
  const mfuOfMax=Math.round(arc.mfu_raw/base*100);
  const mruOfMax=Math.round(arc.mru_raw/base*100);
  const metaOfMax=Math.round(arc.meta_raw/base*100);
  // clamp so segments don't overflow 100%
  const otherPct=Math.max(0,usePct-mfuOfMax-mruOfMax);

  const stackBar='<div class="arc-stack">'
    +'<div class="arc-stack-seg" style="width:'+mfuOfMax+'%;background:var(--text)" title="MFU '+arc.mfu_size+'"></div>'
    +'<div class="arc-stack-seg" style="width:'+mruOfMax+'%;background:var(--muted)" title="MRU '+arc.mru_size+'"></div>'
    +'<div class="arc-stack-seg" style="width:'+otherPct+'%;background:var(--muted2)" title="Other '+arc.size+'"></div>'
    +'<div class="arc-stack-seg" style="flex:1;background:var(--border2)" title="Free"></div>'
    +'</div>'
    +'<div class="arc-stack-legend">'
    +'<div class="arc-leg"><div class="arc-leg-swatch" style="background:var(--text)"></div>MFU '+arc.mfu_size+' ('+mfuOfMax+'%)</div>'
    +'<div class="arc-leg"><div class="arc-leg-swatch" style="background:var(--muted)"></div>MRU '+arc.mru_size+' ('+mruOfMax+'%)</div>'
    +(otherPct>0?'<div class="arc-leg"><div class="arc-leg-swatch" style="background:var(--muted2)"></div>Other</div>':'')
    +'<div class="arc-leg"><div class="arc-leg-swatch" style="background:var(--border2)"></div>Free ('+freePct+'%)</div>'
    +'</div>';

  const side='<div class="arc-side">'
    +'<div class="hc-title">Memory Breakdown</div>'
    +'<div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:4px">'
    +'<span style="font-size:22px;font-weight:700;font-family:var(--mono);letter-spacing:-.03em">'+arc.size+'</span>'
    +'<span style="font-size:10px;font-family:var(--mono);color:var(--muted)">of '+arc.max_size+' max</span></div>'
    +stackBar
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted2);margin-bottom:16px">'+usePct+'% used · min '+arc.min_size+'</div>'
    +'<div style="padding-top:12px;border-top:1px solid var(--border)">'
    +'<div class="hc-title">Metadata</div>'
    +'<div style="display:flex;justify-content:space-between;align-items:baseline;margin-bottom:4px">'
    +'<span style="font-family:var(--mono);font-size:13px">'+arc.meta_used+'</span>'
    +'<span style="font-size:10px;font-family:var(--mono);color:var(--muted)">limit '+arc.meta_limit+'</span></div>'
    +'<div class="arc-hit-track" style="height:3px"><div class="arc-hit-fill" style="width:'+(metaOfMax/Math.max(usePct/100,0.01)*100).toFixed(0)+'%;background:var(--muted2)"></div></div>'
    +'<div style="font-size:10px;font-family:var(--mono);color:var(--muted2);margin-top:3px">'+metaOfMax+'% of max arc</div>'
    +'</div></div>';

  return '<div class="arc-layout">'+main+side+'</div>';
}

// ── Overall health ──
function overallHealth(data){
  let w='ok';
  (data.pools||[]).forEach(p=>{
    const s=(p.health||'').toUpperCase();
    if(s==='FAULTED'||s==='UNAVAIL') w='err';
    else if(s==='DEGRADED'&&w!=='err') w='warn';
  });
  (data.drives||[]).forEach(d=>{
    if(d.error) return;
    const h=(d.health||'').toUpperCase();
    if(h&&!h.includes('PASSED')&&!h.includes('OK')&&w!=='err') w='warn';
  });
  return w;
}

// ── Section helper ──
function sec(title,arr,inner){
  return '<section><div class="sec-head">'
    +'<span class="sec-title">'+title+'</span>'
    +'<span class="sec-count">['+((arr&&arr.length)||0)+']</span>'
    +'<span class="sec-line"></span></div>'+inner+'</section>';
}

// ── Main refresh ──
async function refresh(){
  document.getElementById('lbar').style.display='block';
  try{
    const res=await fetch('/api/data');
    const data=await res.json();

    document.getElementById('hostname').textContent=(data.hostname||'').toUpperCase();
    document.getElementById('updated').innerHTML=data.updated+'<br><span style="color:var(--muted2)">next in <b id="cd">30</b>s</span>';

    // tab badge
    const snapCount=(data.snapshots||[]).length;
    document.getElementById('snap-tab-count').textContent=snapCount;

    const h=overallHealth(data);
    document.title=(h==='err'?'⛔ ':h==='warn'?'⚠ ':'· ')+'ZFS Dashboard';

    // accumulate I/O history before rendering
    updateIOHistory(data.pools);

    // Dashboard page
    document.getElementById('page-dashboard').innerHTML=
      sec('Pools',data.pools,renderPoolSection(data.pools,data.datasets,data.snapshots))
      +renderIOSection(data.pools)
      +sec('Datasets',data.datasets,renderDatasets(data.datasets))
      +sec('Snapshots',data.snapshots,renderSnapshotsAccordion(data.snapshots))
      +sec('Drives / SMART',data.drives,renderDrivesSection(data.drives));

    // Snapshots page
    document.getElementById('page-snapshots').innerHTML=renderSnapshotsPage(data.snapshots);

    // Health page
    document.getElementById('page-health').innerHTML=renderHealthPage(data.pools,data.arc);

    startCD(30);
  }catch(e){
    document.getElementById('page-dashboard').innerHTML='<div class="err-box">'+e.message+'</div>';
  }finally{
    document.getElementById('lbar').style.display='none';
  }
}

let cdTimer=null;
function startCD(s){
  clearInterval(cdTimer);
  let n=s;
  cdTimer=setInterval(()=>{
    n--;
    const el=document.getElementById('cd');
    if(el) el.textContent=n;
    if(n<=0){clearInterval(cdTimer);refresh();}
  },1000);
}

// restore tab from hash
(function(){
  const hash=(location.hash||'').replace('#','');
  if(hash==='snapshots'||hash==='health'){
    const btn=document.querySelector('[onclick*="\''+hash+'\'"]');
    if(btn) switchTab(hash,btn);
  }
})();

refresh();
</script>
</body>
</html>`
