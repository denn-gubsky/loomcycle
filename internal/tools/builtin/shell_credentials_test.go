package builtin

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// shellCredResolver resolves every name to "v-<name>", as a run's stored
// credentials would.
func shellCredResolver(_ context.Context, name string) (string, bool, error) {
	return "v-" + name, true, nil
}

// withShellCreds is a run ctx whose agent declared shell_credentials (nil:
// absent).
func withShellCreds(declared *config.CredentialNames) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme"})
	return tools.WithRunPolicy(ctx, tools.RunPolicy{ShellCreds: tools.ShellCredsFor(declared)})
}

func names(list ...string) *config.CredentialNames { c := config.CredentialNames(list); return &c }

// injected lists the credential names in env (those the resolver produced).
func injected(env []string) []string {
	var out []string
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.HasPrefix(kv[i+1:], "v-") {
			out = append(out, kv[:i])
		}
	}
	sort.Strings(out)
	return out
}

// RFC EW: an agent's shell_credentials keeps only some of the operator's
// credentials in a Bash command's environment. Absent keeps them all, as
// before; [] keeps none; a name the operator did not list adds nothing.
func TestBash_ShellCredentialsNarrowsTheInjectedSet(t *testing.T) {
	b := &Bash{AllowedCreds: []string{"A", "B"}, CredResolve: shellCredResolver}
	for _, c := range []struct {
		name     string
		declared *config.CredentialNames
		want     string
	}{
		{"absent: every credential the operator lists", nil, "A,B"},
		{"one name: only it", names("B"), "B"},
		{"empty: none", names(), ""},
		{"a name the operator did not list: nothing", names("C"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, _ := b.buildEnv(withShellCreds(c.declared))
			if got := strings.Join(injected(env), ","); got != c.want {
				t.Errorf("injected %q, want %q", got, c.want)
			}
		})
	}
}

// The Bashbox fallback narrows the same way, and an operator's "cmd:NAME"
// binding stays bound to its command: an agent keeping A gives A to git
// (which the operator bound it to) and nothing to gh.
func TestBashboxFallback_ShellCredentialsKeepsTheCommandBinding(t *testing.T) {
	b := &Bashbox{FallbackAllowedCreds: []string{"git:A", "B"}, CredResolve: shellCredResolver}
	for _, c := range []struct {
		name     string
		declared *config.CredentialNames
		command  string
		want     string
	}{
		{"absent: git gets its binding and the bare name", nil, "git", "A,B"},
		{"absent: gh gets the bare name", nil, "gh", "B"},
		{"keeps A: git gets A", names("A"), "git", "A"},
		{"keeps A: gh gets nothing, A is git's", names("A"), "gh", ""},
		{"empty: git gets none", names(), "git", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, _ := b.fallbackEnv(withShellCreds(c.declared), c.command)
			if got := strings.Join(injected(env), ","); got != c.want {
				t.Errorf("injected %q, want %q", got, c.want)
			}
		})
	}
}
