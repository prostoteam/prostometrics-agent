package core

import "testing"

// systemd raises fs.file-max to the largest value a signed 64-bit integer holds,
// which is the third field of /proc/sys/fs/file-nr on a current host. Reporting
// it costs the whole series: the client refuses the sample, so the metric loses
// its max line and the agent logs a drop every collection.
func TestIsRealFileMaxRejectsTheAbsentLimit(t *testing.T) {
	cases := []struct {
		name string
		max  uint64
		want bool
	}{
		{"systemd raises the limit out of the way", 9223372036854775807, false},
		{"kernel default on a small host", 9223372, true},
		{"kernel default on a host with a terabyte", 107000000, true},
		{"an administrator's round number", 2097152, true},
		{"nothing read", 0, false},
	}
	for _, c := range cases {
		if got := isRealFileMax(c.max); got != c.want {
			t.Errorf("%s: isRealFileMax(%d) = %v, want %v", c.name, c.max, got, c.want)
		}
	}
}
