package trace

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tools are the commands a builder script runs that are worth showing. What is
// left over is shell plumbing: variables, tests, loops, redirections.
var tools = []string{"oc", "openshift-install", "buildah", "skopeo", "ssh", "git", "curl", "find", "df", "dnf", "podman"}

// fileTools stand in when a script runs none of tools, so that a cleanup script
// still says what it removed.
var fileTools = []string{"rm", "mkdir", "cp", "mv", "ln", "chmod", "chown", "touch"}

// maxLine is how much of one command is shown. The start of a command says what
// it does; the tail is usually long digests and image references. It is wide
// enough to keep the subcommand after a long --kubeconfig preamble.
const maxLine = 200

// ScriptCommands returns the commands a builder script runs, with continuations
// joined and the positional arguments and simple variables filled in. It is a
// reading aid, not a shell: anything it cannot resolve stays as written.
//
// A script that dispatches on an argument, such as registry.sh's "case $op in",
// reports only the branch that argument selects: a `seed list` must not read as
// though it were deleting. A case whose branches are written on one line is not
// recognized, and reports nothing from that block.
func ScriptCommands(script string, args []string) []string {
	lines := logicalLines(script)
	arrs, defs := arrays(lines)
	cmds, files := extract(lines, defs, arrs, args)
	if len(cmds) > 0 {
		return dedup(cmds)
	}
	// A script that runs no tool still gets to say which files it touches.
	return dedup(files)
}

// extract walks a script once, collecting the tool commands it runs and, in
// case it runs none, the file commands it uses.
func extract(lines []string, defs map[string]bool, arrs map[string]string, args []string) (cmds, files []string) {
	w := &walk{args: args, arrs: arrs, vars: map[string]string{}}
	for _, line := range lines {
		if defs[line] || w.step(line) {
			continue // the array's uses say more than its definition
		}
		live := w.live()
		for _, stmt := range statements(line) {
			if n, ok := shift(stmt); ok {
				w.pos += n
				continue
			}
			if assign(stmt, args, w.pos, w.vars) {
				continue
			}
			if !live {
				continue
			}
			s := w.fill(stmt)
			if cmd, ok := toolLine(s, tools); ok {
				cmds = append(cmds, cmd)
				continue
			}
			if len(files) < 5 {
				if cmd, ok := toolLine(s, fileTools); ok {
					files = append(files, cmd)
				}
			}
		}
	}
	return cmds, files
}

// walk is what a script walk has resolved so far: the arguments still in play,
// the arrays and variables it has read, and the case branches it is inside.
type walk struct {
	args  []string
	arrs  map[string]string
	vars  map[string]string
	cases []*caseBlock
	pos   int
}

// caseBlock is one shell case statement. Only the branch whose pattern matches
// the value runs, so only its commands are shown.
type caseBlock struct {
	value   string // the word after "case", resolved when it can be
	emit    bool   // this branch is the one that runs
	matched bool   // a branch matched already, so "*)" is not the default any more
}

// step handles a line that is case structure, reporting whether it was.
func (w *walk) step(line string) bool {
	if expr, ok := caseHead(line); ok {
		w.cases = append(w.cases, &caseBlock{value: w.fill(expr)})
		return true
	}
	if len(w.cases) == 0 {
		return false
	}
	c := w.cases[len(w.cases)-1]
	switch {
	case line == "esac":
		w.cases = w.cases[:len(w.cases)-1]
	case line == ";;":
		c.emit = false
	case isBranch(line):
		pat := strings.TrimSpace(strings.TrimSuffix(line, ")"))
		c.emit = matchBranch(pat, c.value, c.matched)
		c.matched = c.matched || c.emit
	default:
		return false
	}
	return true
}

// live reports whether every case around the current line runs.
func (w *walk) live() bool {
	for _, c := range w.cases {
		if !c.emit {
			return false
		}
	}
	return true
}

// fill expands a line's arrays, positional arguments and variables.
func (w *walk) fill(s string) string {
	return substitute(expandArrays(s, w.arrs), w.args, w.pos, w.vars)
}

// caseHead returns the word a "case <word> in" line tests.
func caseHead(line string) (string, bool) {
	if !strings.HasPrefix(line, "case ") || !strings.HasSuffix(line, " in") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "case "), " in")), true
}

// isBranch reports a branch header such as "list)", "delete-manifests)" or "*)".
// A pattern never contains a space, which keeps a line that merely ends in a
// command substitution from reading as one.
func isBranch(line string) bool {
	if !strings.HasSuffix(line, ")") {
		return false
	}
	pat := strings.TrimSuffix(line, ")")
	return pat != "" && !strings.ContainsAny(pat, " \t")
}

// matchBranch reports whether a branch pattern selects value. "*)" is the
// default and runs only when nothing before it matched.
func matchBranch(pat, value string, matched bool) bool {
	if pat == "*" {
		return !matched
	}
	for _, alt := range strings.Split(pat, "|") {
		if strings.Trim(strings.TrimSpace(alt), `"'`) == value {
			return true
		}
	}
	return false
}

// logicalLines splits a script into statements-worth-reading: comments and
// blank lines dropped, backslash continuations joined.
func logicalLines(script string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasSuffix(line, `\`) {
			cur.WriteString(strings.TrimRight(strings.TrimSuffix(line, `\`), " \t"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(line)
		flush()
	}
	flush()
	return out
}

// arrays returns the array definitions a script makes, and the lines that made
// them. probe.sh builds its oc invocation once and calls "${oc[@]}" three
// times; the three uses say far more than the definition does.
func arrays(lines []string) (map[string]string, map[string]bool) {
	arrs := map[string]string{}
	defs := map[string]bool{}
	for _, l := range lines {
		i := strings.Index(l, "=(")
		if i <= 0 {
			continue
		}
		name := l[:i]
		if !ident(name) {
			continue
		}
		body := strings.TrimPrefix(l[i+1:], "(")
		if j := strings.LastIndex(body, ")"); j >= 0 {
			body = body[:j]
		}
		arrs[name] = strings.TrimSpace(body)
		defs[l] = true
	}
	return arrs, defs
}

// expandArrays replaces "${name[@]}" with the array's contents.
func expandArrays(s string, arrs map[string]string) string {
	for name, body := range arrs {
		s = strings.ReplaceAll(s, `"${`+name+`[@]}"`, body)
		s = strings.ReplaceAll(s, "${"+name+`[@]}`, body)
	}
	return s
}

var argRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*|[0-9])\}?`)

// substitute replaces $1..$9, $@ and variables it has resolved. `${x[...]}` and
// anything else is left as written.
func substitute(s string, args []string, pos int, vars map[string]string) string {
	if len(args) > pos {
		rest := strings.Join(args[pos:], " ")
		s = strings.ReplaceAll(s, `"$@"`, rest)
		s = strings.ReplaceAll(s, "$@", rest)
	}
	return argRef.ReplaceAllStringFunc(s, func(m string) string {
		name := argRef.FindStringSubmatch(m)[1]
		if v, ok := vars[name]; ok {
			return v
		}
		if n, err := strconv.Atoi(name); err == nil {
			if i := pos + n - 1; i >= 0 && i < len(args) {
				return args[i]
			}
		}
		return m
	})
}

// statements splits a line on the semicolons that separate commands, leaving
// quoted ones alone.
func statements(line string) []string {
	var out []string
	var b strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ';':
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// shift reports a "shift N" statement, which moves the positional arguments on.
func shift(stmt string) (int, bool) {
	f := strings.Fields(stmt)
	if len(f) == 0 || f[0] != "shift" {
		return 0, false
	}
	if len(f) == 1 {
		return 1, true
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// assign records a statement that is nothing but name=value pairs. Only a value
// that resolves to plain text is kept: half a path, or a ${x%%=*} that would
// leak into a name, reads worse than the variable itself.
func assign(stmt string, args []string, pos int, vars map[string]string) bool {
	f := strings.Fields(stmt)
	if len(f) == 0 {
		return false
	}
	next := make(map[string]string, len(f))
	for _, kv := range f {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !ident(k) {
			return false
		}
		if v = substitute(v, args, pos, vars); !strings.Contains(v, "$") {
			next[k] = v
		}
	}
	for k, v := range next {
		vars[k] = v
	}
	return true
}

// toolLine finds the first whitelisted tool in a line and returns the command
// from there to whichever operator ends it.
func toolLine(s string, whitelist []string) (string, bool) {
	at := -1
	for _, t := range whitelist {
		i := indexWord(s, t)
		if i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return "", false
	}
	return truncate(cutCommand(s[at:])), true
}

// indexWord finds w in s where it is a whole word: it starts the text or follows
// a space, quote, bracket or shell operator, and ends the text or precedes one.
func indexWord(s, w string) int {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return -1
		}
		j += i
		end := j + len(w)
		before := j == 0 || strings.ContainsRune(" \t;(\"'`", rune(s[j-1]))
		after := end == len(s) || strings.ContainsRune(" \t)\"';|", rune(s[end]))
		if before && after {
			return j
		}
		i = j + 1
	}
}

// cutCommand cuts the command at the first operator that starts something else:
// a pipe, a redirect, a background job, a closing substitution.
func cutCommand(s string) string {
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '|' || r == '&' || r == ')':
			return strings.TrimSpace(s[:i])
		case r == '>':
			return strings.TrimSpace(dropRedirectFD(s[:i]))
		}
	}
	return strings.TrimSpace(s)
}

// dropRedirectFD removes the file descriptor that belongs to a redirect: the
// "2" of "2>/dev/null", which would otherwise read as an argument.
func dropRedirectFD(s string) string {
	n := len(s)
	if n == 0 || (s[n-1] != '2' && s[n-1] != '1') {
		return s
	}
	if n == 1 || s[n-2] == ' ' || s[n-2] == '\t' {
		return s[:n-1]
	}
	return s
}

// truncate keeps the start of a command, which says what it does.
func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxLine {
		return s
	}
	return string([]rune(s)[:maxLine-1]) + "…"
}

// ident reports whether s is a shell variable name.
func ident(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		letter := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		digit := i > 0 && r >= '0' && r <= '9'
		if !letter && !digit {
			return false
		}
	}
	return true
}

// dedup drops repeated commands, keeping the order of first appearance.
func dedup(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
