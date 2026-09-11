package core

import (
	"os"
	"path/filepath"
	"testing"
)

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
