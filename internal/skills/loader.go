package skills

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

// Options tune a Load.
type Options struct {
	// CacheDir holds verified source archives named by digest. An empty value
	// disables the cache.
	CacheDir string
	// Logger receives one cache or fetch line per source, plus warnings. A nil
	// logger discards them.
	Logger *slog.Logger
}

// Load reads local directories and remote sources. Duplicate skill names are
// rejected across every source.
func Load(dirs []string, sources []Source, opts Options) ([]sessioncfg.Skill, error) {
	loader := &skillLoader{seen: map[string]string{}, cacheDir: opts.CacheDir, logger: opts.Logger}
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
	if loader.cacheDir != "" {
		referenced := make(map[string]bool, len(sources))
		for _, source := range sources {
			if validDigest(source.SHA256) {
				referenced[strings.ToLower(source.SHA256)] = true
			}
		}
		if err := pruneCache(loader.cacheDir, referenced); err != nil {
			loader.logWarn("skill cache prune failed", "dir", loader.cacheDir, "error", err)
		}
	}
	sort.Slice(loader.out, func(i, j int) bool { return loader.out[i].Name < loader.out[j].Name })
	return loader.out, nil
}

// LoadDirs reads every Markdown skill under the given directories.
func LoadDirs(dirs []string) ([]sessioncfg.Skill, error) {
	return Load(dirs, nil, Options{})
}

type skillLoader struct {
	out      []sessioncfg.Skill
	seen     map[string]string
	cacheDir string
	logger   *slog.Logger
}

func (l *skillLoader) logInfo(msg string, args ...any) {
	if l.logger != nil {
		l.logger.Info(msg, args...)
	}
}

func (l *skillLoader) logWarn(msg string, args ...any) {
	if l.logger != nil {
		l.logger.Warn(msg, args...)
	}
}

func (l *skillLoader) add(skill sessioncfg.Skill, origin string) error {
	if prev, ok := l.seen[skill.Name]; ok {
		return fmt.Errorf("duplicate skill name %q in %s and %s", skill.Name, prev, origin)
	}
	l.seen[skill.Name] = origin
	l.out = append(l.out, skill)
	return nil
}
