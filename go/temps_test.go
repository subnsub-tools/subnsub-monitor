package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A sysfs the test builds for itself: {relative path: contents}.
func tempTree(t *testing.T, files map[string]string) (hwmon, thermal string) {
	t.Helper()
	root := t.TempDir()
	hwmon, thermal = filepath.Join(root, "hwmon"), filepath.Join(root, "thermal")
	for _, d := range []string{hwmon, thermal} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return hwmon, thermal
}

func tempIDs(list []Temp) string {
	ids := make([]string, len(list))
	for i, t := range list {
		ids[i] = t.ID
	}
	return strings.Join(ids, " ")
}

func TestScanTempsReadsEveryChip(t *testing.T) {
	hwmon, thermal := tempTree(t, map[string]string{
		"hwmon/hwmon0/name":         "acpitz\n",
		"hwmon/hwmon0/temp1_input":  "27800\n",
		"hwmon/hwmon2/name":         "coretemp\n",
		"hwmon/hwmon2/temp1_input":  "54000\n",
		"hwmon/hwmon2/temp1_label":  "Package id 0\n",
		"hwmon/hwmon2/temp1_max":    "80000\n",
		"hwmon/hwmon2/temp1_crit":   "100000\n",
		"hwmon/hwmon2/temp2_input":  "51000\n",
		"hwmon/hwmon2/temp2_label":  "Core 0\n",
		"hwmon/hwmon10/name":        "nvme\n",
		"hwmon/hwmon10/temp1_input": "41850\n",
		"hwmon/hwmon10/temp1_label": "Composite\n",
		// The limit NVMe reports when it has none.
		"hwmon/hwmon10/temp1_max":  "65261850\n",
		"hwmon/hwmon9/name":        "nvme\n",
		"hwmon/hwmon9/temp1_input": "38850\n",
		// A fan controller: a chip with no temperature at all.
		"hwmon/hwmon3/name":       "pwmfan\n",
		"hwmon/hwmon3/fan1_input": "1200\n",
		// Mirrored into hwmon0 above; x86_pkg_temp is not mirrored anywhere.
		"thermal/thermal_zone0/type": "acpitz\n",
		"thermal/thermal_zone0/temp": "27800\n",
		"thermal/thermal_zone1/type": "x86_pkg_temp\n",
		"thermal/thermal_zone1/temp": "54000\n",
	})
	got := scanTemps(hwmon, thermal)
	// Numeric order (hwmon9 before hwmon10), so the two drives keep their
	// ordinals whatever order the directory was listed in.
	want := "acpitz/1/temp1 coretemp/1/temp1 coretemp/1/temp2 nvme/1/temp1 nvme/2/temp1 x86_pkg_temp/1/zone"
	if ids := tempIDs(got); ids != want {
		t.Fatalf("ids\n got %s\nwant %s", ids, want)
	}
	pkg := got[1]
	if pkg.Chip != "coretemp" || pkg.Label != "Package id 0" || pkg.TempC != 54 ||
		pkg.MaxC == nil || *pkg.MaxC != 80 || pkg.CritC == nil || *pkg.CritC != 100 {
		t.Errorf("package row: %+v", pkg)
	}
	if got[3].TempC != 38.85 || got[4].TempC != 41.85 {
		t.Errorf("drives out of order: %v %v", got[3].TempC, got[4].TempC)
	}
	if got[4].MaxC != nil {
		t.Errorf("an absurd limit was reported: %v", *got[4].MaxC)
	}
	if got[5].Label != "" || got[5].MaxC != nil {
		t.Errorf("zone row: %+v", got[5])
	}
}

// A zone gives way to its hwmon mirror — found under the name the kernel gives
// the mirror — and only to a mirror that was actually read.
func TestZonesAndTheirMirrors(t *testing.T) {
	hwmon, thermal := tempTree(t, map[string]string{
		"hwmon/hwmon0/name":          "cpu_thermal\n",
		"hwmon/hwmon0/temp1_input":   "48000\n",
		"hwmon/hwmon1/name":          "acpitz\n",
		"hwmon/hwmon1/temp1_input":   "0\n",
		"thermal/thermal_zone0/type": "cpu-thermal\n",
		"thermal/thermal_zone0/temp": "48000\n",
		"thermal/thermal_zone1/type": "acpitz\n",
		"thermal/thermal_zone1/temp": "90000\n",
	})
	got := scanTemps(hwmon, thermal)
	if ids := tempIDs(got); ids != "cpu_thermal/1/temp1 acpitz/1/zone" {
		t.Fatalf("got %s", ids)
	}
	if got[1].TempC != 90 {
		t.Fatalf("the zone behind an unreadable mirror read %v", got[1].TempC)
	}
}

func TestKnownTempLabel(t *testing.T) {
	for _, l := range []string{"Package id 0", "Core 17", "Composite", "Sensor 2", "Tctl",
		"Tccd3", "edge", "SYSTIN", "AUXTIN1", "PECI Agent 0", "CPU", " Composite\n"} {
		if knownTempLabel(l) != strings.TrimSpace(l) {
			t.Errorf("%q refused", l)
		}
	}
	// A label a driver read off the device rather than wrote itself.
	for _, l := range []string{"rack 12 asset 0042", "alice-laptop", "Core", "Core 12345",
		"Core x", "core 1", "17", "", "Composite 1", "CPU " + strings.Repeat("9", 40)} {
		if got := knownTempLabel(l); got != "" {
			t.Errorf("%q admitted as %q", l, got)
		}
	}
}

func TestScanTempsRefusesWhatIsNotAReading(t *testing.T) {
	files := map[string]string{
		"hwmon/hwmon0/name": "k10temp\n",
		// No name file: nothing to call the chip, so nothing reported.
		"hwmon/hwmon1/temp1_input": "40000\n",
		// Names that are not a chip directory or a channel.
		"hwmon/hwmonX/name":            "bogus\n",
		"hwmon/hwmonX/temp1_input":     "40000\n",
		"hwmon/hwmon0/temp_input":      "40000\n",
		"hwmon/hwmon0/temp99999_input": "40000\n",
		"hwmon/hwmon0/temp1_inputs":    "40000\n",
		"thermal/thermal_zone3/temp":   "45000\n",
	}
	for i, raw := range []string{"0", "-5000", "151000", "NaN", "Inf", "", "hot", "-273150"} {
		files["hwmon/hwmon0/temp"+strconv.Itoa(i+1)+"_input"] = raw + "\n"
	}
	hwmon, thermal := tempTree(t, files)
	got := scanTemps(hwmon, thermal)
	// Only the zone is left, and with no type file it still has a name.
	if len(got) != 1 || got[0].ID != "thermal/1/zone" || got[0].TempC != 45 {
		t.Fatalf("got %+v", got)
	}
	if out := scanTemps(filepath.Join(hwmon, "absent"), filepath.Join(thermal, "absent")); len(out) != 0 {
		t.Fatalf("a machine with no sysfs reported %+v", out)
	}
}

// The chip name is rendered on a page and written into a key. Whatever a
// driver — or something pretending to be sysfs — puts in it, what leaves is
// short and made of the characters a driver name is made of; a label that is
// not one a driver is known to write does not leave at all.
func TestTempNamesAreBounded(t *testing.T) {
	hwmon, thermal := tempTree(t, map[string]string{
		"hwmon/hwmon0/name":        "bad name/../\x1b[31m" + strings.Repeat("x", 80) + "\n",
		"hwmon/hwmon0/temp1_input": "40000\n",
		"hwmon/hwmon0/temp1_label": "  SSD\u202e \x07\u6e29\u5ea6 " + strings.Repeat("y", 80) + "\n",
	})
	got := scanTemps(hwmon, thermal)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	r := got[0]
	if len(r.Chip) > maxTempText {
		t.Errorf("unbounded chip: %d", len(r.Chip))
	}
	if !strings.HasPrefix(r.Chip, "bad_name_.._") || strings.ContainsAny(r.Chip, "/ \x1b[") {
		t.Errorf("chip %q", r.Chip)
	}
	if r.Label != "" {
		t.Errorf("label %q", r.Label)
	}
	if strings.Count(r.ID, "/") != 2 {
		t.Errorf("id %q", r.ID)
	}
}

func manyCores(n int, withPackage bool) []Temp {
	var list []Temp
	if withPackage {
		list = append(list, Temp{ID: "coretemp/1/temp1", Chip: "coretemp", Label: "Package id 0", TempC: 60})
	}
	for i := 0; i < n; i++ {
		list = append(list, Temp{
			ID: "coretemp/1/temp" + strconv.Itoa(i+2), Chip: "coretemp",
			Label: "Core " + strconv.Itoa(i), TempC: 50,
		})
	}
	return append(list, Temp{ID: "nvme/1/temp1", Chip: "nvme", Label: "Composite", TempC: 40})
}

func TestCapTemps(t *testing.T) {
	// Under the cap nothing is touched, cores included.
	if got := capTemps(manyCores(8, true)); len(got) != 10 {
		t.Fatalf("small machine: %d rows", len(got))
	}
	// Over it, the cores give way to the package that summarises them.
	if got := tempIDs(capTemps(manyCores(64, true))); got != "coretemp/1/temp1 nvme/1/temp1" {
		t.Fatalf("64 cores with a package: %s", got)
	}
	// A chip whose ONLY channels are cores has nothing standing in for them,
	// so they stay and the list is cut in scan order.
	got := capTemps(manyCores(64, false))
	if len(got) != maxTemps || got[0].ID != "coretemp/1/temp2" || got[maxTemps-1].ID != "coretemp/1/temp33" {
		t.Fatalf("64 cores alone: %d rows, %s … %s", len(got), got[0].ID, got[len(got)-1].ID)
	}
	for _, l := range []string{"Core", "Core ", "Core x", "Core 1a", "core 1", "Package id 0", ""} {
		if coreLabel(l) {
			t.Errorf("%q taken for a core", l)
		}
	}
}

func resetTempsCache() {
	tempsCache.Lock()
	tempsCache.at, tempsCache.started, tempsCache.all, tempsCache.done = 0, 0, nil, false
	tempsCache.Unlock()
}

func TestFillTemps(t *testing.T) {
	resetTempsCache()
	defer resetTempsCache()

	scans := 0
	scan := func() []Temp {
		scans++
		// The hottest row is one the cap drops: temp_c must still be it.
		list := manyCores(64, true)
		list[5].TempC = 97.5
		return list
	}
	var s System
	fillTemps(&s, tempsRefreshSec, scan)
	if s.TempC == nil || *s.TempC != 97.5 {
		t.Fatalf("temp_c %v, want the hottest of every sensor", s.TempC)
	}
	if len(s.Temps) != 2 {
		t.Fatalf("%d rows", len(s.Temps))
	}
	// Within the refresh window the sensors are not read again, and a caller
	// that edits its rows is not editing the next snapshot's.
	s.Temps[0].TempC = -1
	var again System
	fillTemps(&again, tempsRefreshSec, scan)
	if scans != 1 {
		t.Fatalf("scanned %d times inside the refresh window", scans)
	}
	if again.Temps[0].TempC != 60 {
		t.Fatalf("the cache was edited through a snapshot: %v", again.Temps[0].TempC)
	}

	blob, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"temp_c":97.5`,
		`{"id":"coretemp/1/temp1","chip":"coretemp","label":"Package id 0","temp_c":60}`,
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("wire form lacks %s in %s", want, blob)
		}
	}

	// No sensors: both fields absent, and the answer is cached like any other.
	resetTempsCache()
	scans = 0
	var bare System
	for i := 0; i < 2; i++ {
		fillTemps(&bare, tempsRefreshSec, func() []Temp { scans++; return nil })
	}
	if bare.TempC != nil || bare.Temps != nil || scans != 1 {
		t.Fatalf("sensorless machine: temp_c %v, rows %v, %d scans", bare.TempC, bare.Temps, scans)
	}
}
