package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

/*
Keeps .env.example honest.

Its header states: "Every variable here is read exactly once, in internal/config.
Nothing else in the codebase calls os.Getenv, so this file is the complete list of
knobs." That is a useful promise and, until this test, an unenforced one — six keys
had already drifted out of it (METRICS_ENABLED, TRACING_ENABLED, TRACING_ENDPOINT,
TRACING_SAMPLE_RATIO, APP_VERSION, REDIS_URL).

Nothing breaks when a knob goes undocumented, which is exactly why it happens: the
default applies, the app runs, and the only casualty is the person deploying it who
cannot know the setting exists. METRICS_ENABLED defaults to TRUE and gates /metrics
and /status — two unauthenticated endpoints exposing internal topology. Somebody
shipping to production from an incomplete .env.example would have had no way to know
they were on by default.

The test reads THIS source file for the keys rather than a hand-maintained list, so
adding a knob and forgetting to document it fails here.
*/

// Matches every env key read through the config helpers or os.Getenv. Kept in sync
// with the helper set below, which is asserted separately.
var envKeyPattern = regexp.MustCompile(
	`(?:require|optString|optInt|optBool|optFloat|optDuration|optCSV|os\.Getenv)\("([A-Z_0-9]+)"`)

func keysReadByConfig(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("reading config.go: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range envKeyPattern.FindAllStringSubmatch(string(src), -1) {
		seen[m[1]] = true
	}
	if len(seen) < 20 {
		t.Fatalf("only found %d env keys; the pattern has probably drifted from the "+
			"helper names and this test would pass vacuously", len(seen))
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysDeclaredIn(t *testing.T, path string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		// Commented-out keys do not count. A knob documented only inside a comment
		// is invisible to anyone diffing their own .env against this file.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, found := strings.Cut(line, "="); found {
			out[strings.TrimSpace(name)] = true
		}
	}
	return out
}

func TestEnvExampleDocumentsEveryKnob(t *testing.T) {
	declared := keysDeclaredIn(t, filepath.Join("..", "..", ".env.example"))

	var missing []string
	for _, key := range keysReadByConfig(t) {
		if !declared[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("config reads these but .env.example does not document them: %s\n"+
			"  Every knob belongs in .env.example with the reason it exists. A default "+
			"that nobody knows about is a decision made by accident.",
			strings.Join(missing, ", "))
	}
}

func TestEnvExampleHasNoDeadKeys(t *testing.T) {
	read := map[string]bool{}
	for _, k := range keysReadByConfig(t) {
		read[k] = true
	}

	var dead []string
	for key := range keysDeclaredIn(t, filepath.Join("..", "..", ".env.example")) {
		if !read[key] {
			dead = append(dead, key)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf(".env.example documents keys nothing reads: %s\n"+
			"  A setting that looks configurable and is not is worse than no setting: "+
			"somebody will change it and conclude the app ignores its own config.",
			strings.Join(dead, ", "))
	}
}

func TestEnvKeyPatternCoversEveryHelper(t *testing.T) {
	/*
		The guard on the guard. This test's regex lists helper names, so a NEW helper
		— optDuration was once new — would make every key read through it invisible,
		and both tests above would pass while documenting nothing.

		Caught exactly that: my first version of this pattern omitted optCSV and
		optFloat, which reported CORS_ALLOWED_ORIGINS as a dead key and hid
		TRACING_SAMPLE_RATIO entirely.
	*/
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("reading config.go: %v", err)
	}

	declared := regexp.MustCompile(`(?m)^func ((?:opt|require)[A-Za-z]*)\(`).
		FindAllStringSubmatch(string(src), -1)
	if len(declared) == 0 {
		t.Fatal("found no config helpers; the helper pattern has drifted")
	}

	for _, m := range declared {
		name := m[1]
		if !strings.Contains(envKeyPattern.String(), name) {
			t.Errorf("helper %q exists in config.go but envKeyPattern does not match it; "+
				"every key read through %s is invisible to the tests above", name, name)
		}
	}
}
