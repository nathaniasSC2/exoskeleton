package tools

import "strings"

// ReadOnlyCommand reports whether a shell command only looks at the server.
// The local brain (and shadow runs) may run these; anything else is Claude's
// or the admin's. The command is split quote-aware into simple commands (joined by
// |, ;, && or ||) and every one must be on the allowlist. Redirection (other
// than discarding output), command substitution and backgrounding are refused.
func ReadOnlyCommand(cmd string) bool {
	// Discarding output is harmless and the model writes it constantly.
	for _, quiet := range []string{"&>/dev/null", "2>/dev/null", "2>&1", ">/dev/null"} {
		cmd = strings.ReplaceAll(cmd, quiet, " ")
	}
	stages, ok := splitShell(cmd)
	if !ok || len(stages) == 0 {
		return false
	}
	for _, words := range stages {
		if !readOnlyStage(words) {
			return false
		}
	}
	return true
}

// splitShell splits a command line into simple commands (lists of unquoted
// words). It returns false for anything it won't vet: redirection, `...`,
// $(...), a lone & (background), unbalanced quotes, or a newline.
func splitShell(cmd string) ([][]string, bool) {
	var stages [][]string
	var words []string
	var word strings.Builder
	inWord, quote := false, byte(0)
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endStage := func() bool {
		endWord()
		if len(words) == 0 {
			return false // empty command between separators
		}
		stages = append(stages, words)
		words = nil
		return true
	}
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		switch {
		case quote == '\'':
			if ch == '\'' {
				quote = 0
			} else {
				word.WriteByte(ch)
			}
		case quote == '"':
			switch {
			case ch == '"':
				quote = 0
			case ch == '`' || (ch == '$' && i+1 < len(cmd) && cmd[i+1] == '('):
				return nil, false
			default:
				word.WriteByte(ch)
			}
		case ch == '\'' || ch == '"':
			quote, inWord = ch, true
		case ch == '`' || ch == '<' || ch == '>' || ch == '\n' || (ch == '$' && i+1 < len(cmd) && cmd[i+1] == '('):
			return nil, false
		case ch == '|' || ch == ';' || ch == '&':
			if ch == '&' && (i+1 >= len(cmd) || cmd[i+1] != '&') {
				return nil, false // background
			}
			if (ch == '|' || ch == '&') && i+1 < len(cmd) && cmd[i+1] == ch {
				i++ // || or &&
			}
			if !endStage() {
				return nil, false
			}
		case ch == ' ' || ch == '\t':
			endWord()
		case ch == '\\' && i+1 < len(cmd):
			i++
			word.WriteByte(cmd[i])
			inWord = true
		default:
			word.WriteByte(ch)
			inWord = true
		}
	}
	if quote != 0 || !endStage() {
		return nil, false
	}
	return stages, true
}

var readOnlyBins = map[string]bool{
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "wc": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "column": true, "ls": true, "stat": true, "file": true, "du": true, "df": true,
	"free": true, "uptime": true, "date": true, "hostname": true, "uname": true, "whoami": true, "id": true,
	"which": true, "ps": true, "pgrep": true, "nvidia-smi": true, "ss": true, "ip": true, "lsblk": true,
	"findmnt": true, "sensors": true, "journalctl": true, "ping": true, "dig": true, "nproc": true, "lscpu": true,
	"sqlite3": true, "jq": true, "echo": true, "printf": true, "true": true, "basename": true, "dirname": true,
	"realpath": true, "readlink": true, "env": false,
}

// arguments that turn an otherwise read-only tool into a writer
var deniedArgs = map[string][]string{
	"ip":         {"set", "add", "del", "delete", "flush", "change", "replace", "append"},
	"nvidia-smi": {"-pl", "--power-limit", "-pm", "--persistence-mode", "-r", "--gpu-reset", "-ac", "-rac", "-lgc", "-rgc", "-lmc", "-rmc", "-e", "--ecc-config", "-c", "--compute-mode", "-am", "-acp", "mig"},
	"sort":       {"-o", "--output"},
	"ss":         {"-K", "--kill"},
	"date":       {"-s", "--set"},
	"sensors":    {"-s"},
	"ping":       {"-f"},
	"find":       {"-fprint", "-fprint0", "-fprintf", "-fls"},
}

// subcommand allowlists for tools that can also change things
var readOnlySubs = map[string]map[string]bool{
	"systemctl": {"status": true, "is-active": true, "is-enabled": true, "is-failed": true, "list-units": true,
		"list-timers": true, "list-unit-files": true, "show": true, "cat": true, "--failed": true},
	"docker":    {"ps": true, "logs": true, "inspect": true, "images": true, "stats": true, "top": true, "port": true},
	"tailscale": {"status": true, "ip": true},
	"git":       {"status": true, "log": true, "diff": true, "show": true},
	"top":       {"-bn1": true, "-b": true},
}

func readOnlyStage(f []string) bool {
	if len(f) > 0 && f[0] == "sudo" {
		f = f[1:]
	}
	if len(f) == 0 {
		return false
	}
	bin := f[0]
	if i := strings.LastIndex(bin, "/"); i >= 0 {
		bin = bin[i+1:]
	}
	for _, a := range f[1:] {
		switch {
		case a == "-f" && (bin == "tail" || bin == "journalctl"), // would hang
			strings.HasPrefix(a, "--vacuum"), a == "--rotate", a == "-delete", a == "-exec", a == "-execdir", a == "-ok":
			return false
		}
		for _, d := range deniedArgs[bin] {
			if a == d || strings.HasPrefix(a, d+"=") {
				return false
			}
		}
	}
	switch bin {
	case "hostname":
		return len(f) == 1 || strings.HasPrefix(f[1], "-")
	case "uniq": // a second file argument is an output file
		n := 0
		for _, a := range f[1:] {
			if !strings.HasPrefix(a, "-") {
				n++
			}
		}
		return n <= 1
	case "curl":
		for _, a := range f[1:] {
			if a == "-X" || a == "-d" || a == "-F" || a == "-T" || a == "-o" || a == "-O" || strings.HasPrefix(a, "--data") ||
				strings.HasPrefix(a, "--request") || strings.HasPrefix(a, "--upload") || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--form") {
				return false
			}
		}
		return true
	case "find":
		return true // -delete and -exec are refused above
	case "sqlite3":
		// read-only opens only, and no dot-commands (.shell, .output, ...)
		ro := false
		for _, a := range f[1:] {
			if strings.HasPrefix(a, ".") {
				return false
			}
			ro = ro || a == "-readonly"
		}
		return ro
	}
	if readOnlyBins[bin] {
		return true
	}
	if subs, ok := readOnlySubs[bin]; ok {
		for _, a := range f[1:] {
			if strings.HasPrefix(a, "-") && !subs[a] {
				continue // flags before the subcommand
			}
			return subs[a]
		}
		return bin == "systemctl" || bin == "top" // bare: lists units / one snapshot
	}
	return false
}
