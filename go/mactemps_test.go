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
	// By name; the thermometer is reported, under a number and no name.
	if got := strings.Join(labels, "|"); got != "|NAND CH0 temp|PMU tdie1|gas gauge battery|pACC MTR Temp Sensor3" {
		t.Fatalf("labels %q", got)
	}
	if rows[0].TempC != 22 {
		t.Errorf("unlabelled row reads %v", rows[0].TempC)
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
	if again[0].ID != rows[0].ID {
		t.Errorf("the unlabelled sensor moved from %s to %s", rows[0].ID, again[0].ID)
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
