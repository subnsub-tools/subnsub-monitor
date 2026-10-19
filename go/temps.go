package main

// Temperatures, one row per sensor.
//
// temp_c is one number — the hottest thing on the machine — and that is the
// right headline and the wrong detail: it says a machine is hot without saying
// whether that is the processor, the NVMe drive or the Wi-Fi radio, and those
// three have different limits and different remedies. The kernel already keeps
// them apart under /sys/class/hwmon, one directory per chip:
//
//	hwmon3/name          "nvme"
//	hwmon3/temp1_input   41850          millidegrees
//	hwmon3/temp1_label   "Composite"    optional
//	hwmon3/temp1_max     81850          optional
//	hwmon3/temp1_crit    84850          optional
//
// WHAT A ROW CARRIES is the driver's name, a label for the channel, and
// numbers: no model, no serial, no device path — drivetemp knows which disk it
// is reading and this file never asks it. The label is the one string here
// that is not always the kernel's own. Most drivers write theirs from source
// ("Package id 0", "Composite"), but some hand back a device property — tmp102
// reads `label` from the device tree or ACPI, and whoever wrote that may have
// written a rack position or an asset number. So a label is sent only when it
// is one of the shapes a driver is known to write (knownTempLabel); anything
// else is dropped and the page names the channel by its number (review
// 2026-09-28).
//
// THERMAL ZONES are the older interface and, on most ARM boards and routers,
// the only one. A kernel built with CONFIG_THERMAL_HWMON mirrors each zone
// into hwmon under the zone's own type — with every '-' made a '_', which is
// what the comparison here has to do too — so a zone is only reported when no
// chip of that name has already given a reading. The same sensor twice is not
// two readings; a mirror that could not be read is not a reading at all.
//
// No file here is tagged: the scan is plain file reads under two roots, so its
// tests run on every platform against a directory they build themselves. Only
// Linux calls it.

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// One sensor. ID is what the page keys a tile by: the chip, which of that
// chip's namesakes this is (two NVMe drives are both "nvme"), and the channel.
type Temp struct {
	ID    string   `json:"id"`
	Chip  string   `json:"chip"`
	Label string   `json:"label,omitempty"`
	TempC float64  `json:"temp_c"`
	MaxC  *float64 `json:"max_c,omitempty"`
	CritC *float64 `json:"crit_c,omitempty"`
}

const (
	maxTemps     = 32 // rows a snapshot carries
	maxTempChips = 64 // hwmon directories looked into
	maxTempChans = 128
	maxTempZones = 24
	maxTempText  = 32
	// A sensor's reading changes on a scale of seconds and the sampler calls
	// the collector once a second. Some of these reads are an I2C or SMBus
	// transaction rather than a memory fetch, so they are not taken at that
	// rate: ten seconds is three readings per push. (macOS, where a reading
	// is a process, waits longer — macTempsRefreshSec.)
	tempsRefreshSec = 10
)

// Millidegrees in, °C out. A channel that reads absurd (≤0 or >150°C) is a
// broken, absent or sleeping sensor and is not a reading.
func milliC(raw string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || !finite(n) {
		return 0, false
	}
	c := n / 1000
	if c <= 0 || c > 150 {
		return 0, false
	}
	return round2(c), true
}

// A limit beside a reading, under the same band: drivers that have no limit
// to report fill the file anyway (NVMe's 65261.85°C is the well-known one).
func milliLimit(path string) *float64 {
	raw, ok := readSmall(path)
	if !ok {
		return nil
	}
	if c, ok := milliC(raw); ok {
		return fp(c)
	}
	return nil
}

// A chip or zone name: the characters driver names are made of, and nothing
// else. Anything outside them becomes an underscore rather than being
// dropped, so two names that differ only there stay two names.
func tempChipName(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		if b.Len() >= maxTempText {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// The labels drivers write from their own source, as words and as a word
// followed by a number. The relay holds the same two lists
// and has to: a label this admits and the relay does not
// simply never arrives.
var tempLabelWords = map[string]bool{
	"Composite": true,                             // nvme
	"Tctl":      true, "Tdie": true, "Tccd": true, // k10temp
	"edge": true, "junction": true, "mem": true, // amdgpu
	"SYSTIN": true, "CPUTIN": true, "AUXTIN": true, "PECI Agent": true, // nct67xx
	"PCH_CHIP_CPU_MAX_TEMP": true, "PCH_CHIP_TEMP": true, "PCH_CPU_TEMP": true,
	// the words this helper gives the SMC's keys (mactemps.go smcLabels)
	"CPU proximity": true, "CPU die": true, "CPU heatsink": true,
	"GPU proximity": true, "GPU die": true, "GPU heatsink": true,
	"Memory proximity": true, "Wireless proximity": true, "Drive proximity": true,
	"Palm rest": true, "PCH die": true, "Battery": true,
	"CPU": true, "GPU": true, "SoC": true, "PCH": true, "VRM": true,
	"Motherboard": true, "Chipset": true, "Ambient": true,
}
var tempLabelNumbered = map[string]bool{
	"Package id": true, "Core": true, "Sensor": true, "Tccd": true,
	"AUXTIN": true, "PECI Agent": true, "temp": true, "CPU core": true,
}

// A channel label, or nothing: only what a driver is known to write.
func knownTempLabel(raw string) string {
	l := strings.TrimSpace(raw)
	if len(l) == 0 || len(l) > maxTempText {
		return ""
	}
	if tempLabelWords[l] {
		return l
	}
	// "Core 17", "Tccd3": a listed word, then one to four digits.
	i := len(l)
	for i > 0 && l[i-1] >= '0' && l[i-1] <= '9' {
		i--
	}
	if i == len(l) || len(l)-i > 4 {
		return ""
	}
	if tempLabelNumbered[strings.TrimSuffix(l[:i], " ")] {
		return l
	}
	return ""
}

// A zone's type as thermal_hwmon.c names its mirror: '-' is not allowed in a
// hwmon name, so the kernel writes '_' in its place.
func mirrorName(chip string) string { return strings.ReplaceAll(chip, "-", "_") }

// "hwmon12" → 12. Sorting the directory by this number rather than by name
// keeps hwmon10 after hwmon9.
func suffixNumber(name, prefix, suffix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	mid := name[len(prefix) : len(name)-len(suffix)]
	if mid == "" || len(mid) > 4 {
		return 0, false
	}
	n, err := strconv.Atoi(mid)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func numbered(dir, prefix, suffix string, max int) []int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if n, ok := suffixNumber(e.Name(), prefix, suffix); ok {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// Every sensor under the two roots, chips first and in the kernel's own order.
// Not capped: the hottest reading is taken over all of them (see linuxTemp)
// and capTemps decides what the snapshot carries.
func scanTemps(hwmonRoot, thermalRoot string) []Temp {
	var out []Temp
	nth := map[string]int{}
	// Chips that gave at least one reading — which is a different set from the
	// chips that exist, and the one a zone is compared against: a mirror whose
	// input cannot be read must not take the readable zone down with it.
	read := map[string]bool{}
	for _, h := range numbered(hwmonRoot, "hwmon", "", maxTempChips) {
		dir := hwmonRoot + "/hwmon" + strconv.Itoa(h)
		raw, ok := readSmall(dir + "/name")
		if !ok {
			continue
		}
		chip := tempChipName(raw)
		if chip == "" {
			continue
		}
		// Counted whether or not the chip has a temperature to report, so the
		// second NVMe drive is "2" even on a push where the first one slept.
		nth[chip]++
		for _, ch := range numbered(dir, "temp", "_input", maxTempChans) {
			base := dir + "/temp" + strconv.Itoa(ch)
			raw, ok := readSmall(base + "_input")
			if !ok {
				continue
			}
			c, ok := milliC(raw)
			if !ok {
				continue
			}
			t := Temp{
				ID:    chip + "/" + strconv.Itoa(nth[chip]) + "/temp" + strconv.Itoa(ch),
				Chip:  chip,
				TempC: c,
				MaxC:  milliLimit(base + "_max"),
				CritC: milliLimit(base + "_crit"),
			}
			if raw, ok := readSmall(base + "_label"); ok {
				t.Label = knownTempLabel(raw)
			}
			read[chip] = true
			out = append(out, t)
		}
	}
	for i := 0; i < maxTempZones; i++ {
		dir := thermalRoot + "/thermal_zone" + strconv.Itoa(i)
		raw, ok := readSmall(dir + "/temp")
		if !ok {
			continue
		}
		c, ok := milliC(raw)
		if !ok {
			continue
		}
		chip := ""
		if raw, ok := readSmall(dir + "/type"); ok {
			chip = tempChipName(raw)
		}
		if chip == "" {
			chip = "thermal"
		} else if read[chip] || read[mirrorName(chip)] {
			continue
		}
		nth["zone:"+chip]++
		out = append(out, Temp{
			ID:    chip + "/" + strconv.Itoa(nth["zone:"+chip]) + "/zone",
			Chip:  chip,
			TempC: c,
		})
	}
	return out
}

// "Core 17" — one of coretemp's per-core channels.
func coreLabel(l string) bool {
	if !strings.HasPrefix(l, "Core ") || len(l) == 5 {
		return false
	}
	for _, r := range l[5:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// The rows a snapshot carries. Under the cap, all of them, in scan order.
//
// Over it, the per-core channels go first — but only on a chip that has some
// other channel, which on coretemp is the package reading the cores are
// already summarised by: a 64-core server loses sixty-four rows and no
// information. If that is still too many the list is cut where it stands, in
// scan order rather than by heat: a set that depended on the readings would
// change from push to push and rebuild the page's tiles each time. temp_c is
// taken before this runs, so the hottest sensor is never lost to it.
func capTemps(list []Temp) []Temp {
	if len(list) <= maxTemps {
		return list
	}
	other := map[string]bool{}
	for _, t := range list {
		if !coreLabel(t.Label) {
			other[t.ID[:strings.LastIndexByte(t.ID, '/')]] = true
		}
	}
	out := make([]Temp, 0, len(list))
	for _, t := range list {
		if coreLabel(t.Label) && other[t.ID[:strings.LastIndexByte(t.ID, '/')]] {
			continue
		}
		out = append(out, t)
	}
	if len(out) > maxTemps {
		out = out[:maxTemps]
	}
	return out
}

var tempsCache struct {
	sync.Mutex
	at      float64 // when the cached scan FINISHED
	started float64
	all     []Temp
	done    bool
}

// The scan, or the last one while it is still fresh. Same two rules as
// fillMounts and for the same reasons: no lock is held across the reads (one
// slow sensor must not stand in the way of every other reading), so the scan
// that STARTED later is the one that wins. Unlike the mount table an empty
// answer is cached like any other — a machine with no sensors is a
// configuration, and asking it again every second would not find one.
func cachedTemps(refreshSec float64, scan func() []Temp) []Temp {
	started := now()
	tempsCache.Lock()
	fresh := tempsCache.done && started-tempsCache.at < refreshSec && started >= tempsCache.at
	all := tempsCache.all
	tempsCache.Unlock()
	if fresh {
		return all
	}
	list := scan()
	tempsCache.Lock()
	defer tempsCache.Unlock()
	if !tempsCache.done || started >= tempsCache.started {
		tempsCache.started, tempsCache.at = started, now()
		tempsCache.all, tempsCache.done = list, true
	}
	return tempsCache.all
}

// Both temperature fields from one scan: the hottest reading of ALL sensors,
// and the rows the snapshot has room for.
func fillTemps(s *System, refreshSec float64, scan func() []Temp) {
	all := cachedTemps(refreshSec, scan)
	if len(all) == 0 {
		return
	}
	best := all[0].TempC
	for _, t := range all[1:] {
		if t.TempC > best {
			best = t.TempC
		}
	}
	s.TempC = fp(best)
	// A copy: the cache hands the same backing array to every snapshot.
	s.Temps = append([]Temp(nil), capTemps(all)...)
}
