package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A config naming what v1.172.0 removed does not load (RFC GW P15). doctor
// says so under "Removed", one FAIL per item with its migration line, from
// the load's own error: the variable refusal and the agent refusal alike.
func TestDoctor_RemovedListsEachUseOfARefusedLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		yaml string
		want []string
	}{
		"agent": {
			yaml: "defaults:\n  provider: mock\n  model: mock\nagents:\n  coder:\n    tools: [Read, Bash]\n    shell_credentials: [cred-91c2]\n",
			want: []string{`agent "coder" in t.yaml: tools names Bash`, `agent "coder" in t.yaml sets shell_credentials`},
		},
		"variable": {
			env:  "LOOMCYCLE_BASH_ALLOWED_CREDS",
			yaml: "defaults:\n  provider: mock\n  model: mock\n",
			want: []string{"LOOMCYCLE_BASH_ALLOWED_CREDS: removed in " + config.RemovedIn},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, "cred-91c2")
			}
			_, err := config.LoadLayers(config.Layer{Name: "t.yaml", Data: []byte(tc.yaml)})
			if err == nil {
				t.Fatal("the load passed")
			}
			var out bytes.Buffer
			r := &doctorRun{stdout: &out}
			reportRemoved(r, err)
			if r.failCount != len(tc.want) {
				t.Errorf("%d Removed FAILs, want %d:\n%s", r.failCount, len(tc.want), out.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("no Removed line %q in:\n%s", w, out.String())
				}
			}
			if strings.Contains(out.String(), "cred-91c2") {
				t.Errorf("a value reached doctor's output:\n%s", out.String())
			}
		})
	}
}

// A load error about anything else adds no Removed line.
func TestDoctor_RemovedSaysNothingOfAnUnrelatedLoadError(t *testing.T) {
	var out bytes.Buffer
	r := &doctorRun{stdout: &out}
	reportRemoved(r, errString("agent \"x\": unknown tool Foo | another"))
	if r.failCount != 0 {
		t.Errorf("unrelated error reported as Removed:\n%s", out.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }
