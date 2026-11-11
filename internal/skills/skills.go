// Package skills loads Markdown skill files with optional YAML frontmatter.
//
// A skill file looks like:
//
//	---
//	name: incident-triage
//	description: How to triage a production incident
//	---
//	# Instructions
//	...
package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hayden/mayfly/internal/sessioncfg"
)

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func (l *skillLoader) loadDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("skill directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill path %s is not a directory", dir)
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".md" && ext != ".markdown" {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		skill, err := parseSkill(raw, baseName(p), p)
		if err != nil {
			return err
		}
		return l.add(skill, p)
	})
	if err != nil {
		return err
	}
	return nil
}

func baseName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func parseSkill(raw []byte, nameHint, origin string) (sessioncfg.Skill, error) {
	meta, body, err := splitFrontmatter(raw)
	if err != nil {
		return sessioncfg.Skill{}, fmt.Errorf("%s: %w", origin, err)
	}
	if meta.Name == "" {
		meta.Name = nameHint
	}
	if strings.TrimSpace(body) == "" {
		return sessioncfg.Skill{}, fmt.Errorf("%s: skill body is empty", origin)
	}
	return sessioncfg.Skill{
		Name:        meta.Name,
		Description: meta.Description,
		Body:        strings.TrimSpace(body),
	}, nil
}

func splitFrontmatter(raw []byte) (frontmatter, string, error) {
	var meta frontmatter
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return meta, text, nil
	}
	start := strings.IndexByte(text, '\n') + 1
	offset := start
	for offset <= len(text) {
		next := strings.IndexByte(text[offset:], '\n')
		lineEnd := len(text)
		line := text[offset:]
		if next >= 0 {
			line = text[offset : offset+next]
			lineEnd = offset + next + 1
		}
		if strings.TrimSuffix(line, "\r") == "---" {
			if err := yaml.Unmarshal([]byte(text[start:offset]), &meta); err != nil {
				return meta, "", fmt.Errorf("invalid frontmatter: %w", err)
			}
			return meta, text[lineEnd:], nil
		}
		if next < 0 {
			break
		}
		offset = lineEnd
	}
	return meta, "", errors.New("unterminated frontmatter")
}
