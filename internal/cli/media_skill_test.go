package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkillCreatesRefusesOverwriteAndSupportsForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".agents", "skills", "tailge", "SKILL.md")
	first := []byte("first skill\n")
	if err := installSkill(path, first, false); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != string(first) {
		t.Fatalf("skill contents=%q err=%v", contents, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("skill permissions=%v err=%v", info, err)
	}
	if err := installSkill(path, []byte("replacement"), false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite without --force: err=%v", err)
	}
	contents, _ = os.ReadFile(path)
	if string(contents) != string(first) {
		t.Fatalf("existing skill changed without force: %q", contents)
	}
	if err := installSkill(path, []byte("replacement\n"), true); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(path)
	if err != nil || string(contents) != "replacement\n" {
		t.Fatalf("forced skill contents=%q err=%v", contents, err)
	}
}

func TestInstallSkillRefusesSymlinkAndDirectoryTargets(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim.md")
	if err := os.WriteFile(victim, []byte("do not modify"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link.md")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := installSkill(link, []byte("replacement"), true); err == nil {
		t.Fatal("force replaced a symlink target")
	}
	contents, err := os.ReadFile(victim)
	if err != nil || string(contents) != "do not modify" {
		t.Fatalf("symlink target changed: contents=%q err=%v", contents, err)
	}
	directory := filepath.Join(base, "skill-dir")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(directory, []byte("replacement"), true); err == nil {
		t.Fatal("force replaced a directory")
	}
}

func TestMediaAndSkillCommandValidation(t *testing.T) {
	application := app{}
	cases := []struct {
		name string
		run  func(*bytes.Buffer, *bytes.Buffer) int
		want string
	}{
		{name: "media missing subcommand", run: func(out, errOut *bytes.Buffer) int { return application.mediaCommand(nil, out, errOut) }, want: "media requires serve"},
		{name: "media missing directory", run: func(out, errOut *bytes.Buffer) int { return application.mediaCommand([]string{"serve"}, out, errOut) }, want: "requires at least one directory"},
		{name: "media empty listen", run: func(out, errOut *bytes.Buffer) int {
			return application.mediaCommand([]string{"serve", "/tmp", "--listen="}, out, errOut)
		}, want: "non-empty ADDRESS:PORT"},
		{name: "media invalid allowed host", run: func(out, errOut *bytes.Buffer) int {
			return application.mediaCommand([]string{"serve", "/tmp", "--allow-host", "https://bad.example"}, out, errOut)
		}, want: "--allow-host must be a hostname or IP address"},
		{name: "media unknown flag", run: func(out, errOut *bytes.Buffer) int {
			return application.mediaCommand([]string{"serve", "/tmp", "--public"}, out, errOut)
		}, want: "unknown flag"},
		{name: "skill missing subcommand", run: func(out, errOut *bytes.Buffer) int { return application.skillCommand(nil, out, errOut) }, want: "skill requires install"},
		{name: "skill extra args", run: func(out, errOut *bytes.Buffer) int {
			return application.skillCommand([]string{"install", "extra"}, out, errOut)
		}, want: "does not accept positional arguments"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := test.run(&stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("validation response code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestMediaListenerHostsIncludeBoundIP(t *testing.T) {
	hosts := mediaListenerHosts(&net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8765})
	for _, want := range []string{"localhost", "127.0.0.1", "::1", "192.0.2.10"} {
		found := false
		for _, host := range hosts {
			if host == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allowed Host names omit %q: %v", want, hosts)
		}
	}
}

func TestAllowedMediaHostsRequireHostnameOrIP(t *testing.T) {
	for raw, want := range map[string]string{
		"media.tailnet.example":  "media.tailnet.example",
		"MEDIA.TAILNET.EXAMPLE.": "media.tailnet.example",
		"192.0.2.5":              "192.0.2.5",
		"[2001:db8::1]":          "2001:db8::1",
	} {
		if got, err := parseAllowedHost(raw); err != nil || got != want {
			t.Errorf("parseAllowedHost(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "https://media.example", "media.example:8765", "bad host", "-invalid.example"} {
		if _, err := parseAllowedHost(raw); err == nil {
			t.Errorf("parseAllowedHost(%q) accepted invalid host", raw)
		}
	}
}

func TestUsageAndCompletionListMediaAndSkill(t *testing.T) {
	var out, errOut bytes.Buffer
	printUsage(&out)
	for _, command := range []string{"tailge media serve DIRECTORY", "tailge skill install", "tailge skill uninstall --confirm"} {
		if !strings.Contains(out.String(), command) {
			t.Errorf("usage omits %q: %s", command, out.String())
		}
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out.Reset()
		if code := completion([]string{shell}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "media") || !strings.Contains(out.String(), "skill") {
			t.Errorf("%s completion omits new commands: code=%d output=%q", shell, code, out.String())
		}
	}
}

func TestSkillInstallCommandWritesIntoCurrentProject(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	application := app{}
	var stdout, stderr bytes.Buffer
	if code := application.run([]string{"skill", "install"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("skill command failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	path := filepath.Join(project, ".agents", "skills", "tailge", "SKILL.md")
	if contents, err := os.ReadFile(path); err != nil || string(contents) != tailgeAgentSkill {
		t.Fatalf("skill file not installed: err=%v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := application.run([]string{"skill", "install"}, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("existing skill was not preserved: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := application.run([]string{"skill", "uninstall"}, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "requires --confirm") {
		t.Fatalf("unconfirmed uninstall was not refused: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := application.run([]string{"skill", "uninstall", "--confirm"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("confirmed uninstall failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("confirmed uninstall left the skill file: err=%v", err)
	}
	if err := os.WriteFile(path, []byte("customized skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := application.run([]string{"skill", "uninstall", "--confirm"}, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "modified skill file") {
		t.Fatalf("custom skill was removed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestEnsureSkillDirectoryRefusesSymlinkParents(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := os.Symlink(outside, ".agents"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outsideSkill := filepath.Join(outside, "skills", "tailge", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(outsideSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideSkill, []byte("generated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureSkillDirectory(filepath.Join(".agents", "skills", "tailge")); err == nil {
		t.Fatal("accepted a symlinked project skill directory")
	}
	if _, err := uninstallSkill(filepath.Join(".agents", "skills", "tailge", "SKILL.md"), []byte("generated")); err == nil {
		t.Fatal("uninstall accepted a symlinked project skill directory")
	}
	if contents, err := os.ReadFile(outsideSkill); err != nil || string(contents) != "generated" {
		t.Fatalf("uninstall changed file outside the project root: contents=%q err=%v", contents, err)
	}
}

func TestSkillDocumentIsAgentInvocableAndIncludesMediaSafety(t *testing.T) {
	for _, want := range []string{
		"name: tailge",
		"description:",
		"tailge media serve DIRECTORY",
		"127.0.0.1:8765",
		"--confirm-public",
		"tailge skill uninstall --confirm",
		"selected directory trees",
	} {
		if !strings.Contains(tailgeAgentSkill, want) {
			t.Errorf("generated agent skill omits %q", want)
		}
	}
}
