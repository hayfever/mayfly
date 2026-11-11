package skills

import (
	"fmt"
	"sort"
	"time"

	"github.com/hayfever/mayfly/internal/sessioncfg"
)

// Source is a pinned tar.gz archive of skills. It is normally a code hosting
// archive URL. Fetching and verification happen in the control plane.
type Source struct {
	Name            string
	URL             string
	SHA256          string
	Subdir          string
	StripComponents int
	Headers         map[string]string
	Timeout         time.Duration
}

// Load reads local directories and remote sources. Duplicate skill names are
// rejected across every source.
func Load(dirs []string, sources []Source) ([]sessioncfg.Skill, error) {
	loader := &skillLoader{seen: map[string]string{}}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := loader.loadDir(dir); err != nil {
			return nil, err
		}
	}
	for _, source := range sources {
		if err := loader.loadSource(source); err != nil {
			return nil, err
		}
	}
	sort.Slice(loader.out, func(i, j int) bool { return loader.out[i].Name < loader.out[j].Name })
	return loader.out, nil
}

// LoadDirs reads every Markdown skill under the given directories.
func LoadDirs(dirs []string) ([]sessioncfg.Skill, error) {
	return Load(dirs, nil)
}

type skillLoader struct {
	out  []sessioncfg.Skill
	seen map[string]string
}

func (l *skillLoader) add(skill sessioncfg.Skill, origin string) error {
	if prev, ok := l.seen[skill.Name]; ok {
		return fmt.Errorf("duplicate skill name %q in %s and %s", skill.Name, prev, origin)
	}
	l.seen[skill.Name] = origin
	l.out = append(l.out, skill)
	return nil
}
