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
			value = "<dynamic>"
		}
		segment.Argv = append(segment.Argv, value)
	}
	segment.Argv = unwrapEnv(segment.Argv)
	if len(segment.Argv) != 0 {
		segment.HostExecutable = filepath.Base(segment.Argv[0])
		segment.Interpreter, segment.InterpreterPayload = interpreterBoundary(segment.Argv)
	}
	return segment
}

func staticWord(word *syntax.Word) (string, bool) {
	var builder strings.Builder
	var appendParts func([]syntax.WordPart) bool
	appendParts = func(parts []syntax.WordPart) bool {
		for _, part := range parts {
			switch value := part.(type) {
			case *syntax.Lit:
				builder.WriteString(value.Value)
			case *syntax.SglQuoted:
				builder.WriteString(value.Value)
			case *syntax.DblQuoted:
				if !appendParts(value.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !appendParts(word.Parts) {
		return "", false
	}
	return builder.String(), true
}

func unwrapEnv(argv []string) []string {
	if len(argv) == 0 || filepath.Base(argv[0]) != "env" {
		return argv
	}
	index := 1
	for index < len(argv) {
		value := argv[index]
		if strings.Contains(value, "=") && !strings.HasPrefix(value, "=") {
			index++
			continue
		}
		if value == "--" {
			index++
		}
		break
	}
	return argv[index:]
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
	analysis, err := AnalyzeCommand(command)
	if err != nil {
		return false
	}
	prefixArgv, err := parseStaticPrefix(prefix)
	if err != nil {
		return false
	}
	if action != ActionDeny && action != ActionHold && action != ActionAsk &&
		(analysis.Complex || len(analysis.Segments) != 1) {
		return false
	}
	// Ask, deny, and hold scan every segment: a piped or chained command
	// that touches a gated prefix must still gate, and asking can only add
	// friction. Allow keeps single-segment matching so a prefix rule never
	// approves a composite it cannot see through.
	for _, segment := range analysis.Segments {
		if segment.Dynamic || (action == ActionAllow && segment.InterpreterPayload) {
			continue
		}
		if argvPrefix(segment.Argv, prefixArgv) {
			return true
		}
	}
	return false
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
