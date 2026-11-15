package diagnostics

// Keep timeout commands (including those in legacy downloaded scripts) in the
// runner's process group. A command ignoring TERM is killed after one second.
const timeoutPreamble = `timeout() { command timeout --foreground --kill-after=1s "$@"; }
export -f timeout
`

const scriptPreamble = timeoutPreamble + `set -o pipefail
section() { printf '\n--- %s ---\n' "$1"; }
run() { printf '\n$'; printf ' %q' "$@"; printf '\n'; timeout 3s "$@" 2>&1; result=$?; if [ "$result" -ne 0 ]; then printf '[check exit code: %s]\n' "$result"; fi; return 0; }
section 'Collection time (UTC)'
date -u --iso-8601=seconds
`

var scripts = map[string]string{
	"systemd": systemdScript,
	"containers": `section 'Container states'
run docker ps -a --format '{{.ID}} {{.Names}} {{.Status}}'
# Match the metric collector: completed jobs and Compose one-offs are not down
# services. Inspect unhealthy services before other failures and healthy ones.
rows=$(timeout 3s docker ps -a --no-trunc --format '{{.ID}}\t{{.State}}\t{{.Status}}\t{{.Label "com.docker.compose.oneoff"}}' 2>&1)
code=$?
if [ "$code" -ne 0 ]; then
 printf '[container selection exit code: %s]\n%s\n' "$code" "$rows"
 exit 0
fi
for id in $(printf '%s\n' "$rows" | awk -F '\t' '
 tolower($4) == "true" || $3 ~ /^Exited \(0\)/ {next}
 {priority=3; if ($2 != "running") priority=2; if ($3 ~ /\(unhealthy\)/) priority=1; print priority "\t" $1}
' | sort -n -k1,1 | head -10 | cut -f2); do
 case "$id" in *[!a-f0-9]*) continue;; esac
 section "$id"
 run docker inspect --format '{{.Name}} state={{json .State}} restarts={{.RestartCount}} memoryLimit={{.HostConfig.Memory}}' "$id"
 run docker logs --tail=15 --timestamps "$id"
done
true
`,
	"memory": `section 'Memory and swap'
run free -k
run cat /proc/pressure/memory
section 'Largest processes (no command arguments)'
ps -eo pid,comm,rss,pmem --sort=-rss | head -16
section 'Container memory'
run docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.MemPerc}}'
section 'Recent kernel memory events'
run journalctl -k --since=-15min --grep='Out of memory|Killed process|oom-kill' --lines=20 --no-pager --output=short-iso
true
`,
	"cpu": `section 'Load'
run uptime
run cat /proc/pressure/cpu
run cat /proc/pressure/io
section 'Processes by CPU (no command arguments)'
ps -eo pid,comm,pcpu,pmem --sort=-pcpu | head -16
section 'Container CPU'
run docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}'
true
`,
	"disk": `section 'Filesystem space'
run df -hT
section 'Filesystem inodes'
run df -i
section 'Disk pressure'
run cat /proc/pressure/io
run iostat -x 1 2
section 'Recent kernel disk errors'
run journalctl -k --since=-15min --grep='I/O error|filesystem error|EXT4-fs error|nvme.*error' --lines=20 --no-pager --output=short-iso
true
`,
	"network": `section 'Interface counters'
run ip -s link
section 'Routes'
run ip route
section 'Socket summary'
run ss -s
true
`,
	"nginx": `section 'Nginx service'
run systemctl show nginx --property=ActiveState,SubState,Result,ExecMainStatus,StateChangeTimestamp
run journalctl --unit=nginx --lines=30 --no-pager --output=short-iso
section 'Recent nginx error log'
run tail -n 30 /var/log/nginx/error.log
section 'Listening sockets'
run ss -ltn
true
`,
	"databases": `section 'Database-related service states'
systemctl list-units --type=service --all --no-pager --plain | grep -Ei 'mongo|postgres|mysql|mariadb|redis|rabbitmq' | head -20
section 'Container states'
run docker ps -a --format '{{.ID}} {{.Names}} {{.Status}}'
section 'Memory'
run free -k
section 'Disk'
run df -h
section 'Recent database service journal'
run journalctl --unit=mongod --unit=postgresql --unit=mysql --unit=mariadb --unit=redis-server --unit=rabbitmq-server --lines=30 --no-pager --output=short-iso
true
`,
}
