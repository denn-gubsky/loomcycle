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

// envValue is name's value in env and whether it is set at all.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return kv[len(name)+1:], true
		}
	}
	return "", false
}

// RFC EW D6: a credential name the agent's shell_credentials narrowed away
// reaches the Bashbox fallback from neither source. The operator's host
// allowlist admits GITHUB_TOKEN from the host environment too; an agent that
// gave the name up gets neither the stored credential nor the host variable,
// for every command (the agent narrowed the name, not a binding). A host
// variable that is not one of the operator's shell credentials (LANG) is
// untouched, and an agent that did not narrow keeps today's behaviour.
func TestBashboxFallback_ShellCredentialsWithholdsTheSameNamedHostVar(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "host-value")
	t.Setenv("LANG", "C.UTF-8")
	for _, c := range []struct {
		name     string
		resolve  CredEnvResolver
		declared *config.CredentialNames
		command  string
		want     string // GITHUB_TOKEN's value, "" when it must be unset
	}{
		{"absent: git gets the host variable, as before", shellCredResolver, nil, "git", "host-value"},
		{"absent: gh gets its stored credential over the host one", shellCredResolver, nil, "gh", "v-GITHUB_TOKEN"},
		{"empty: git gets neither", shellCredResolver, names(), "git", ""},
		{"empty: gh gets neither", shellCredResolver, names(), "gh", ""},
		{"keeps another name: git gets neither", shellCredResolver, names("NPM_TOKEN"), "git", ""},
		{"keeps it: git gets the host variable", shellCredResolver, names("GITHUB_TOKEN"), "git", "host-value"},
		{"keeps it: gh gets its stored credential", shellCredResolver, names("GITHUB_TOKEN"), "gh", "v-GITHUB_TOKEN"},
		{"no credential resolver: still withheld", nil, names(), "git", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &Bashbox{
				FallbackAllowedEnv:   []string{"GITHUB_TOKEN", "LANG"},
				FallbackAllowedCreds: []string{"gh:GITHUB_TOKEN", "NPM_TOKEN"},
				CredResolve:          c.resolve,
			}
			env, _ := b.fallbackEnv(withShellCreds(c.declared), c.command)
			got, set := envValue(env, "GITHUB_TOKEN")
			if c.want == "" && set {
				t.Errorf("GITHUB_TOKEN reached the command (%q); the agent narrowed it away", got)
			}
			if c.want != "" && got != c.want {
				t.Errorf("GITHUB_TOKEN = %q, want %q", got, c.want)
			}
			if v, _ := envValue(env, "LANG"); v != "C.UTF-8" {
				t.Errorf("LANG = %q; a host variable that is not a shell credential must pass", v)
			}
		})
	}
}

// The same for the Bash tool, whose host allowlist is the same mechanism.
func TestBash_ShellCredentialsWithholdsTheSameNamedHostVar(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "host-value")
	b := &Bash{AllowedExtraEnv: []string{"GITHUB_TOKEN"}, AllowedCreds: []string{"GITHUB_TOKEN"}}
	env, _ := b.buildEnv(withShellCreds(nil))
	if v, _ := envValue(env, "GITHUB_TOKEN"); v != "host-value" {
		t.Errorf("absent: GITHUB_TOKEN = %q, want the host variable as before", v)
	}
	env, _ = b.buildEnv(withShellCreds(names()))
	if _, set := envValue(env, "GITHUB_TOKEN"); set {
		t.Error("empty: GITHUB_TOKEN reached the command; the agent narrowed it away")
	}
}
