package extend

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func skillFile(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# Body\n"
}

func TestSkillsProjectOverridesUserAndSortByName(t *testing.T) {
	dirs := newLayout(t)
	userSkills := filepath.Join(dirs.userDir, skillsDirName)
	projectSkills := filepath.Join(dirs.projectDir(), skillsDirName)
	writeFile(t, filepath.Join(userSkills, "release", skillFileName), skillFile("release", "User release steps"))
	writeFile(t, filepath.Join(userSkills, "zebra", skillFileName), skillFile("zebra", "Last skill"))
	writeFile(t, filepath.Join(userSkills, "README.md"), "a regular file is not a skill")
	writeFile(t, filepath.Join(projectSkills, "release", skillFileName), skillFile("release", "'Project release steps'"))
	writeFile(t, filepath.Join(projectSkills, "api-docs", skillFileName), skillFile("api-docs", "Write API docs"))

	skills, err := loadSkills(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Skill{
		{Name: "api-docs", Description: "Write API docs", Path: filepath.Join(projectSkills, "api-docs", skillFileName)},
		{Name: "release", Description: "Project release steps", Path: filepath.Join(projectSkills, "release", skillFileName)},
		{Name: "zebra", Description: "Last skill", Path: filepath.Join(userSkills, "zebra", skillFileName)},
	}
	if !reflect.DeepEqual(skills, want) {
		t.Fatalf("skills =\n%+v\nwant\n%+v", skills, want)
	}
	for _, skill := range skills {
		if !filepath.IsAbs(skill.Path) {
			t.Fatalf("path %q is not absolute", skill.Path)
		}
	}
}

func TestSkillPathIsAbsoluteForRelativeWorkDir(t *testing.T) {
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, projectDirName, skillsDirName, "lint", skillFileName), skillFile("lint", "Run the linter"))
	t.Chdir(workDir)
	skills, err := loadSkills(t.TempDir(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || !filepath.IsAbs(skills[0].Path) {
		t.Fatalf("skills = %+v, want one skill with an absolute path", skills)
	}
}

func TestSkillFileErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"no front matter", "# Release\n", "does not start with ---"},
		{"missing name", "---\ndescription: x\n---\n", "has no name"},
		{"missing description", "---\nname: release\n---\n", "has no description"},
		{"name is not directory name", skillFile("deploy", "x"), `name "deploy" is not the directory name "release"`},
		{"unknown key", "---\nname: release\ndescription: x\nversion: 2\n---\n", `unknown front matter key "version"`},
		{"invalid UTF-8 description", "---\nname: release\ndescription: \xff\xfe\n---\n", "not valid UTF-8"},
		{"front matter longer than the limit", "---\nname: release\ndescription: " + strings.Repeat("x", skillHeaderLimit) + "\n---\n", "no closing ---"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dirs := newLayout(t)
			path := filepath.Join(dirs.userDir, skillsDirName, "release", skillFileName)
			writeFile(t, path, test.content)
			_, err := loadSkills(dirs.userDir, dirs.workDir)
			wantErrorContaining(t, err, path, test.want)
		})
	}

	t.Run("skill directory without SKILL.md", func(t *testing.T) {
		dirs := newLayout(t)
		if err := os.MkdirAll(filepath.Join(dirs.userDir, skillsDirName, "empty"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := loadSkills(dirs.userDir, dirs.workDir)
		wantErrorContaining(t, err, filepath.Join("empty", skillFileName))
	})

	t.Run("invalid directory name", func(t *testing.T) {
		dirs := newLayout(t)
		writeFile(t, filepath.Join(dirs.userDir, skillsDirName, "Release", skillFileName), skillFile("Release", "x"))
		_, err := loadSkills(dirs.userDir, dirs.workDir)
		wantErrorContaining(t, err, "must match")
	})
}

// TestSkillLoadIgnoresHugeInvalidBody proves that the loader does not read
// or check the body: a body far over the limit, that is not text, loads.
func TestSkillLoadIgnoresHugeInvalidBody(t *testing.T) {
	dirs := newLayout(t)
	body := bytes.Repeat([]byte{0xff, 0xfe, 0x00}, 1<<20)
	writeFile(t, filepath.Join(dirs.userDir, skillsDirName, "big", skillFileName), skillFile("big", "Big body")+string(body))
	skills, err := loadSkills(dirs.userDir, dirs.workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Description != "Big body" {
		t.Fatalf("skills = %+v", skills)
	}
}

// TestSkillLoadStopsAfterFrontMatter uses a named pipe that sends only the
// front matter and then stays open. A loader that reads past the closing
// --- would block until the test closes the pipe.
func TestSkillLoadStopsAfterFrontMatter(t *testing.T) {
	dirs := newLayout(t)
	skillDir := filepath.Join(dirs.userDir, skillsDirName, "piped")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pipePath := filepath.Join(skillDir, skillFileName)
	if err := syscall.Mkfifo(pipePath, 0o644); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			writerDone <- err
			return
		}
		defer writer.Close()
		if _, err := writer.WriteString(skillFile("piped", "From a pipe")); err != nil {
			writerDone <- err
			return
		}
		<-release
		writerDone <- nil
	}()

	type outcome struct {
		skills []Skill
		err    error
	}
	loaded := make(chan outcome, 1)
	go func() {
		skills, err := loadSkills(dirs.userDir, dirs.workDir)
		loaded <- outcome{skills, err}
	}()
	select {
	case result := <-loaded:
		close(release)
		if err := <-writerDone; err != nil {
			t.Fatal(err)
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.skills) != 1 || result.skills[0].Description != "From a pipe" {
			t.Fatalf("skills = %+v", result.skills)
		}
	case <-time.After(5 * time.Second):
		close(release)
		<-loaded
		t.Fatal("the loader read past the closing --- and blocked on the pipe")
	}
}

func TestSkillsPrompt(t *testing.T) {
	if prompt := SkillsPrompt(nil); prompt != "" {
		t.Fatalf("SkillsPrompt(nil) = %q, want empty", prompt)
	}
	skills := []Skill{
		{Name: "api-docs", Description: "Write API docs", Path: "/a/api-docs/SKILL.md"},
		{Name: "release", Description: "Release steps", Path: "/b/release/SKILL.md"},
	}
	prompt := SkillsPrompt(skills)
	for _, line := range []string{
		"- api-docs: Write API docs (/a/api-docs/SKILL.md)\n",
		"- release: Release steps (/b/release/SKILL.md)\n",
		"read_file",
	} {
		if !strings.Contains(prompt, line) {
			t.Fatalf("prompt %q does not contain %q", prompt, line)
		}
	}
	if strings.Index(prompt, "api-docs") > strings.Index(prompt, "release:") {
		t.Fatalf("prompt does not keep the skill order: %q", prompt)
	}
	if SkillsPrompt(skills) != prompt {
		t.Fatal("prompt is not stable")
	}
}
