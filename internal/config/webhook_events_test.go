package config

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/jsonpath"
)

// githubEventBodies are the skeletons of GitHub event bodies: the keys each
// event's payload carries at the levels the preset shapes read. Gitea models
// its payloads on these.
var githubEventBodies = map[string]string{
	"push":                        `{"ref":"r","before":"a","after":"b","pusher":{},"commits":[],"repository":{"id":1}}`,
	"pull_request":                `{"action":"opened","number":1,"pull_request":{"number":1,"head":{"sha":"s"}},"repository":{"id":1}}`,
	"pull_request_review":         `{"action":"submitted","review":{"id":1},"pull_request":{"number":1,"head":{"sha":"s"}},"repository":{"id":1}}`,
	"pull_request_review_comment": `{"action":"created","comment":{"id":1,"pull_request_review_id":1},"pull_request":{"number":1,"head":{"sha":"s"}},"repository":{"id":1}}`,
	"pull_request_review_thread":  `{"action":"resolved","thread":{"comments":[{"id":1}]},"pull_request":{"number":1,"head":{"sha":"s"}},"repository":{"id":1}}`,
	"issues":                      `{"action":"opened","issue":{"number":1},"repository":{"id":1}}`,
	"issue_comment":               `{"action":"created","issue":{"number":1,"pull_request":{"url":"u"}},"comment":{"id":1},"repository":{"id":1}}`,
	"commit_comment":              `{"action":"created","comment":{"id":1,"commit_id":"c"},"repository":{"id":1}}`,
	"discussion_comment":          `{"action":"created","discussion":{"number":1},"comment":{"id":1},"repository":{"id":1}}`,
	"create":                      `{"ref":"r","ref_type":"branch","master_branch":"main","pusher_type":"user","repository":{"id":1}}`,
	"delete":                      `{"ref":"r","ref_type":"branch","pusher_type":"user","repository":{"id":1}}`,
	"release":                     `{"action":"published","release":{"id":1},"repository":{"id":1}}`,
	"workflow_run":                `{"action":"completed","workflow_run":{"id":1,"head_sha":"s"},"workflow":{"id":1},"repository":{"id":1}}`,
	"workflow_job":                `{"action":"completed","workflow_job":{"id":1,"run_id":1},"repository":{"id":1}}`,
	"check_suite":                 `{"action":"completed","check_suite":{"id":1,"head_sha":"s"},"repository":{"id":1}}`,
	"check_run":                   `{"action":"completed","check_run":{"id":1,"check_suite":{"id":1}},"repository":{"id":1}}`,
	"status":                      `{"id":1,"sha":"s","state":"success","commit":{"sha":"s"},"repository":{"id":1}}`,
	"merge_group":                 `{"action":"checks_requested","merge_group":{"head_sha":"s","head_ref":"r"},"repository":{"id":1}}`,
	"star":                        `{"action":"created","starred_at":"t","repository":{"id":1}}`,
	"fork":                        `{"forkee":{"id":2},"repository":{"id":1}}`,
	"ping":                        `{"zen":"z","hook_id":1,"hook":{"id":1},"repository":{"id":1}}`,
}

func bodyHasShape(t *testing.T, body string, shape []string) bool {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range shape {
		segs, err := jsonpath.Parse(p)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := jsonpath.Eval(doc, segs); !ok || v == nil {
			return false
		}
	}
	return true
}

// The header naming an event is not signed, so a preset's shape is what
// keeps a signed body of one event from counting as another. A shape lets
// through any body holding its paths, listed event or not; so no preset may
// give a shape that another event's body satisfies, and each preset event's
// own body must satisfy its shape.
func TestWebhookProviders_NoOtherEventsBodyHasAPresetShape(t *testing.T) {
	// The instrument can fail: the issues shape this rule keeps out of the
	// presets is satisfied by an issue_comment body.
	if !bodyHasShape(t, githubEventBodies["issue_comment"], []string{"$.action", "$.issue.number", "$.repository.id"}) {
		t.Fatal("positive control: an issue_comment body should hold every path of an issues shape")
	}
	for _, provider := range []string{"github", "gitea"} {
		p, _ := WebhookProviderPreset(provider)
		events := make([]string, 0, len(p.Shapes))
		for e := range p.Shapes {
			events = append(events, e)
		}
		sort.Strings(events)
		for _, e := range events {
			own, ok := githubEventBodies[e]
			if !ok {
				t.Errorf("%s: no sample body for %s; add one so its shape is checked", provider, e)
				continue
			}
			if !bodyHasShape(t, own, p.Shapes[e]) {
				t.Errorf("%s: %s's own body lacks its shape %v", provider, e, p.Shapes[e])
			}
			for other, body := range githubEventBodies {
				if other != e && bodyHasShape(t, body, p.Shapes[e]) {
					t.Errorf("%s: a %s body passes as %s (shape %v)", provider, other, e, p.Shapes[e])
				}
			}
		}
	}
}
