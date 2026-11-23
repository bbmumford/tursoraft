/*
 * Copyright (c) 2026 Brandon Miller-Mumford.
 * Licensed under the MIT License; see LICENSE.
 */

package tursoraft_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// zeroPseudoVersion is the pseudo-version naming a zero timestamp and an
// all-zero revision, which Go rejects before any fetch.
const zeroPseudoVersion = "v0.0.0-00010101000000-000000000000"

// replacedSiblings is the set of github.com/bbmumford requirements this
// module's go.mod names at v0.0.0 or at the zero pseudo-version. No origin
// serves either version, so each builds only through a replace onto the
// sibling checkout beside this module, the way every module of the estate
// resolves the shared packages until they are tagged.
//
// The set is pinned, not derived, so it fails both ways: a requirement that
// joins the condition fails until it is recorded here with its replace, and
// a recorded one that leaves it (dropped, or given a version a tag serves)
// fails until it is removed. A list kept anywhere else drifts silently when
// go.mod gains or drops a require; this one is read against the literal
// go.mod on every run.
var replacedSiblings = map[string]string{
	"github.com/bbmumford/filekind":   "v0.0.0",
	"github.com/bbmumford/go-libsql":  "v0.0.0",
	"github.com/bbmumford/policypool": "v0.0.0",
}

// goModJSON is the literal go.mod as `go mod edit -json` prints it. That
// reads the file itself; `go list -m` would answer with the effective module
// after replaces, which hides the version the line names.
type goModJSON struct {
	Require []struct {
		Path    string
		Version string
	}
	Replace []struct {
		Old struct{ Path, Version string }
		New struct{ Path, Version string }
	}
}

func readGoMod(t *testing.T) goModJSON {
	t.Helper()
	out, err := exec.Command("go", "mod", "edit", "-json").Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("reading this module's go.mod: %v %s", err, stderr)
	}
	var mod goModJSON
	if err := json.Unmarshal(out, &mod); err != nil {
		t.Fatalf("decoding go.mod: %v", err)
	}
	if len(mod.Require) == 0 {
		t.Fatal("go.mod reported zero requirements, so this guard checked nothing")
	}
	return mod
}

// TestEveryUnservableRequirementIsPinnedAndReplacedOntoItsSibling holds the
// set above against go.mod in both directions and requires each member to
// carry a version-less replace onto ../<name>, so a build of this module
// compiles the sibling tree rather than failing to fetch.
func TestEveryUnservableRequirementIsPinnedAndReplacedOntoItsSibling(t *testing.T) {
	mod := readGoMod(t)

	found := map[string]string{}
	for _, r := range mod.Require {
		if strings.HasPrefix(r.Path, "github.com/bbmumford/") && (r.Version == "v0.0.0" || r.Version == zeroPseudoVersion) {
			found[r.Path] = r.Version
		}
	}
	replaced := map[string]string{}
	for _, r := range mod.Replace {
		if r.Old.Version == "" {
			replaced[r.Old.Path] = r.New.Path
		}
	}

	for path, version := range found {
		want, known := replacedSiblings[path]
		switch {
		case !known:
			t.Errorf("%s is required at %s, which no origin serves; record it here and replace it onto ../%s",
				path, version, path[strings.LastIndex(path, "/")+1:])
		case want != version:
			t.Errorf("%s is required at %s, recorded at %s", path, version, want)
		}
	}
	for path, want := range replacedSiblings {
		if _, still := found[path]; !still {
			t.Errorf("%s is no longer required at %s: drop it from this record", path, want)
			continue
		}
		wantNew := "../" + path[strings.LastIndex(path, "/")+1:]
		if got := replaced[path]; got != wantNew {
			t.Errorf("%s has replace target %q, want the version-less replace onto %q", path, got, wantNew)
		}
	}
	if t.Failed() {
		paths := make([]string, 0, len(found))
		for p := range found {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		t.Logf("requirements at an unservable version (%d): %s", len(paths), strings.Join(paths, " "))
	}
}

// TestEveryReplacedSiblingIsACheckoutOfThatModule reads each replace target
// beside this module and requires it to declare the module path it stands
// in for. The checkouts sit outside this repository, so a sibling directory
// that is absent skips; one that is present without a go.mod fails.
func TestEveryReplacedSiblingIsACheckoutOfThatModule(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this module: %v", err)
	}
	verified := 0
	for path := range replacedSiblings {
		dir := filepath.Join(wd, "..", path[strings.LastIndex(path, "/")+1:])
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Errorf("%s replaces %s and holds no readable go.mod: %v", dir, path, err)
			continue
		}
		verified++
		first := strings.SplitN(string(data), "\n", 2)[0]
		if strings.TrimSpace(strings.TrimPrefix(first, "module")) != path {
			t.Errorf("%s replaces %s, and its go.mod declares %q", dir, path, first)
		}
	}
	if verified == 0 && !t.Failed() {
		t.Skip("no sibling checkout beside this module")
	}
}
