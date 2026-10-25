package main

import (
	"strings"
	"testing"
)

func TestSMCCelsius(t *testing.T) {
	for _, c := range []struct {
		typ  string
		data []byte
		want float64
		ok   bool
	}{
		{"sp78", []byte{0x27, 0xd0}, 39.81, true},
		{"sp78", []byte{0x2e, 0x00}, 46, true},
		// An empty core socket: -127.
		{"sp78", []byte{0x81, 0x00}, 0, false},
		{"sp78", []byte{0x00, 0x00}, 0, false},
		{"sp78", []byte{0x7f, 0xff}, 128, true},
		{"sp78", []byte{0x27}, 0, false},
		{"flt ", []byte{0x00, 0x00, 0x28, 0x42}, 42, true},
		{"flt ", []byte{0x00, 0x00, 0xc0, 0x7f}, 0, false}, // NaN
		{"flt ", []byte{0x00, 0x00, 0x7a, 0x44}, 0, false}, // 1000
		// Keys that begin with T and are not temperatures.
		{"ui16", []byte{0x27, 0xd0}, 0, false},
		{"flag", []byte{0x01}, 0, false},
	} {
		got, ok := smcCelsius(c.typ, c.data)
		if ok != c.ok || got != c.want {
			t.Errorf("%q %x: got %v %v, want %v %v", c.typ, c.data, got, ok, c.want, c.ok)
		}
	}
}

func TestSMCRows(t *testing.T) {
	rows := smcRows(map[string]float64{
		"TCXC": 49.22, "TC0P": 39.81, "TC2C": 49, "TA0V": 26.88, "TM0P": 38.38,
		// Not keys this file puts in an id.
		"T C0": 40, "TC0": 40, "FC0P": 40, "TC0P ": 40, "T#0P": 40,
	})
	if got := tempIDs(rows); got != "smc/1/TC0P smc/1/TC2C smc/1/TM0P smc/1/TA0V smc/1/TCXC" {
		t.Fatalf("got %s", got)
	}
	want := []string{"CPU proximity", "CPU core 2", "Memory proximity", "", ""}
	for i, r := range rows {
		if r.Label != want[i] || r.Chip != "smc" {
			t.Errorf("row %d: %+v, want label %q", i, r, want[i])
		}
		// Every word this file gives a key has to be one the wire admits.
		if r.Label != "" && admitTempLabel(r.Label) != r.Label {
			t.Errorf("label %q would be dropped on the way out", r.Label)
		}
	}
	for k, l := range smcLabels {
		if !smcTempKey(k) || admitTempLabel(l) != l {
			t.Errorf("table entry %q → %q is not admissible", k, l)
		}
	}
	if smcLabel("TC0C") != "" || smcLabel("TCaC") != "" {
		t.Error("a core label for a key that is not a numbered core")
	}
}

func TestHIDRows(t *testing.T) {
	all := []hidReading{
		{"gas gauge battery", 30.5, true}, {"PMU tdie1", 45.2, true}, {"NAND CH0 temp", 38, true},
		{"Alice's Thermometer", 22, true}, {"pACC MTR Temp Sensor3", 52, true},
		// Cannot be told apart by name: not reported.
		{"", 41, true}, {"", 42, true}, {"PMU tdev2", 39, true}, {"PMU tdev2", 40, true},
		// Gave no reading this time.
		{"PMU tdev12", 0, false},
	}
	rows := hidRows(all)
	byLabel := map[string]string{}
	var labels []string
	for _, r := range rows {
		if r.Chip != "hid" || tempIDRE.FindStringSubmatch(r.ID) == nil {
			t.Errorf("row %+v", r)
		}
		labels = append(labels, r.Label)
		byLabel[r.Label] = r.ID
	}
	// The CPU's first, then by name; the thermometer is reported, under a
	// number and no name.
	if got := strings.Join(labels, "|"); got != "pACC MTR Temp Sensor3||NAND CH0 temp|PMU tdie1|gas gauge battery" {
		t.Fatalf("labels %q", got)
	}
	if rows[1].TempC != 22 {
		t.Errorf("unlabelled row reads %v", rows[1].TempC)
	}

	// The same sensors in another order, one of them silent and one that was
	// silent now answering: every sensor that is still there keeps its id.
	again := hidRows([]hidReading{
		{"PMU tdev12", 39, true}, {"pACC MTR Temp Sensor3", 53, true}, {"PMU tdie1", 46, true},
		{"NAND CH0 temp", 0, false}, {"gas gauge battery", 31, true}, {"Alice's Thermometer", 22, true},
	})
	seen := 0
	for _, r := range again {
		if id, ok := byLabel[r.Label]; ok && r.Label != "" {
			seen++
			if id != r.ID {
				t.Errorf("%q moved from %s to %s", r.Label, id, r.ID)
			}
		}
	}
	if seen != 3 || len(again) != 5 {
		t.Errorf("%d rows, %d of them known from before", len(again), seen)
	}
	if again[1].ID != rows[1].ID {
		t.Errorf("the unlabelled sensor moved from %s to %s", rows[1].ID, again[1].ID)
	}
}

// An M1's 49 services against the snapshot's 32: the CPU clusters' readings
// survive the cut, which by name alone took every one of them.
func TestHIDRowsCPUSurviveTheCap(t *testing.T) {
	var all []hidReading
	for i := 1; i <= 8; i++ {
		for _, p := range []string{"PMU tdie", "PMU tdev", "PMU2 tdie", "PMU2 tdev"} {
			all = append(all, hidReading{p + itoa(i), 50, true})
		}
	}
	for _, n := range []string{"ANE MTR Temp Sensor1", "NAND CH0 temp", "PMGR SOC Die Temp Sensor0",
		"eACC MTR Temp Sensor0", "pACC MTR Temp Sensor2", "pACC MTR Temp Sensor9",
		"GPU MTR Temp Sensor1", "SOC MTR Temp Sensor0"} {
		all = append(all, hidReading{n, 60, true})
	}
	rows := capTemps(hidRows(all))
	if len(rows) != maxTemps {
		t.Fatalf("%d rows", len(rows))
	}
	var head []string
	for _, r := range rows[:5] {
		head = append(head, r.Label)
	}
	if got := strings.Join(head, "|"); got != "eACC MTR Temp Sensor0|pACC MTR Temp Sensor2|pACC MTR Temp Sensor9|GPU MTR Temp Sensor1|SOC MTR Temp Sensor0" {
		t.Errorf("first rows %q", got)
	}
}

func TestParseTempRows(t *testing.T) {
	rows := parseTempRows(` [
		{"id":"smc/1/TC0P","chip":"smc","label":"CPU proximity","temp_c":39.814},
		{"id":"hid/1/temp3","chip":"hid","label":"PMU tdie1","temp_c":45.2,"max_c":90,"crit_c":9000},
		{"id":"smc/1/TCXC","chip":"smc","label":"rack 12 asset 0042","temp_c":49.22},
		{"id":"smc/1/TC0P","chip":"smc","temp_c":99},
		{"id":"smc/1/TC0D","chip":"nvme","temp_c":40},
		{"id":"smc/0/TC0D","chip":"smc","temp_c":40},
		{"id":"smc/1/XC0D","chip":"smc","temp_c":40},
		{"id":"smc/1/TC0D/x","chip":"smc","temp_c":40},
		{"id":"smc/1/TC1C","chip":"smc","temp_c":-127},
		{"id":"smc/1/TC2C","chip":"smc","temp_c":151},
		{"id":"smc/1/TC3C","chip":"smc"},
		{"id":"smc/1/TC4C","chip":"smc","temp_c":40,"path":"/Users/alice","serial":"C02XK"}
	]
	`)
	if got := tempIDs(rows); got != "smc/1/TC0P hid/1/temp3 smc/1/TCXC smc/1/TC4C" {
		t.Fatalf("got %s", got)
	}
	if rows[0].TempC != 39.81 || rows[0].Label != "CPU proximity" {
		t.Errorf("first row %+v", rows[0])
	}
	if rows[1].MaxC == nil || *rows[1].MaxC != 90 || rows[1].CritC != nil {
		t.Errorf("limits %+v", rows[1])
	}
	if rows[2].Label != "" {
		t.Errorf("a label nobody here wrote survived: %q", rows[2].Label)
	}
	// Not a list: a crash banner, a truncated write, nothing, an object.
	for _, out := range []string{"", "panic: runtime error", `[{"id":"smc/1/TC0P","chip":"smc","temp_`,
		`{"id":"smc/1/TC0P"}`, "null", `"hot"`, `[1,2]`} {
		if got := parseTempRows(out); len(got) != 0 {
			t.Errorf("%q read as %+v", out, got)
		}
	}
	// More rows than any machine has: read up to the bound and no further.
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 2000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"hid/1/temp` + itoa(i%9999+1) + `","chip":"hid","temp_c":40}`)
	}
	b.WriteString("]")
	if got := parseTempRows(b.String()); len(got) != maxTempRowsIn {
		t.Errorf("%d rows admitted", len(got))
	}
}

func TestAppleGen(t *testing.T) {
	for brand, want := range map[string]int{
		"Apple M1": 1, "Apple M2 Pro": 2, "Apple M3 Max": 3, "Apple M4": 4, "Apple M12 Ultra": 12,
		"Intel(R) Core(TM) i7-8850H CPU @ 2.60GHz": 0, "Apple A18 Pro": 0, "": 0, "Apple Mx": 0,
	} {
		if got := appleGen(brand); got != want {
			t.Errorf("appleGen(%q) = %d, want %d", brand, got, want)
		}
	}
}

// An M2 Pro's SMC, 2026-10-25, a few of its 195 keys: the table's cores and
// GPU clusters come out named and in its order, the rest stay behind.
func TestAppleSMCRowsM2(t *testing.T) {
	read := map[string]float64{
		"Tp00": 56.74, "Tp01": 63.44, "Tp02": 76.22, "Tp05": 61.07, "Tp09": 63.33, "Tp0D": 60.71,
		"Tp0X": 61.67, "Tp0b": 59.55, "Tp0f": 60.72, "Tp0j": 59.14,
		"Tp1h": 66.11, "Tp1t": 59.75, "Tp1p": 61.9, "Tp1l": 62.17,
		"Tg0f": 58.28, "Tg0j": 59.26, "Te05": 59.93, "Tf06": 83.13, "TA0P": 44.88,
	}
	rows := appleSMCRows(2, read)
	var got []string
	for _, r := range rows {
		got = append(got, strings.TrimPrefix(r.ID, "smc/1/")+"="+r.Label)
		if r.Chip != "smc" || tempIDRE.FindStringSubmatch(r.ID) == nil || admitTempLabel(r.Label) != r.Label {
			t.Errorf("row %+v", r)
		}
	}
	want := "Tp01=CPU performance core 1|Tp05=CPU performance core 2|Tp09=CPU performance core 3|" +
		"Tp0D=CPU performance core 4|Tp0X=CPU performance core 5|Tp0b=CPU performance core 6|" +
		"Tp0f=CPU performance core 7|Tp0j=CPU performance core 8|" +
		"Tp1h=CPU efficiency core 1|Tp1t=CPU efficiency core 2|Tp1p=CPU efficiency core 3|Tp1l=CPU efficiency core 4|" +
		"Tg0f=GPU 1|Tg0j=GPU 2"
	if s := strings.Join(got, "|"); s != want {
		t.Errorf("rows\n %s\nwant\n %s", s, want)
	}

	// A key the machine lacks is skipped and the count closes up: an M4 Pro's
	// GPU keys are not the plain M4's.
	m4 := appleSMCRows(4, map[string]float64{"Tp01": 50, "Te05": 45, "Tg1U": 40, "Tg1k": 41})
	if len(m4) != 4 || m4[2].Label != "GPU 1" || m4[3].Label != "GPU 2" {
		t.Errorf("M4 Pro rows %+v", m4)
	}

	// A plain M3: its efficiency cores under the table's keys, its
	// performance cores under Tp ones. The table does not describe it, so
	// three efficiency cores are not reported as the processor.
	m3 := appleSMCRows(3, map[string]float64{"Te05": 50, "Te0L": 51, "Te0P": 52, "Tp01": 60, "Tp05": 61, "Tg0f": 55})
	var m3keys []string
	for _, r := range m3 {
		m3keys = append(m3keys, strings.TrimPrefix(r.ID, "smc/1/")+"="+r.Label)
	}
	if s := strings.Join(m3keys, " "); s != "Te05= Te0L= Te0P= Tp01= Tp05=" {
		t.Errorf("plain M3 rows %s", s)
	}

	// A chip the table does not know: every Tp and Te key, by key, unnamed.
	unknown := appleSMCRows(9, read)
	var keys []string
	for _, r := range unknown {
		keys = append(keys, strings.TrimPrefix(r.ID, "smc/1/"))
		if r.Label != "" {
			t.Errorf("unknown chip row named %q", r.Label)
		}
	}
	if s := strings.Join(keys, " "); s != "Te05 Tp00 Tp01 Tp02 Tp05 Tp09 Tp0D Tp0X Tp0b Tp0f Tp0j Tp1h Tp1l Tp1p Tp1t" {
		t.Errorf("unknown chip keys %s", s)
	}
}

// Every name the table can give is one the helper (and the relay) admits.
func TestAppleSMCLabelsAdmitted(t *testing.T) {
	for gen, set := range appleSMCKeys {
		read := map[string]float64{}
		for _, ks := range []string{set.super, set.perf, set.eff, set.gpu} {
			for _, k := range strings.Fields(ks) {
				if !smcTempKey(k) {
					t.Errorf("M%d: %q is not a temperature key", gen, k)
				}
				read[k] = 50
			}
		}
		for _, r := range appleSMCRows(gen, read) {
			if r.Label == "" || admitTempLabel(r.Label) != r.Label {
				t.Errorf("M%d: %s labelled %q", gen, r.ID, r.Label)
			}
		}
	}
}

func TestAppleRows(t *testing.T) {
	asked := 0
	smc := func() map[string]float64 { asked++; return map[string]float64{"Tp01": 63, "Tp1h": 60, "Tg0f": 58} }
	m1 := []Temp{{ID: "hid/1/temp1", Chip: "hid", Label: "pACC MTR Temp Sensor2", TempC: 68}}
	if rows := appleRows(m1, 1, smc); len(rows) != 1 || asked != 0 {
		t.Errorf("an M1's HID carries the CPU: %d rows, SMC asked %d times", len(rows), asked)
	}
	m2 := []Temp{{ID: "hid/1/temp3690", Chip: "hid", Label: "NAND CH0 temp", TempC: 40}}
	rows := appleRows(m2, 2, smc)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r.Label)
	}
	if s := strings.Join(labels, "|"); s != "CPU performance core 1|CPU efficiency core 1|GPU 1|NAND CH0 temp" || asked != 1 {
		t.Errorf("an M2: %s, SMC asked %d times", s, asked)
	}
}
