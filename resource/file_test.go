package resource

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goss-org/goss/system"
	"github.com/goss-org/goss/util"

	"gopkg.in/yaml.v3"
)

// Contents must stay unset and marshal away: an empty list asserts nothing, so
// every generated gossfile would warn about it.
func TestNewFileLeavesContentsUnset(t *testing.T) {
	sysFile := system.NewDefFile(t.Context(), "/etc/hostname", nil, util.Config{})
	f, err := NewFile(sysFile, util.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Contents != nil {
		t.Errorf("NewFile Contents = %#v, want nil", f.Contents)
	}

	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "contents") {
		t.Errorf("marshalled file contains a contents key:\n%s", out)
	}
}

// countOpenFDs reports the number of file descriptors the current process has
// open, so repeated validation rounds can be checked against a baseline.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	var dir string
	switch runtime.GOOS {
	case "linux":
		dir = "/proc/self/fd"
	case "darwin":
		dir = "/dev/fd"
	default:
		t.Skipf("descriptor counting is not supported on %s", runtime.GOOS)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("cannot enumerate open descriptors via %s: %v", dir, err)
	}
	return len(entries)
}

// Repeatedly validating the same content checks (as goss serve does on every
// cache cycle) must return the process to its pre-round descriptor count,
// whether the checks pass or are forced to fail. The hash and size checks run
// alongside contents and stay independent of it.
func TestFileContentsValidationReleasesDescriptorsAcrossRounds(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "content.txt")
	content := "first line\nsecond line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum([]byte(content))

	newResource := func(contents matcher) *File {
		return &File{
			id:       path,
			Path:     path,
			Exists:   true,
			Contents: contents,
			Size:     len(content),
			Md5:      fmt.Sprintf("%x", sum),
		}
	}

	cases := []struct {
		name     string
		contents matcher
		want     int
	}{
		{"passing-patterns", []any{"first line", "second line"}, SUCCESS},
		{"failing-patterns", []any{"this-pattern-is-absent"}, FAIL},
		{"passing-scalar", content, SUCCESS},
		{"failing-scalar", "this-pattern-is-absent", FAIL},
	}

	const rounds = 200
	sys := system.New("")

	// Warm up so any lazily created process-wide descriptors (runtime poller,
	// package detection) exist before the baseline is taken.
	for range 5 {
		newResource(cases[0].contents).Validate(ctx, sys)
	}
	runtime.GC()
	baseline := countOpenFDs(t)

	for _, c := range cases {
		f := newResource(c.contents)
		for round := range rounds {
			results := f.Validate(ctx, sys)
			var contentResult *TestResult
			for j := range results {
				r := &results[j]
				switch r.Property {
				case "contents":
					contentResult = r
				case "size", "md5":
					if r.Result != SUCCESS {
						t.Fatalf("%s round %d: %s result = %v, want independent SUCCESS",
							c.name, round, r.Property, r.Result)
					}
				}
			}
			if contentResult == nil {
				t.Fatalf("%s round %d: no contents result", c.name, round)
			}
			if contentResult.Result != c.want {
				t.Fatalf("%s round %d: contents result = %v, want %v",
					c.name, round, contentResult.Result, c.want)
			}
		}
	}

	if got := countOpenFDs(t); got != baseline {
		t.Fatalf("open descriptors = %d after %d content-check rounds, baseline %d (leaked %d)",
			got, rounds*len(cases), baseline, got-baseline)
	}
}
