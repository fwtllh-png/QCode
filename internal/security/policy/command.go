package policy

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type CommandSegment struct {
	Argv               []string `json:"argv"`
	HostExecutable     string   `json:"host_executable"`
	Interpreter        string   `json:"interpreter,omitempty"`
	InterpreterPayload bool     `json:"interpreter_payload,omitempty"`
	Dynamic            bool     `json:"dynamic,omitempty"`
}

type CommandAnalysis struct {
	Segments []CommandSegment `json:"segments"`
	Complex  bool             `json:"complex,omitempty"`
}

func AnalyzeCommand(command string) (CommandAnalysis, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return CommandAnalysis{}, errors.New("command is empty")
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(command), "")
	if err != nil {
		return CommandAnalysis{}, err
	}
	var analysis CommandAnalysis
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node.(type) {
		case *syntax.Redirect, *syntax.Subshell, *syntax.Block,
			*syntax.IfClause, *syntax.WhileClause, *syntax.ForClause,
			*syntax.CaseClause, *syntax.FuncDecl:
			analysis.Complex = true
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		segment := commandSegment(call)
		analysis.Segments = append(analysis.Segments, segment)
		if script, ok := evalPayload(segment); ok {
			nested, nestedErr := AnalyzeCommand(script)
			if nestedErr != nil {
				segment.Dynamic = true
				analysis.Segments[len(analysis.Segments)-1] = segment
			} else {
				analysis.Segments = append(analysis.Segments, nested.Segments...)
			}
		}
		if script, ok := shellPayload(segment.Argv); ok {
			nested, nestedErr := AnalyzeCommand(script)
			if nestedErr != nil {
				segment.Dynamic = true
				analysis.Segments[len(analysis.Segments)-1] = segment
			} else {
				analysis.Segments = append(analysis.Segments, nested.Segments...)
			}
		}
		return true
	})
	if len(analysis.Segments) == 0 {
		return CommandAnalysis{}, errors.New("command has no executable segment")
	}
	return analysis, nil
}

func commandSegment(call *syntax.CallExpr) CommandSegment {
	segment := CommandSegment{Argv: make([]string, 0, len(call.Args))}
	for _, word := range call.Args {
		value, ok := staticWord(word)
		if !ok {
			segment.Dynamic = true
			value = dynamicWord
		}
		segment.Argv = append(segment.Argv, value)
	}
	argv, unwrapped := unwrapEnv(segment.Argv)
	if !unwrapped {
		segment.Dynamic = true
	}
	segment.Argv = argv
	if len(segment.Argv) != 0 {
		segment.HostExecutable = filepath.Base(segment.Argv[0])
		segment.Interpreter, segment.InterpreterPayload = interpreterBoundary(segment.Argv)
	}
	return segment
}

func staticWord(word *syntax.Word) (string, bool) {
	var builder strings.Builder
	var appendParts func([]syntax.WordPart, bool) bool
	appendParts = func(parts []syntax.WordPart, quoted bool) bool {
		for _, part := range parts {
			switch value := part.(type) {
			case *syntax.Lit:
				builder.WriteString(unescapeLiteral(value.Value, quoted))
			case *syntax.SglQuoted:
				builder.WriteString(value.Value)
			case *syntax.DblQuoted:
				if !appendParts(value.Parts, true) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !appendParts(word.Parts, false) {
		return "", false
	}
	return builder.String(), true
}

// unescapeLiteral applies bash backslash removal so `\git` and `g\it` name
// the executable the shell actually runs. Inside double quotes a backslash
// only escapes $, `, ", \ and newline.
func unescapeLiteral(value string, quoted bool) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 == len(value) {
			builder.WriteByte(value[index])
			continue
		}
		next := value[index+1]
		switch {
		case next == '\n':
			index++
		case !quoted || strings.IndexByte("$`\"\\", next) >= 0:
			builder.WriteByte(next)
			index++
		default:
			builder.WriteByte(value[index])
		}
	}
	return builder.String()
}

// unwrapEnv strips `env` and its options and assignments. It reports false
// when an option makes the executed argv opaque (a split string) or unknown.
func unwrapEnv(argv []string) ([]string, bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "env" {
		return argv, true
	}
	index := 1
	for index < len(argv) {
		value := argv[index]
		switch {
		case value == "--":
			return argv[index+1:], true
		case value == "-" || value == "-i" || value == "--ignore-environment" ||
			value == "-0" || value == "--null" || value == "-v" || value == "--debug":
			index++
		case value == "-u" || value == "-C" || value == "-P":
			index += 2
		case strings.HasPrefix(value, "--unset=") || strings.HasPrefix(value, "--chdir="):
			index++
		case value == "-S" || strings.HasPrefix(value, "-S") ||
			strings.HasPrefix(value, "--split-string"):
			return argv, false
		case strings.HasPrefix(value, "-"):
			return argv, false
		case strings.Contains(value, "=") && !strings.HasPrefix(value, "="):
			index++
		default:
			return argv[index:], true
		}
	}
	if index > len(argv) {
		index = len(argv)
	}
	return argv[index:], true
}

func evalPayload(segment CommandSegment) (string, bool) {
	if len(segment.Argv) < 2 || segment.HostExecutable != "eval" || segment.Dynamic {
		return "", false
	}
	return strings.Join(segment.Argv[1:], " "), true
}

func interpreterBoundary(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	name := filepath.Base(argv[0])
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "fish":
		for _, value := range argv[1:] {
			if value == "-c" || value == "-lc" {
				return name, true
			}
		}
		return name, false
	case "python", "python2", "python3", "node", "ruby", "perl", "php":
		for _, value := range argv[1:] {
			if value == "-c" || value == "-e" {
				return name, true
			}
		}
		return name, false
	}
	return "", false
}

func shellPayload(argv []string) (string, bool) {
	interpreter, payload := interpreterBoundary(argv)
	if !payload || !isShell(interpreter) {
		return "", false
	}
	for index := 1; index+1 < len(argv); index++ {
		if argv[index] == "-c" || argv[index] == "-lc" {
			return argv[index+1], true
		}
	}
	return "", false
}

func isShell(name string) bool {
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "fish":
		return true
	default:
		return false
	}
}

func commandRuleMatches(command, prefix string, action Action) bool {
	prefixArgv, err := parseStaticPrefix(prefix)
	if err != nil {
		return false
	}
	restrictive := action == ActionDeny || action == ActionHold || action == ActionAsk
	analysis, err := AnalyzeCommand(command)
	if err != nil {
		// A command the analyzer cannot read cannot be shown to avoid a
		// gated prefix: restrictive rules fail closed, allow never widens.
		return restrictive
	}
	if !restrictive {
		if analysis.Complex || len(analysis.Segments) != 1 {
			return false
		}
		segment := analysis.Segments[0]
		return !segment.Dynamic && !segment.InterpreterPayload &&
			argvPrefix(segment.Argv, prefixArgv)
	}
	// Ask, deny, and hold scan every segment: a piped or chained command
	// that touches a gated prefix must still gate, and asking can only add
	// friction. Allow keeps single-segment literal matching so a prefix rule
	// never approves a composite, a wrapper, or a path it cannot see through.
	for _, segment := range analysis.Segments {
		if restrictiveSegmentMatches(segment, prefixArgv) {
			return true
		}
	}
	return false
}

// restrictiveSegmentMatches answers "could this segment run the prefix?".
// Executables compare by base name, dynamic words match any prefix word,
// wrapper commands are searched through, options between prefix words are
// skipped, and opaque payloads (unparsable shell or eval text, or an
// interpreter program naming every prefix word) match.
func restrictiveSegmentMatches(segment CommandSegment, prefix []string) bool {
	argv := segment.Argv
	if len(argv) == 0 {
		return false
	}
	if segment.Dynamic && (segment.HostExecutable == "eval" ||
		(segment.InterpreterPayload && isShell(segment.Interpreter))) {
		return true
	}
	if segment.InterpreterPayload && !isShell(segment.Interpreter) &&
		payloadNamesPrefix(argv, prefix) {
		return true
	}
	if argvMatchesFrom(argv, 0, prefix, false) {
		return true
	}
	if !commandWrapper(argv[0]) {
		return false
	}
	// xargs and find -exec complete the executed argv at run time, so a
	// prefix cut short by the end of their argument list still matches.
	partial := false
	for index := range argv {
		if name := executableName(argv[index]); name == "xargs" || name == "find" {
			partial = true
		}
		if index > 0 && argvMatchesFrom(argv, index, prefix, partial) {
			return true
		}
	}
	return false
}

func argvMatchesFrom(argv []string, start int, prefix []string, partial bool) bool {
	if start >= len(argv) || !wordMatches(argv[start], prefix[0], true) {
		return false
	}
	index := start + 1
	for _, want := range prefix[1:] {
		matched := false
		afterOption := false
		for index < len(argv) {
			value := argv[index]
			index++
			if wordMatches(value, want, false) {
				matched = true
				break
			}
			if strings.HasPrefix(value, "-") {
				afterOption = true
				continue
			}
			if afterOption {
				afterOption = false
				continue
			}
			return false
		}
		if !matched {
			return partial
		}
	}
	return true
}

func wordMatches(value, want string, executable bool) bool {
	if value == dynamicWord {
		return true
	}
	if executable {
		return executableName(value) == executableName(want)
	}
	return value == want
}

func executableName(value string) string {
	return filepath.Base(strings.TrimSpace(value))
}

const dynamicWord = "<dynamic>"

// commandWrapper names executables that run another argv taken from their
// own arguments (or, for xargs, from input).
func commandWrapper(value string) bool {
	switch executableName(value) {
	case "command", "builtin", "exec", "env", "nice", "nohup", "time",
		"timeout", "gtimeout", "sudo", "doas", "xargs", "find", "stdbuf",
		"ionice", "setsid", "chrt", "taskset", "caffeinate", "flock",
		"watch", "unbuffer", "busybox", "chroot", "runuser", "su", "script":
		return true
	default:
		return false
	}
}

func payloadNamesPrefix(argv, prefix []string) bool {
	words := map[string]bool{}
	for _, value := range argv[1:] {
		for _, word := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r == '_' || r == '-' || r == '.' || r == '/' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) {
			words[word] = true
			words[executableName(word)] = true
		}
	}
	for index, want := range prefix {
		if index == 0 {
			want = executableName(want)
		}
		if !words[want] {
			return false
		}
	}
	return true
}

func parseStaticPrefix(prefix string) ([]string, error) {
	analysis, err := AnalyzeCommand(prefix)
	if err != nil || analysis.Complex || len(analysis.Segments) != 1 {
		return nil, errors.New("command prefix must be one static command segment")
	}
	segment := analysis.Segments[0]
	if segment.Dynamic || segment.InterpreterPayload || len(segment.Argv) == 0 {
		return nil, errors.New("command prefix crosses a dynamic or interpreter boundary")
	}
	return segment.Argv, nil
}

func argvPrefix(argv, prefix []string) bool {
	if len(prefix) == 0 || len(argv) < len(prefix) {
		return false
	}
	for index := range prefix {
		if argv[index] != prefix[index] {
			return false
		}
	}
	return true
}

func commandGrantIdentity(command string) (string, bool) {
	analysis, err := AnalyzeCommand(command)
	if err != nil {
		return "", false
	}
	encoded, err := json.Marshal(analysis)
	return string(encoded), err == nil
}

// commandGrantPrefix returns the static argv of a single-segment command.
// A reusable approval for such a command also matches later commands that
// extend the argv within the same grant scope, so re-running a build with
// one extra flag does not restart approval. Composite, dynamic, or
// interpreter-payload commands have no prefix: exact identity only.
func commandGrantPrefix(command string) []string {
	analysis, err := AnalyzeCommand(command)
	if err != nil || analysis.Complex || len(analysis.Segments) != 1 {
		return nil
	}
	segment := analysis.Segments[0]
	if segment.Dynamic || segment.InterpreterPayload || len(segment.Argv) == 0 {
		return nil
	}
	return segment.Argv
}

func unsafePersistentPrefix(prefix string) bool {
	argv, err := parseStaticPrefix(prefix)
	if err != nil {
		return true
	}
	name := filepath.Base(argv[0])
	if isShell(name) {
		return true
	}
	switch name {
	case "python", "python2", "python3", "node", "ruby", "perl", "php":
		return true
	case "git", "rm":
		return len(argv) == 1
	default:
		return false
	}
}
