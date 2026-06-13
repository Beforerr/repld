package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func parseCLI(args []string) parsed { return parseArgs(args) }

func TestParseArgsRepld(t *testing.T) {
	got := parseCLI([]string{"python3", "-c", "print(1)"})
	require.Equal(t, "python3", got.exe)
	require.Equal(t, "eval", got.evalMode)
	require.Equal(t, "print(1)", got.code)
	require.Equal(t, "python", resolveLang(got))

	// -c is not Julia's eval flag → forwarded to the interpreter, not consumed.
	got = parseCLI([]string{"julia", "-c", "x"})
	require.Equal(t, "", got.evalMode)
	require.Equal(t, []string{"-c", "x"}, got.fwd)

	// -e is not Python's eval flag → forwarded (python's own -e).
	got = parseCLI([]string{".venv/bin/python", "-e", "x"})
	require.Equal(t, ".venv/bin/python", got.exe)
	require.Equal(t, "python", resolveLang(got))
	require.Equal(t, "", got.evalMode)

	got = parseCLI([]string{"--lang", "julia", "/opt/weird", "-e", "1"})
	require.Equal(t, "/opt/weird", got.exe)
	require.Equal(t, "julia", resolveLang(got), "--lang overrides exe basename")
	require.Equal(t, "eval", got.evalMode)

	// reuse by label, no exe: union flags so -c still parses as eval.
	got = parseCLI([]string{"--session", "ml", "-c", "print(x)"})
	require.Equal(t, "", got.exe)
	require.Equal(t, "ml", got.session)
	require.Equal(t, "eval", got.evalMode)

	got = parseCLI([]string{"sessions"})
	require.Equal(t, "sessions", got.sub, "subcommand is not consumed as exe")
	require.Equal(t, "", got.exe)

	got = parseCLI([]string{"R", "-e", "1 + 1"})
	require.Equal(t, "R", got.exe)
	require.Equal(t, "r", resolveLang(got))
	require.Equal(t, "eval", got.evalMode)

	got = parseCLI([]string{"wolframscript", "-c", "1 + 1"})
	require.Equal(t, "wolframscript", got.exe)
	require.Equal(t, "wolfram", resolveLang(got))
	require.Equal(t, "print", got.evalMode)
}

// TestParseArgsSeparable: repld's own flags are recognized only before the exe;
// after it, non-eval flags forward to the interpreter.
func TestParseArgsSeparable(t *testing.T) {
	// --session before the exe is repld's.
	got := parseCLI([]string{"--session", "scratch", "python3", "interrupt"})
	require.Equal(t, "scratch", got.session)
	require.Equal(t, "python3", got.exe)
	require.Equal(t, "interrupt", got.sub)

	// --project after the exe forwards to the interpreter (Julia consumes it).
	got = parseCLI([]string{"julia", "--project=/env", "-e", "1"})
	require.Equal(t, []string{"--project=/env"}, got.fwd)
	require.Equal(t, "eval", got.evalMode)

	got = parseCLI([]string{"julia", "--project", "/env", "script.jl", "arg1"})
	require.Equal(t, []string{"--project", "/env", "script.jl", "arg1"}, got.fwd)

	got = parseCLI([]string{"--session", "scratch", "python3", "-c", "x = 1"})
	require.Equal(t, "scratch", got.session)
	require.Equal(t, "python3", got.exe)
	require.Equal(t, "eval", got.evalMode)

	got = parseCLI([]string{"--trace", "full", "julia", "-e", "error(\"boom\")"})
	require.Equal(t, "full", got.trace)
	require.Equal(t, "julia", got.exe)
	require.Equal(t, "eval", got.evalMode)
}

// TestParseTargetVerbFirst: trace/interrupt are verb-first; the interpreter and
// its flags follow the verb, locating an existing session.
func TestParseTargetVerbFirst(t *testing.T) {
	p := parseCLI([]string{"trace", "julia", "--project=/env", "--trace", "smart"})
	require.Equal(t, "trace", p.sub)
	tg := parseTarget(p)
	require.Equal(t, "julia", tg.exe)
	require.Equal(t, "julia", tg.lang)
	require.Equal(t, "smart", tg.level)
	require.Equal(t, []string{"--project=/env"}, tg.fwd)

	// Label only — no interpreter needed.
	tg = parseTarget(parseCLI([]string{"interrupt", "--session", "ml"}))
	require.Equal(t, "ml", tg.session)
	require.Equal(t, "", tg.exe)
	require.Equal(t, "", tg.lang)

	// A bare token in the id alphabet targets by session id; real exe
	// names ("r" is too short, others use letters outside k-z) stay exes.
	tg = parseTarget(parseCLI([]string{"close", "kqzm"}))
	require.Equal(t, "kqzm", tg.id)
	require.Equal(t, "", tg.exe)
	tg = parseTarget(parseCLI([]string{"close", "r"}))
	require.Equal(t, "", tg.id)
	require.Equal(t, "r", tg.exe)
	tg = parseTarget(parseCLI([]string{"close", "python"}))
	require.Equal(t, "", tg.id)
	require.Equal(t, "python", tg.exe)
}

func TestParseArgsFileMode(t *testing.T) {
	dir := t.TempDir()
	jl := filepath.Join(dir, "script.jl")
	require.NoError(t, os.WriteFile(jl, []byte("1\n"), 0644))

	// Everything after the file (flags included, no ext required) is script args.
	got := parseCLI([]string{"julia", jl, "a", "--flag", "b"})
	require.Equal(t, jl, got.file)
	require.Equal(t, []string{"a", "--flag", "b"}, got.fileArgs)
	require.Empty(t, got.fwd)

	recipe := filepath.Join(dir, "recipe")
	require.NoError(t, os.WriteFile(recipe, []byte("#!/usr/bin/env -S repld julia\n1\n"), 0755))
	require.Equal(t, recipe, parseCLI([]string{"julia", recipe, "x"}).file)

	// An eval flag disables detection; a missing path still enters file mode.
	got = parseCLI([]string{"julia", "-e", "1", jl})
	require.Equal(t, "", got.file)
	require.Equal(t, []string{jl}, got.fwd)
	require.Equal(t, filepath.Join(dir, "nope.jl"), parseCLI([]string{"julia", filepath.Join(dir, "nope.jl")}).file)

	for _, argv := range [][]string{
		{"julia", "--project=test", jl, "a"},
		{"julia", "--project=test", "--", jl, "a"},
	} {
		got = parseCLI(argv)
		require.Equal(t, []string{"--project=test"}, got.fwd)
		require.Equal(t, jl, got.file)
		require.Equal(t, []string{"a"}, got.fileArgs)
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want parsed
	}{
		{"eval", []string{"-e", "1+1"}, parsed{evalMode: "eval", code: "1+1"}},
		{"julia print long", []string{"julia", "--print", "x"}, parsed{exe: "julia", evalMode: "print", code: "x"}},
		{"our flags", []string{"--session", "s", "--trace", "smart"},
			parsed{session: "s", trace: "smart"}},
		{"-t after exe forwards to julia natively", []string{"julia", "-t", "4", "-e", "c"},
			parsed{exe: "julia", evalMode: "eval", code: "c", fwd: []string{"-t", "4"}}},
		{"passthrough after ours", []string{"--session", "s", "-L", "init.jl", "-e", "c"},
			parsed{session: "s", evalMode: "eval", code: "c", fwd: []string{"-L", "init.jl"}}},
		{"passthrough before ours", []string{"-L", "init.jl", "-e", "c"},
			parsed{evalMode: "eval", code: "c", fwd: []string{"-L", "init.jl"}}},
		{"eq form forwarded whole", []string{"--startup-file=no"},
			parsed{fwd: []string{"--startup-file=no"}}},
		{"juliaup channel forwards after exe", []string{"julia", "+1.11", "-e", "c"},
			parsed{exe: "julia", evalMode: "eval", code: "c", fwd: []string{"+1.11"}}},
		// Missing files forward as launch args even after flags.
		{"missing file forwards after launch flags", []string{"julia", "--project", "/env", "script.jl", "arg1"},
			parsed{exe: "julia", fwd: []string{"--project", "/env", "script.jl", "arg1"}}},
		{"subcommand", []string{"sessions"}, parsed{sub: "sessions"}},
		{"flags before subcommand", []string{"--socket", "x", "sessions"},
			parsed{socket: "x", sub: "sessions"}},
		{"bare tokens forward once forwarding starts", []string{"-L", "a.jl", "sessions"},
			parsed{fwd: []string{"-L", "a.jl", "sessions"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCLI(tc.args)
			if tc.want.socket == "" {
				tc.want.socket = defaultSocket
			}
			require.Equal(t, tc.want.socket, got.socket)
			require.Equal(t, tc.want.session, got.session)
			require.Equal(t, tc.want.trace, got.trace)
			require.Equal(t, tc.want.exe, got.exe)
			require.Equal(t, tc.want.evalMode, got.evalMode)
			require.Equal(t, tc.want.code, got.code)
			require.Equal(t, tc.want.fwd, got.fwd)
			require.Equal(t, tc.want.sub, got.sub)
		})
	}
}
