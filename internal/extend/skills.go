package extend

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	skillsDirName = "skills"
	skillFileName = "SKILL.md"
	// skillHeaderLimit bounds the bytes read from each skill file at load
	// time. The body can be large; the model reads it only when it needs it.
	skillHeaderLimit = 4 << 10
)

// Skill is a set of instructions in a file that the model reads on demand.
type Skill struct {
	Name, Description string
	// Path is absolute, so the model can read it from any directory.
	Path string
}

// LoadSkills reads the front matter of skills/<name>/SKILL.md in the user
// directory and in <workDir>/.inkling. The result is sorted by Name.
func LoadSkills(workDir string) ([]Skill, error) {
	userDir, err := UserDir()
	if err != nil {
		return nil, err
	}
	return loadSkills(userDir, workDir)
}

func loadSkills(userDir, workDir string) ([]Skill, error) {
	byName := map[string]Skill{}
	for _, dir := range layerDirs(userDir, workDir) {
		skillsDir := filepath.Join(dir, skillsDirName)
		entries, err := readDirIfExists(skillsDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			// A symbolic link can point to a skill directory, so only
			// regular files are skipped.
			if entry.Type().IsRegular() {
				continue
			}
			path, err := filepath.Abs(filepath.Join(skillsDir, entry.Name(), skillFileName))
			if err != nil {
				return nil, fmt.Errorf("extend: resolve %s: %w", entry.Name(), err)
			}
			skill, err := readSkill(path, entry.Name())
			if err != nil {
				return nil, err
			}
			byName[skill.Name] = skill
		}
	}
	skills := make([]Skill, 0, len(byName))
	for _, skill := range byName {
		skills = append(skills, skill)
	}
	slices.SortFunc(skills, func(left, right Skill) int {
		return strings.Compare(left.Name, right.Name)
	})
	return skills, nil
}

func readSkill(path, directoryName string) (Skill, error) {
	file, err := os.Open(path)
	if err != nil {
		return Skill{}, fmt.Errorf("extend: open %s: %w", path, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, skillHeaderLimit), skillHeaderLimit)
	fields, err := readFrontMatter(reader, "name", "description")
	if err != nil {
		return Skill{}, fmt.Errorf("extend: %s: %w (the front matter must end in the first %d bytes)", path, err, skillHeaderLimit)
	}
	name, description := fields["name"], fields["description"]
	switch {
	case name == "":
		return Skill{}, fmt.Errorf("extend: %s: front matter has no name", path)
	case description == "":
		return Skill{}, fmt.Errorf("extend: %s: front matter has no description", path)
	case name != directoryName:
		return Skill{}, fmt.Errorf("extend: %s: name %q is not the directory name %q", path, name, directoryName)
	case !isValidName(name):
		return Skill{}, fmt.Errorf("extend: %s: skill name %q must match [a-z0-9][a-z0-9_-]*", path, name)
	}
	return Skill{Name: name, Description: description, Path: path}, nil
}

// SkillsPrompt lists the skills for the system prompt. The text depends only
// on the skills, so the prompt prefix stays stable for caching.
func SkillsPrompt(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var prompt strings.Builder
	prompt.WriteString("# Skills\n\nThese skills are available. Before you use a skill, read its file with read_file and follow it.\n\n")
	for _, skill := range skills {
		fmt.Fprintf(&prompt, "- %s: %s (%s)\n", skill.Name, skill.Description, skill.Path)
	}
	return prompt.String()
}
