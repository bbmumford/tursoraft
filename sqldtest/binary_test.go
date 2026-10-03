//go:build unix

package sqldtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const (
	sqld023   = "sqld sqld 0.23.0 (52d8b8f1 2024-02-26)"
	sqld02409 = "sqld sqld 0.24.9 (00000000 2024-12-01)"
	sqld02432 = "sqld sqld 0.24.32 (40c272de 2025-02-14)"
	sqld0251  = "sqld sqld 0.25.1 (00000000 2025-06-01)"
	noVersion = "sqld (no version)"
)

// machine is the set of sqld binaries Binary can find, each given as the line
// its --version prints; "" means no binary there.
type machine struct {
	homebrew string            // the stand-in for /opt/homebrew/bin/sqld
	onPath   string            // the sqld on PATH
	releases map[string]string // ~/.local/share/libsql-server/<name>/sqld
	override string            // $TURSORAFT_SQLD's binary; "" leaves it unset
}

// install writes the machine's binaries as shell scripts under a temporary
// directory, runs each once, points homebrewSqld, PATH, HOME and
// TURSORAFT_SQLD at them, and returns each binary's path by "homebrew",
// "path", "override" or its release directory name.
func (m machine) install(t *testing.T) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{}
	write := func(name, path, version string) {
		if version == "" {
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\necho '" + version + "'\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		// A newly written executable's first run is much slower than its
		// later runs, and under load it can pass binaryVersion's ten-second
		// bound, so Binary would rank the stand-in as unreadable and the
		// case would fail on timing, not selection. Running each stand-in
		// once here, with no bound, takes that first run outside Binary.
		if err := exec.Command(path, "--version").Run(); err != nil {
			t.Fatalf("first run of the %s stand-in: %v", name, err)
		}
		paths[name] = path
	}
	write("homebrew", filepath.Join(root, "homebrew", "bin", "sqld"), m.homebrew)
	write("path", filepath.Join(root, "path", "sqld"), m.onPath)
	write("override", filepath.Join(root, "override", "sqld"), m.override)
	home := filepath.Join(root, "home")
	for name, version := range m.releases {
		write(name, filepath.Join(home, ".local", "share", "libsql-server", name, "sqld"), version)
	}
	if err := os.MkdirAll(filepath.Join(root, "path"), 0o755); err != nil {
		t.Fatal(err)
	}

	previous := homebrewSqld
	homebrewSqld = filepath.Join(root, "homebrew", "bin", "sqld")
	t.Cleanup(func() { homebrewSqld = previous })
	t.Setenv("PATH", filepath.Join(root, "path"))
	t.Setenv("HOME", home)
	t.Setenv("TURSORAFT_SQLD", paths["override"])
	return paths
}

// TestTheDefaultIsTheNewestSqld: with $TURSORAFT_SQLD unset, Binary takes
// the candidate whose --version names the newest release, wherever it sits
// among Homebrew, PATH and the release directories.
func TestTheDefaultIsTheNewestSqld(t *testing.T) {
	cases := []struct {
		name string
		m    machine
		want string
	}{
		{"a release build newer than Homebrew and PATH",
			machine{homebrew: sqld023, onPath: sqld023, releases: map[string]string{"0.24.32": sqld02432}}, "0.24.32"},
		{"a sqld on PATH newer than Homebrew's and a release build",
			machine{homebrew: sqld023, onPath: sqld0251, releases: map[string]string{"0.24.32": sqld02432}}, "path"},
		{"the later of two release builds by number, not by name",
			machine{homebrew: sqld023, releases: map[string]string{"0.24.32": sqld02432, "0.24.9": sqld02409}}, "0.24.32"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := c.m.install(t)
			if got, want := Binary(), paths[c.want]; got != want {
				t.Fatalf("Binary() = %q, want %q (%s)", got, want, c.want)
			}
		})
	}
}

// TestEqualVersionsKeepTheEarlierCandidate: when two candidates answer
// --version with the same release, and when neither answers with one at all,
// Binary keeps whichever candidates lists first. candidates lists Homebrew's
// sqld, then the sqld on PATH, then each release directory filepath.Glob
// returns, so keeping the first of an equal set is what fixes the sqld a run
// uses to that order instead of to whichever equal candidate the loop reaches
// last. Binary replaces its choice only on a strictly newer readable version,
// and only a readable one can displace a readable one, which are the two
// conditions this pins.
func TestEqualVersionsKeepTheEarlierCandidate(t *testing.T) {
	cases := []struct {
		name string
		m    machine
	}{
		{"two candidates name the same release",
			machine{homebrew: sqld02432, releases: map[string]string{"0.24.32": sqld02432}}},
		{"neither candidate prints a version",
			machine{homebrew: noVersion, releases: map[string]string{"0.24.32": noVersion}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := c.m.install(t)
			if got, want := Binary(), paths["homebrew"]; got != want {
				t.Fatalf("Binary() = %q, want %q (homebrew)", got, want)
			}
		})
	}
}

// TestAnOlderOrUnreadableSqldIsNotTheDefault: Binary never prefers a sqld
// whose version cannot be read over one whose version can, never replaces
// $TURSORAFT_SQLD with a newer sqld, and finds nothing on a machine with no
// sqld.
func TestAnOlderOrUnreadableSqldIsNotTheDefault(t *testing.T) {
	cases := []struct {
		name string
		m    machine
		want string // "" expects no binary
	}{
		{"Homebrew's sqld prints no version",
			machine{homebrew: noVersion, onPath: sqld023}, "path"},
		{"$TURSORAFT_SQLD names an older sqld than a release build",
			machine{homebrew: sqld023, releases: map[string]string{"0.24.32": sqld02432}, override: sqld023}, "override"},
		{"a machine with no sqld", machine{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := c.m.install(t)
			if got, want := Binary(), paths[c.want]; got != want {
				t.Fatalf("Binary() = %q, want %q (%s)", got, want, c.want)
			}
		})
	}
}
