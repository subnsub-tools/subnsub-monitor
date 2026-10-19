package main

// The half of the macOS temperature reading that is not macOS: what a sensor
// is called, how its bytes become degrees, and what the parent believes of
// what the child printed. No build tag, for the reason macstats.go gives —
// this is tested everywhere, and the only part that cannot be is the part
// that calls into IOKit (temps_darwin.go).
//
// WHERE THE READINGS COME FROM. An Intel Mac keeps its sensors in the SMC,
// under four-character keys that begin with T ("TC0P"); each is asked for by
// name and answers in a fixed-point format. An Apple Silicon Mac publishes
// them as HID services with a product name ("PMU tdie1") and a temperature
// event. Neither has a command-line tool that prints them without root, and
// both are reached through IOKit — which this build, being CGO_ENABLED=0,
// calls through purego.
//
// WHY A CHILD PROCESS. Those are calls into somebody else's library through
// pointers this program computed, and a mistake in one is a segfault that no
// recover() sees. So the helper does not make them: it runs itself as
// `subnsub-monitor temps`, which makes them, prints rows, and exits — and a
// child that crashes, hangs or prints nonsense costs the temperature tiles and
// nothing else. The parent reads the output the way it reads vm_stat's.

import (
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A spawn of a twenty-megabyte binary is not a file read; the push interval is
// as often as the page could show a new number anyway.
const macTempsRefreshSec = 30

// How many rows a child may print before the parent stops reading them.
const maxTempRowsIn = 256

// The SMC keys whose meaning is settled, in the words the page shows. A key
// that is not here is still reported — under its own four characters, which
// is what every SMC tool prints and the only name for it that is certainly
// right.
var smcLabels = map[string]string{
	"TC0P": "CPU proximity", "TC0D": "CPU die", "TC0H": "CPU heatsink",
	"TG0P": "GPU proximity", "TG0D": "GPU die", "TG0H": "GPU heatsink",
	"TM0P": "Memory proximity", "TW0P": "Wireless proximity",
	"TH0P": "Drive proximity", "TA0P": "Ambient", "TB0T": "Battery",
	"Ts0P": "Palm rest", "TPCD": "PCH die",
}

func smcLabel(key string) string {
	if l, ok := smcLabels[key]; ok {
		return l
	}
	// TC1C … TC9C: the cores, counted from one.
	if len(key) == 4 && key[:2] == "TC" && key[3] == 'C' && key[2] >= '1' && key[2] <= '9' {
		return "CPU core " + key[2:3]
	}
	return ""
}

// A key this file will ask the SMC about and put in an id: T, then three
// letters or digits. The SMC's alphabet is wider than that (keys hold spaces
// and punctuation) and none of the temperature keys use the rest of it.
func smcTempKey(key string) bool {
	if len(key) != 4 || key[0] != 'T' {
		return false
	}
	for i := 1; i < 4; i++ {
		c := key[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Degrees out of the two formats temperature keys come in: sp78, a signed
// 8.8 fixed-point number, big-endian; and flt, an IEEE single, little-endian.
// Anything else is some other kind of key that happens to begin with T.
func smcCelsius(typ string, data []byte) (float64, bool) {
	var v float64
	switch {
	case typ == "sp78" && len(data) == 2:
		v = float64(int16(binary.BigEndian.Uint16(data))) / 256
	case typ == "flt " && len(data) == 4:
		v = float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
	default:
		return 0, false
	}
	return celsiusReading(v)
}

// The band every reading here is held to, the same one milliC keeps: an empty
// socket reads -127 on an SMC, and that is not a temperature.
func celsiusReading(c float64) (float64, bool) {
	if !finite(c) || c <= 0 || c > 150 {
		return 0, false
	}
	return round2(c), true
}

// SMC rows in the order they are reported: the ones with a name first, then
// the rest, each by key — so the cut at maxTemps, when a machine has more
// sensors than that, falls on the ones nobody can put a word to.
func smcRows(read map[string]float64) []Temp {
	keys := make([]string, 0, len(read))
	for k := range read {
		if smcTempKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := smcLabel(keys[i]) != "", smcLabel(keys[j]) != ""
		if a != b {
			return a
		}
		return keys[i] < keys[j]
	})
	out := make([]Temp, 0, len(keys))
	for _, k := range keys {
		out = append(out, Temp{ID: "smc/1/" + k, Chip: "smc", Label: smcLabel(k), TempC: read[k]})
	}
	return out
}

// The product names Apple Silicon gives its temperature services. Firmware
// strings, the same on every machine of a model — and held to this list all
// the same, because the property they are read from is one any HID device
// may set.
var hidLabelRE = regexp.MustCompile(`^(?:PMU2? t(?:die|dev|cal)\d{1,2}|NAND CH\d temp|gas gauge battery|` +
	`(?:PMGR SOC Die|SOC MTR|eACC MTR|pACC MTR|GPU MTR|ANE MTR|ISP MTR) Temp Sensor\d{1,2})$`)

// One HID service: its name, and its reading if it gave one. Services that
// gave none are carried this far too, because which sensor a row is has to be
// settled over the whole set (hidRows).
type hidReading struct {
	name string
	c    float64
	ok   bool
}

// A channel number has to mean the same sensor on the next push, and nothing
// about the order the services arrive in is promised. A position in the list
// of this push's readings was the first answer and the wrong one: a sensor
// that read nothing for one push moved every sensor after it down a number,
// and the page put one sensor's reading on another's tile (review
// 2026-09-29). So the number is made from the NAME — the same on every push,
// whatever else did or did not answer — and a service that cannot be told
// apart by name, because it has none or shares it, is not reported: a row
// that might be either of two sensors is not a reading of one.
func hidChannel(name string) int { return int(crc32.ChecksumIEEE([]byte(name))%9999) + 1 }

func hidRows(all []hidReading) []Temp {
	named := map[string]int{}
	for _, r := range all {
		named[r.name]++
	}
	var read []hidReading
	for _, r := range all {
		if r.ok && r.name != "" && len(r.name) <= 128 && named[r.name] == 1 {
			read = append(read, r)
		}
	}
	sort.Slice(read, func(i, j int) bool { return read[i].name < read[j].name })
	out := make([]Temp, 0, len(read))
	taken := map[int]bool{}
	for _, r := range read {
		ch := hidChannel(r.name)
		// Two names on one number: the first by name keeps it, every push.
		if taken[ch] || len(out) >= maxTempRowsIn {
			continue
		}
		taken[ch] = true
		t := Temp{ID: "hid/1/temp" + strconv.Itoa(ch), Chip: "hid", TempC: r.c}
		if hidLabelRE.MatchString(r.name) {
			t.Label = r.name
		}
		out = append(out, t)
	}
	return out
}

// A label from any source this helper reads: a Linux driver's, an SMC key's
// word, an Apple Silicon service's name. monitor-worker/src/clean.js holds
// the same lists (TEMP_LABEL_RE).
func admitTempLabel(l string) string {
	l = strings.TrimSpace(l)
	if k := knownTempLabel(l); k != "" {
		return k
	}
	if len(l) <= maxTempText && hidLabelRE.MatchString(l) {
		return l
	}
	return ""
}

var tempIDRE = regexp.MustCompile(`^([A-Za-z0-9_.-]{1,32})/[1-9][0-9]{0,2}/(?:temp[0-9]{1,4}|zone|T[A-Za-z0-9]{3})$`)

// What the child printed, as far as it can be believed. The child is this
// same binary, and is read as if it were not: it has just been inside a
// library this program does not control, and the rows it prints go on a wire.
// Every field is rebuilt; a row that is not a row is dropped, and output that
// is not a list is no readings at all.
func parseTempRows(out string) []Temp {
	var in []Temp
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &in); err != nil {
		return nil
	}
	if len(in) > maxTempRowsIn {
		in = in[:maxTempRowsIn]
	}
	rows := make([]Temp, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		m := tempIDRE.FindStringSubmatch(t.ID)
		c, ok := celsiusReading(t.TempC)
		if m == nil || t.Chip != m[1] || !ok || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		row := Temp{ID: t.ID, Chip: t.Chip, Label: admitTempLabel(t.Label), TempC: c}
		for _, lim := range []struct {
			in  *float64
			out **float64
		}{{t.MaxC, &row.MaxC}, {t.CritC, &row.CritC}} {
			if lim.in != nil {
				if v, ok := celsiusReading(*lim.in); ok {
					*lim.out = fp(v)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}
