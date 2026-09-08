package core

import (
	"os"
	"path/filepath"
	"testing"
)

// /proc/net/snmp and /proc/net/netstat interleave a header line of column names
// with a value line, and the columns differ between kernels, so values must be
// located by name rather than by position.
func TestReadProcNetTablePairsHeadersWithValues(t *testing.T) {
	content := `Tcp: RtoAlgorithm RtoMin ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab RetransSegs InErrs
Tcp: 1 200 5 6 7 8 9 10 11
Udp: InDatagrams NoPorts InErrors RcvbufErrors
Udp: 100 1 2 3
`
	path := filepath.Join(t.TempDir(), "snmp")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	table, err := readProcNetTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := table["Tcp"]["RetransSegs"]; got != 10 {
		t.Fatalf("Tcp.RetransSegs = %d, want 10", got)
	}
	if got := table["Tcp"]["CurrEstab"]; got != 9 {
		t.Fatalf("Tcp.CurrEstab = %d, want 9", got)
	}
	if got := table["Udp"]["RcvbufErrors"]; got != 3 {
		t.Fatalf("Udp.RcvbufErrors = %d, want 3", got)
	}
	if _, present := table["Tcp"]["ListenOverflows"]; present {
		t.Fatal("a column this kernel does not report must be absent, not zero")
	}
}

func TestReadProcNetTableRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snmp")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProcNetTable(path); err == nil {
		t.Fatal("expected an error for a file with no rows")
	}
}

func TestReadPressureFileTakesAvg10PerKind(t *testing.T) {
	content := `some avg10=12.34 avg60=5.00 avg300=1.00 total=99
full avg10=1.50 avg60=0.50 avg300=0.10 total=42
`
	path := filepath.Join(t.TempDir(), "memory")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	samples, err := readPressureFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if samples["some"] != 12.34 {
		t.Fatalf("some = %v, want 12.34", samples["some"])
	}
	if samples["full"] != 1.5 {
		t.Fatalf("full = %v, want 1.5", samples["full"])
	}
}

// A process name may contain spaces and brackets, so the state character has to
// be found after the last ')' rather than by splitting the line on whitespace.
func TestReadProcessStateHandlesNamesWithSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stat")
	if err := os.WriteFile(path, []byte("1234 (my app (worker)) Z 1 1234 1234 0 -1 4194560"), 0o644); err != nil {
		t.Fatal(err)
	}

	state, ok := readProcessState(path)
	if !ok {
		t.Fatal("expected the line to parse")
	}
	if state != 'Z' {
		t.Fatalf("state = %q, want Z", state)
	}
	if procStateNames[state] != "zombie" {
		t.Fatalf("state %q did not map to zombie", state)
	}
}

func TestReadProcessStateMissingFile(t *testing.T) {
	if _, ok := readProcessState(filepath.Join(t.TempDir(), "gone")); ok {
		t.Fatal("a process that exited between listing and reading must not be counted")
	}
}
