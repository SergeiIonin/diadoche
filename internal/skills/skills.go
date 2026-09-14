// Package skills resolves which .claude/skills subfolders are relevant to a
// run, based on --langs/--frameworks. The actual skill content is consumed by
// the nested claude CLI process itself (standard Claude Code skill
// discovery); entelecheia only needs to tell agents which ones apply.
package skills

import (
	"errors"
	"os"
	"strings"
)

// Resolve lists skill directory names under skillsDir matching any of
// langs/frameworks (substring, case-insensitive). If both are empty, it
// returns every skill directory present, so a user who just drops skills in
// .claude/skills without CLI flags still gets them all surfaced.
func Resolve(skillsDir string, langs, frameworks []string) ([]string, error) {
	entries, err := os.ReadDir(skillsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	wanted := make([]string, 0, len(langs)+len(frameworks))
	wanted = append(wanted, langs...)
	wanted = append(wanted, frameworks...)

	var matched []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if len(wanted) == 0 {
			matched = append(matched, e.Name())
			continue
		}
		name := strings.ToLower(e.Name())
		for _, w := range wanted {
			if strings.Contains(name, strings.ToLower(w)) {
				matched = append(matched, e.Name())
				break
			}
		}
	}
	return matched, nil
}
