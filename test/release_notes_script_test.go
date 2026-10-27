package test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// releaseNotesScript resolves .github/scripts/assemble-release-notes.sh and
// skips the test on hosts without the shell tools it needs.
func releaseNotesScript(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"bash", "awk", "wc", "tr", "mktemp"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not available: %v", bin, err)
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve the test file location")
	}
	script := filepath.Join(filepath.Dir(file), "..", ".github", "scripts", "assemble-release-notes.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("release notes script is missing: %v", err)
	}
	return script
}

type releaseNotesInput struct {
	assets    string
	entries   string
	generated string
	link      string
}

// assembleReleaseNotes runs the release notes helper with an explicit body cap
// and returns the written body.
func assembleReleaseNotes(t *testing.T, input releaseNotesInput, maxBytes int) []byte {
	t.Helper()
	script := releaseNotesScript(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	output := filepath.Join(dir, "output.md")
	cmd := exec.Command("bash", script,
		write("assets.md", input.assets),
		write("entries.md", input.entries),
		write("generated.md", input.generated),
		write("link.md", input.link),
		output,
	)
	cmd.Env = append(environWithoutBodyCap(), fmt.Sprintf("MAX_RELEASE_BODY_BYTES=%d", maxBytes))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("assemble-release-notes.sh failed: %v\n%s", err, stderr.String())
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read assembled body: %v", err)
	}
	return body
}

func environWithoutBodyCap() []string {
	env := os.Environ()
	filtered := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, "MAX_RELEASE_BODY_BYTES=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

const releaseNotesAssets = "<!-- release-assets:start -->\n## Linux release assets\n\n- `CLIProxyAPI_linux_amd64.tar.gz` is the default Linux build.\n<!-- release-assets:end -->\n"

func releaseNotesLink() string {
	return "**Full Changelog**: https://github.com/router-for-me/CLIProxyAPI/compare/v1.0.0...v1.1.0\n"
}

func TestAssembleReleaseNotesSmallBodyIsUnchanged(t *testing.T) {
	entries := "- oldest change (aaa111)\n- newest change (bbb222)"
	generated := "## What's Changed\n\n* newest pull request\n\n**Full Changelog**: https://github.com/router-for-me/CLIProxyAPI/compare/v1.0.0...v1.1.0\n"
	input := releaseNotesInput{
		assets:    releaseNotesAssets,
		entries:   entries,
		generated: generated,
		link:      releaseNotesLink(),
	}
	want := releaseNotesAssets + "\n" + "## Changelog\n\n" + entries + "\n\n" + generated
	body := assembleReleaseNotes(t, input, 120000)
	if string(body) != want {
		t.Fatalf("small body changed\nwant:\n%q\ngot:\n%q", want, string(body))
	}
	if bytes.Contains(body, []byte("trimmed")) {
		t.Fatalf("small body must not carry a truncation notice:\n%s", body)
	}
}

func TestAssembleReleaseNotesOversizedKeepsNewestEntries(t *testing.T) {
	var entries strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&entries, "- change %03d (hash%03d)\n", i, i)
	}
	input := releaseNotesInput{
		assets:    releaseNotesAssets,
		entries:   entries.String(),
		generated: "## What's Changed\n\n* newest pull request\n\n" + releaseNotesLink(),
		link:      releaseNotesLink(),
	}
	const maxBytes = 1200
	body := assembleReleaseNotes(t, input, maxBytes)

	if len(body) > maxBytes {
		t.Fatalf("body is %d bytes, over the %d byte cap", len(body), maxBytes)
	}
	text := string(body)
	for _, want := range []string{"## Linux release assets", "## Changelog", "trimmed", releaseNotesLink()} {
		if !strings.Contains(text, want) {
			t.Fatalf("oversized body is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "- change 001 ") {
		t.Fatalf("oldest entry should be dropped:\n%s", text)
	}
	if !strings.Contains(text, "- change 200 ") {
		t.Fatalf("newest entry should be kept:\n%s", text)
	}
	if count := strings.Count(text, "**Full Changelog**"); count != 1 {
		t.Fatalf("oversized body carries %d changelog links, want 1:\n%s", count, text)
	}

	kept := keptEntryNumbers(t, text)
	if len(kept) == 0 {
		t.Fatalf("no changelog entries were kept:\n%s", text)
	}
	if kept[len(kept)-1] != 200 {
		t.Fatalf("last kept entry is %d, want the newest entry 200", kept[len(kept)-1])
	}
	for i := 1; i < len(kept); i++ {
		if kept[i] != kept[i-1]+1 {
			t.Fatalf("kept entries are not a contiguous chronological run: %v", kept)
		}
	}
}

func keptEntryNumbers(t *testing.T, text string) []int {
	t.Helper()
	var kept []int
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "- change ") {
			continue
		}
		value := strings.TrimPrefix(line, "- change ")
		if len(value) < 3 {
			t.Fatalf("malformed entry line %q", line)
		}
		number, err := strconv.Atoi(value[:3])
		if err != nil {
			t.Fatalf("malformed entry line %q: %v", line, err)
		}
		kept = append(kept, number)
	}
	return kept
}

func TestAssembleReleaseNotesOversizedBodyStaysValidUTF8(t *testing.T) {
	var entries strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&entries, "- 修复第 %03d 项变更 (hash%03d) — 中文说明 ✅\n", i, i)
	}
	input := releaseNotesInput{
		assets:    releaseNotesAssets,
		entries:   entries.String(),
		generated: "## What's Changed\n\n* 新增功能说明 🚀\n\n" + releaseNotesLink(),
		link:      releaseNotesLink(),
	}
	validLines := map[string]bool{}
	for _, line := range strings.Split(entries.String(), "\n") {
		validLines[line] = true
	}
	for _, maxBytes := range []int{400, 413, 701, 1200, 2500, 6000} {
		body := assembleReleaseNotes(t, input, maxBytes)
		if len(body) > maxBytes {
			t.Fatalf("cap %d: body is %d bytes", maxBytes, len(body))
		}
		if !utf8.Valid(body) {
			t.Fatalf("cap %d: body is not valid UTF-8:\n%q", maxBytes, body)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "- 修复") {
				continue
			}
			if !validLines[line] {
				t.Fatalf("cap %d: kept a partial entry line %q", maxBytes, line)
			}
		}
	}
}

func TestAssembleReleaseNotesCapsOversizedStaticFrame(t *testing.T) {
	var assets strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&assets, "- asset note line %02d 说明\n", i)
	}
	input := releaseNotesInput{
		assets:    assets.String(),
		entries:   "- single change (aaa111)\n",
		generated: "## What's Changed\n\n* one pull request\n",
		link:      releaseNotesLink(),
	}
	for _, maxBytes := range []int{1, 120, 130, 250, 400, 900} {
		body := assembleReleaseNotes(t, input, maxBytes)
		if len(body) > maxBytes {
			t.Fatalf("cap %d: body is %d bytes", maxBytes, len(body))
		}
		if !utf8.Valid(body) {
			t.Fatalf("cap %d: body is not valid UTF-8:\n%q", maxBytes, body)
		}
		if maxBytes >= len(releaseNotesLink()) && !strings.Contains(string(body), "**Full Changelog**") {
			t.Fatalf("cap %d: body dropped the changelog link:\n%s", maxBytes, body)
		}
	}
}

func TestAssembleReleaseNotesKeepsSingleCompareLink(t *testing.T) {
	input := releaseNotesInput{
		assets:    releaseNotesAssets,
		entries:   "- " + strings.Repeat("x", 2000) + " (aaa111)\n",
		generated: "## What's Changed\n\n* one pull request\n\n" + releaseNotesLink(),
		link:      releaseNotesLink(),
	}
	const maxBytes = 1000
	body := assembleReleaseNotes(t, input, maxBytes)
	if len(body) > maxBytes {
		t.Fatalf("body is %d bytes, over the %d byte cap", len(body), maxBytes)
	}
	if count := strings.Count(string(body), "**Full Changelog**"); count != 1 {
		t.Fatalf("body carries %d changelog links, want 1:\n%s", count, body)
	}
	if !strings.Contains(string(body), "one pull request") {
		t.Fatalf("generated notes were dropped from the tail of the body:\n%s", body)
	}
}
