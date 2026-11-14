package sessioncfg

import (
	"os"
	"sort"
	"strings"
)

// DefaultPath is the PATH a child process gets when the runtime has none.
const DefaultPath = "/usr/local/bin:/usr/bin:/bin"

// passthroughEnv lists the parent variables a child process of the session
// keeps unchanged: the locale, the timezone, the TLS trust store, and the proxy
// settings of the container. Tools read the upper and the lower case form, so a
// name the parent sets in both forms is inherited in both.
var passthroughEnv = []string{
	"LANG",
	"TZ",
	"SSL_CERT_FILE",
	"SSL_CERT_DIR",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"NO_PROXY",
}

// toolchainEnv lists the parent variables a stdio MCP server needs to find and
// run its command: PATH, a HOME for its caches, and TMPDIR.
var toolchainEnv = []string{"PATH", "HOME", "TMPDIR"}

// StdioEnv returns the complete environment of a stdio MCP server: the
// toolchain and passthrough variables of the runtime, then extra.
//
// Nothing else is inherited, so the server cannot read MAYFLY_SESSION_TOKEN,
// MAYFLY_CONFIG, a provider key, or a webhook secret from the runtime. The
// session token authorizes the tool gateway, so handing it to an MCP server
// would let that server dispatch calls of the session.
func StdioEnv(extra map[string]string) []string {
	values := make(map[string]string, len(toolchainEnv)+2*len(passthroughEnv)+len(extra))
	inherit(values, toolchainEnv)
	if _, ok := values["PATH"]; !ok {
		values["PATH"] = DefaultPath
	}
	for name, value := range PassthroughEnv() {
		values[name] = value
	}
	for name, value := range extra {
		values[name] = value
	}
	return envList(values)
}

// PassthroughEnv returns the parent's values for the passthrough variables, in
// both the upper and the lower case form of each name. A child that builds the
// rest of its environment itself, such as an inline plugin with its own PATH,
// HOME, and TMPDIR, uses this.
func PassthroughEnv() map[string]string {
	values := make(map[string]string, 2*len(passthroughEnv))
	inherit(values, passthroughEnv)
	return values
}

// inherit copies the parent's values for names into values, adding the lower
// case form of each name the parent also sets.
func inherit(values map[string]string, names []string) {
	for _, name := range names {
		for _, key := range []string{name, strings.ToLower(name)} {
			if value, ok := os.LookupEnv(key); ok {
				values[key] = value
			}
		}
	}
}

// envList renders values as name=value pairs, sorted by name.
func envList(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+values[name])
	}
	return out
}
