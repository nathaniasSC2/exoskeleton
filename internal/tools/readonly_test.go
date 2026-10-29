package tools

import "testing"

func TestReadOnlyCommand(t *testing.T) {
	ok := []string{
		"systemctl is-active exo", "systemctl status llama-server --no-pager", "journalctl -u exo -n 50 --no-pager",
		"docker ps --format '{{.Names}}'", "df -h /var", "nvidia-smi", "free -m", "sudo ss -tlnp | grep 8656",
		"curl -s http://127.0.0.1:8656/healthz", "ls -la /var/lib/exo", "tail -n 20 /var/log/syslog",
		"sqlite3 -readonly /var/lib/exo/exo.db 'select count(*) from turns'", "top -bn1 | head -20", "systemctl --failed", "ip -br addr", "ls -lt /mnt/media/videos/ 2>/dev/null | head -15", "find /mnt/media -name x -type f -printf %p 2>/dev/null | sort -rn", "nvidia-smi --query-gpu=memory.used --format=csv", "hostname", "sort -n x | uniq -c",
	}
	bad := []string{
		"systemctl restart exo", "docker stop jellyfin", "rm -rf /tmp/x", "echo hi > /tmp/x", "ls; reboot",
		"cat x && rm y", "curl -X POST http://x", "curl -d a=b http://x", "find / -delete", "find . -exec rm {} +",
		"journalctl --vacuum-time=1d", "tail -f /var/log/syslog", "echo $(whoami)", "sudo reboot", "bash -c ls",
		"sqlite3 /var/lib/exo/exo.db 'delete from turns'", "tailscale serve --bg 8080", "git push", "docker",
		"sed -i s/a/b/ f", "ls 2>/dev/null > /tmp/x", "cat a >/dev/null; rm b", "ip link set eno1 down", "nvidia-smi -pl 100", "sort -o /etc/x y", "ss -K dst 1.2.3.4",
		"sqlite3 -readonly x.db '.shell rm x'", "hostname evil", "uniq a b", "git branch x", "find / -fprint /etc/x",
		"sudo date -s 2020-01-01", "awk 'BEGIN{system(\"rm x\")}'", "systemctl stop exo",
	}
	// commands the local model really wrote in shadow runs (2026-10-11)
	ok = append(ok,
		`docker inspect browser --format '{{json .HostConfig.Memory}}' 2>/dev/null || docker inspect browser`,
		`grep -ri 'commercial\|channel:manage:broadcast\|broadcaster' /opt/apps/streamer/ 2>/dev/null | head -20`,
		`ps aux --sort=-%cpu | head -10; echo "---"; ps aux | grep obs | grep -v grep`,
		`systemctl list-units --all '*media*' 2>/dev/null; docker ps -a --filter 'name=media' 2>/dev/null; docker images`,
		`docker ps --format '{{.Names}} {{.Ports}}' | grep 8795`,
		`journalctl -u streamer --no-pager -n 50 && echo ok`,
	)
	bad = append(bad, "ls && rm -rf /", "cat x | sh", "echo hi > /etc/x", "sleep 100 &", `echo "$(rm x)"`, "echo `id`",
		"ls ;", "; ls", "ls || ", `echo 'unterminated`, "find . -execdir rm {} +", "ls\nrm x", "env rm x")
	for _, c := range ok {
		if !ReadOnlyCommand(c) {
			t.Errorf("should allow %q", c)
		}
	}
	for _, c := range bad {
		if ReadOnlyCommand(c) {
			t.Errorf("should refuse %q", c)
		}
	}
}
