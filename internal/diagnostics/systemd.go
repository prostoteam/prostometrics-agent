package diagnostics

// Branches stay local to the same bounded, read-only collection task. Command
// output is data: it is never evaluated as shell code or used as a command name.
const systemdScript = `
network_wait_diagnostics() {
 section 'Network wait configuration and current network managers'
 run systemctl show systemd-networkd.service NetworkManager.service networking.service --property=Id,ActiveState,SubState,UnitFileState
 section 'Current network state (not a reconstruction of an earlier timeout)'
 run ip -brief address
 run ip route show default
 links=$(timeout 3s networkctl --no-pager --no-legend list 2>&1)
 code=$?
 printf '\n$ networkctl --no-pager --no-legend list\n%s\n' "$links"
 [ "$code" -eq 0 ] || { printf '[check exit code: %s]\n' "$code"; return; }
 # Managed links first, then physical unmanaged links; avoid flooding the report
 # with Docker's virtual interfaces. At most five interfaces are investigated.
 checked=0
 while IFS= read -r interface; do
  [ "$checked" -lt 5 ] || break
  case "$interface" in ''|-*|*[!a-zA-Z0-9_.:@-]*) continue;; esac
  checked=$((checked + 1))
  section "Network interface $interface"
  state=$(timeout 3s networkctl --no-pager --lines=0 status "$interface" 2>&1)
  code=$?
  printf '\n$ networkctl --no-pager --lines=0 status %s\n%s\n' "$interface" "$state"
  [ "$code" -eq 0 ] || printf '[check exit code: %s]\n' "$code"
  config=$(printf '%s\n' "$state" | sed -n 's/^[[:space:]]*Network File: //p' | head -1)
  case "$config" in
   /etc/systemd/network/*.network|/run/systemd/network/*.network|/usr/lib/systemd/network/*.network)
    case "$config" in *'/../'*) continue;; esac
    section 'Network matching and online requirements (base file; drop-ins may override)'
    # Do not dump whole configuration files, environment or authentication data.
    run awk '/^\[/ {section=$0} /^[[:space:]]*(Name|Type|Driver|RequiredForOnline|RequiredFamilyForOnline|ActivationPolicy|DHCP|IPv6AcceptRA|LinkLocalAddressing)[[:space:]]*=/ {print section; print}' "$config"
    ;;
  esac
 done < <(printf '%s\n' "$links" | awk 'NF >= 5 && $1 ~ /^[0-9]+$/ && $5 != "unmanaged" {print $2}'; printf '%s\n' "$links" | awk 'NF >= 5 && $1 ~ /^[0-9]+$/ && $5 == "unmanaged" && $3 == "ether" && $2 !~ /^veth/ {print $2}')
 section 'Recent network configuration journal (may not cover the original failure)'
 run journalctl --unit=systemd-networkd.service --since=-15min --lines=30 --no-pager --output=short-iso
}

permission_diagnostics() {
 # Inspect metadata only for bounded absolute paths actually named by an access
 # error. Restrict syntax rather than evaluating quoted text from a journal.
 while IFS= read -r denied_path; do
  case "$denied_path" in *'/../'*|*/..) continue;; esac
  section 'Access error path metadata (historical error; not proof of current failure)'
  run namei -l -- "$denied_path"
  run stat --format='path=%n owner=%U group=%G mode=%a type=%F' -- "$denied_path"
  parent="$denied_path"
  for ((depth=0; depth<10; depth++)); do
   [ -e "$parent" ] && break
   parent=$(dirname -- "$parent")
  done
  run df -h -- "$parent"
  run df -i -- "$parent"
 done < <(printf '%s\n' "$1" | sed -nE 's/.*: (\/[a-zA-Z0-9_.\/-]+): Permission denied.*/\1/p' | awk '!seen[$0]++' | head -2)
}

section 'Failed services'
run systemctl --failed --type=service --no-pager --plain
need_network_wait=''
while IFS= read -r unit; do
 case "$unit" in -*|*[!a-zA-Z0-9_.@:\\-]*) continue;; esac
 section "$unit"
 run systemctl show "$unit" --property=Id,Description,LoadState,ActiveState,SubState,Result,ExecMainCode,ExecMainStatus,ExecMainStartTimestamp,ExecMainExitTimestamp,ActiveEnterTimestamp,InactiveEnterTimestamp,StateChangeTimestamp,Transient,RuntimeMaxUSec,Restart,Type,User,Group,DynamicUser,FragmentPath,UnitFileState
 journal=$(timeout 3s journalctl --unit="$unit" --lines=30 --no-pager --output=short-iso --all 2>&1)
 code=$?
 printf '\n$ journalctl --unit=%s --lines=30\n%s\n' "$unit" "$journal"
 [ "$code" -eq 0 ] || printf '[check exit code: %s]\n' "$code"
 permission_diagnostics "$journal"
 case "$unit" in systemd-networkd-wait-online.service|systemd-networkd-wait-online@*.service)
  run systemctl show "$unit" --property=ExecStart,TimeoutStartUSec,After,Before,Wants,Requires
  need_network_wait=1;; esac
done < <(timeout 3s systemctl list-units --failed --type=service --no-legend --plain --no-pager | awk '{print $1}' | head -10)
[ -z "$need_network_wait" ] || network_wait_diagnostics
true
`
