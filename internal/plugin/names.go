package plugin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"unicode"
)

const (
	containerPrefix = "plugin-"
	pluginDir       = "plugins"
	// gatewayKeyPrefix names the Secret that holds a service plugin's
	// call-token key. It must not use SecretPrefix: a bundle's own secret named
	// mayfly-plugin-<name> must never resolve to the key that signs calls.
	gatewayKeyPrefix = "mayfly-gateway-"
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

// ObjectName is the name of the Deployment, Service, and NetworkPolicy that
// mayflyd reconciles for one service plugin: a DNS-1035 label, which must start
// with a letter.
func ObjectName(name string) string {
	return dnsName(SecretPrefix, name)
}

// GatewayKeyName is the name of the Secret that holds one service plugin's
// call-token key. The prefix differs from SecretPrefix on purpose: a bundle
// cannot reference this Secret as a plugin secret, and pruning the plugin
// cannot delete an operator's credential.
func GatewayKeyName(name string) string {
	return dnsName(gatewayKeyPrefix, name)
}

// SidecarKey derives the call-token key of one sidecar plugin from the
// session's random key. One key per plugin keeps a sidecar's own key file from
// minting a token that a sibling accepts.
func SidecarKey(sessionKey []byte, name string) []byte {
	mac := hmac.New(sha256.New, sessionKey)
	mac.Write([]byte("mayfly/sidecar/" + name))
	return mac.Sum(nil)
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
