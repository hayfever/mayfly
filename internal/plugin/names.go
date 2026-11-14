package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode"
)

const (
	containerPrefix = "plugin-"
	pluginDir       = "plugins"
)

// ToolName is the model-visible name of one plugin tool. The shape matches
// mcp_<server>_<tool>, and it does not depend on the placement, so moving a
// plugin between placements keeps prompts, transcripts, and journal steps.
func ToolName(name, tool string) string {
	return "plugin_" + sanitize(name) + "_" + sanitize(tool)
}

// sanitize keeps letters and digits and turns everything else into "_",
// matching internal/mcp.sanitize so both tool families read alike.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unnamed"
	}
	return out
}

// IsManifestPath classifies one slash-separated path relative to a bundle
// directory. It reports the manifest name when the path is a plugin manifest,
// and skip when the path is plugin source that the bundle loader must ignore.
//
// Only the top-level plugins/ directory of a bundle entry is special. The
// flat <name>.plugin.yaml form is accepted anywhere else in the bundle, for
// ConfigMap bundles whose keys cannot hold a directory.
func IsManifestPath(rel string) (name string, manifest, skip bool) {
	rel = path.Clean(rel)
	if rest, ok := strings.CutPrefix(rel, pluginDir+"/"); ok {
		if dir, file, nested := strings.Cut(rest, "/"); nested {
			if file == ManifestFile {
				return dir, true, false
			}
			return "", false, true
		}
	}
	base := path.Base(rel)
	if flat, ok := strings.CutSuffix(base, FlatSuffix); ok && flat != "" {
		return flat, true, false
	}
	return "", false, false
}

// ContainerName is the sidecar container name: a DNS label.
func ContainerName(name string) string {
	return dnsName(containerPrefix, name)
}

// ObjectName is the name of the Deployment, Service, NetworkPolicy, and
// Secret that mayflyd reconciles for one service plugin: a DNS-1035 label,
// which must start with a letter.
func ObjectName(name string) string {
	return dnsName(SecretPrefix, name)
}

// dnsName maps a plugin name onto a DNS label. A name that does not fit keeps
// a hash suffix, so two long names cannot truncate to the same object.
func dnsName(prefix, name string) string {
	label := prefix + mappedName(name)
	if len(label) <= dnsMaxLength {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	suffix := hex.EncodeToString(sum[:])[:nameHashLength]
	keep := dnsMaxLength - len(suffix) - 1
	return strings.TrimRight(label[:keep], "-") + "-" + suffix
}

// mappedName is the plugin name as a label component: lowercase, with dots
// and underscores turned into hyphens.
func mappedName(name string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(name))
}
