package diagnostics

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runSystemdFixture(t *testing.T, accessError, networkFailure bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	sentinel := filepath.Join(dir, "must-not-exist")
	mock := `#!/bin/bash
command=${0##*/}
printf '%s' "$command" >> "$PM_DIAG_TEST_LOG"
printf ' <%s>' "$@" >> "$PM_DIAG_TEST_LOG"
printf '\n' >> "$PM_DIAG_TEST_LOG"
case "$command" in
 timeout) while [[ "$1" == --* ]]; do shift; done; shift; exec "$@";;
 systemctl)
  case "$1" in
   list-units)
    printf '%s\n' 'capture.service loaded failed failed' 'bad;unit.service loaded failed failed'
    [ "$PM_DIAG_TEST_NO_NETWORK_WAIT" = 1 ] || printf '%s\n' 'systemd-networkd-wait-online.service loaded failed failed'
    ;;
   show) printf '%s\n' 'RuntimeMaxUSec=5h' 'ExecMainStatus=0' 'Transient=yes' 'User=capture';;
  esac;;
 journalctl)
  if [ "$PM_DIAG_TEST_ACCESS" = 1 ]; then
   printf '%s\n' 'host tcpdump: /var/lib/example/capture.pcap: Permission denied'
   printf '%s\n' 'host tcpdump: /tmp/../secret: Permission denied'
  fi
  printf '%s\n' 'untrusted $(touch '"$PM_DIAG_TEST_SENTINEL"')'
  ;;
 networkctl)
  [ "$PM_DIAG_TEST_NETWORK_FAILURE" != 1 ] || exit 127
  case "$*" in
   *list*) printf '%s\n' '1 lo loopback carrier unmanaged' '2 eth0 ether routable unmanaged' '3 veth123 ether enslaved unmanaged' '4 private0 ether configuring configuring';;
   *) printf '%s\n' 'Network File: /tmp/do-not-read' 'State: routable (unmanaged)';;
  esac;;
 ip) printf '%s\n' 'default via 192.0.2.1 dev eth0';;
 namei|stat|df) printf '%s\n' 'read-only metadata';;
esac
`
	for _, name := range []string{"timeout", "systemctl", "journalctl", "networkctl", "ip", "namei", "stat", "df"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(mock), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PM_DIAG_TEST_LOG", log)
	t.Setenv("PM_DIAG_TEST_SENTINEL", sentinel)
	if accessError {
		t.Setenv("PM_DIAG_TEST_ACCESS", "1")
	}
	if networkFailure {
		t.Setenv("PM_DIAG_TEST_NETWORK_FAILURE", "1")
	}
	cmd := exec.Command("bash", "-c", scriptPreamble+systemdScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("journal executed as shell")
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(calls)
}

func TestSystemdInvestigatesAccessErrorsAndNetworkWait(t *testing.T) {
	out, calls := runSystemdFixture(t, true, false)
	for _, want := range []string{"RuntimeMaxUSec=5h", "Transient=yes", "User=capture", "historical error", "not a reconstruction", "routable (unmanaged)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing evidence %q", want)
		}
	}
	for _, want := range []string{"namei <-l> <--> </var/lib/example/capture.pcap>", "df <-i>", "TimeoutStartUSec", "NetworkManager.service", "status> <private0>", "status> <eth0>"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing check %q", want)
		}
	}
	for _, unwanted := range []string{"<bad;unit.service>", "status> <veth123>", "</tmp/../secret>", "</tmp/do-not-read>", "<restart>", "<reset-failed>", "<Environment>"} {
		if strings.Contains(calls, unwanted) {
			t.Errorf("unexpected command argument %q", unwanted)
		}
	}
	if strings.Index(calls, "status> <private0>") > strings.Index(calls, "status> <eth0>") {
		t.Error("managed interfaces should be investigated first")
	}
}

func TestSystemdSkipsPathChecksWithoutAccessError(t *testing.T) {
	_, calls := runSystemdFixture(t, false, false)
	if strings.Contains(calls, "namei ") || strings.Contains(calls, "stat ") || strings.Contains(calls, "df ") {
		t.Fatal("unrelated filesystem checks ran")
	}
}

func TestSystemdKeepsEvidenceWhenNetworkToolFails(t *testing.T) {
	out, calls := runSystemdFixture(t, false, true)
	if !strings.Contains(out, "RuntimeMaxUSec=5h") || !strings.Contains(out, "[check exit code: 127]") {
		t.Fatal("tool failure hid evidence or its limitation")
	}
	if strings.Contains(calls, "networkctl <--no-pager> <--lines=0>") {
		t.Fatal("failed listing used as an interface name")
	}
}

func TestSystemdDoesNotInvestigateNetworkForUnrelatedService(t *testing.T) {
	t.Setenv("PM_DIAG_TEST_NO_NETWORK_WAIT", "1")
	_, calls := runSystemdFixture(t, false, false)
	if strings.Contains(calls, "networkctl ") || strings.Contains(calls, "ip ") || strings.Contains(calls, "TimeoutStartUSec") {
		t.Fatal("network checks ran without a network wait failure")
	}
}
