package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// credentialShapedSubstrings is gc's OWN vocabulary for a sensitive env name,
// copied deliberately rather than invented: internal/execenv's IsSensitiveKey
// drops any inherited key CONTAINING one of these from a session's
// environment. Teaching a different rule here would mean the pack advises on
// one definition of "credential" while the platform enforces another.
var credentialShapedSubstrings = []string{
	"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL",
	"OAUTH", "PRIVATE_KEY", "API_KEY", "ACCESS_KEY", "AUTH_JSON",
}

// countingNotCredential are the substrings where a credential word is being
// used as a UNIT, not as a secret.
//
// MEASURED, by this file's own test failing on a real key from a real city
// config: GC_CONTEXT_WINDOW_TOKENS contains "TOKEN" and is a context-size
// number. gc's IsSensitiveKey would drop it from inherited env on the same
// match, which is its business; what matters here is that WARNING about it
// would be noise, and a warning that cries wolf on a context-window setting
// stops being read before it ever sees a real API key.
//
// So this narrows gc's vocabulary rather than replacing it, and only in the
// direction of saying less. The plural is the signal: a credential is a
// TOKEN, a budget is TOKENS.
var countingNotCredential = []string{
	"TOKENS", "TOKEN_LIMIT", "TOKEN_COUNT", "TOKEN_BUDGET", "CONTEXT_WINDOW",
}

// credentialShapedName reports whether a name looks like it carries a secret.
func credentialShapedName(key string) bool {
	upper := strings.ToUpper(key)
	for _, benign := range countingNotCredential {
		if strings.Contains(upper, benign) {
			return false
		}
	}
	for _, s := range credentialShapedSubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

// warnCredentialShapedNames names the declared keys that look like credentials
// and are NOT routed to a file.
//
// WHY IT EXISTS. A security reviewer with only the shipped docs refused to
// approve this pack for real credentials, and one of their findings was exact:
// "No enforcement or even a lint warning ties a credential-shaped
// GC_NOMAD_EXTRA_SECRET_KEYS name to the `_PEM` suffix. The docs say plainly
// 'the name you choose decides which one you get... it is not reversible after
// the fact'."
//
// The routing is real and it is decided by a string: a `_PEM` suffix stages
// the value as a FILE and hands the agent a path, while any other name exports
// the value into the agent's process environment, where it is readable in
// /proc/<pid>/environ by the agent and everything it spawns. The pack knew the
// rule and never mentioned which side of it a given deployment had landed on.
//
// ADVISORY, NOT A REFUSAL, and that is a judgement rather than timidity: the
// pack cannot know that MY_API_TOKEN holds a credential and not a feature flag
// named after one, and refusing a launch on a name would break working
// deployments over a guess. Saying it once, at the layer that owns the routing
// rule, is the honest amount of opinion to have.
func warnCredentialShapedNames(w io.Writer, sessionName string, declared []string) {
	if w == nil {
		return
	}
	var flagged []string
	for _, k := range declared {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		if _, isPointer := pemPointerName(key); isPointer {
			// Already routed to a file; nothing to say.
			continue
		}
		if credentialShapedName(key) {
			flagged = append(flagged, key)
		}
	}
	if len(flagged) == 0 {
		return
	}
	sort.Strings(flagged)
	fmt.Fprintf(w, "warning: session %q stages %d credential-shaped key(s) into the agent's PROCESS ENVIRONMENT, "+
		"not as files: %s. Those values are readable in /proc/<pid>/environ by the agent and every process it "+
		"spawns. Renaming a key to end in _PEM routes it to a file under $NOMAD_SECRETS_DIR instead and hands "+
		"the agent a PATH, so the bytes never enter an environment — and the name is what decides this, which "+
		"is not reversible after the value has already been staged under the other name. If these are feature "+
		"flags rather than credentials, the name is the only thing misleading here and this warning is the "+
		"cost of that.\n",
		sessionName, len(flagged), strings.Join(flagged, ", "))
}
