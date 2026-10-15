package config

import (
	"encoding/json"
	"sort"
	"testing"
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

// giteaEventBodies are Gitea's own payloads where they differ from GitHub's:
// a comment carries is_pull, create carries the ref's sha and no
// master_branch, and the issue events beside issues send the issues body.
var giteaEventBodies = map[string]string{
	"push":                 `{"ref":"r","before":"a","after":"b","commits":[],"repository":{"id":1}}`,
	"issues":               `{"action":"opened","number":1,"issue":{"number":1},"repository":{"id":1}}`,
	"issue_label":          `{"action":"label_updated","number":1,"issue":{"number":1},"repository":{"id":1}}`,
	"issue_comment":        `{"action":"created","issue":{"number":1},"comment":{"id":1},"repository":{"id":1},"is_pull":false}`,
	"pull_request_comment": `{"action":"created","issue":{"number":1},"pull_request":{"number":1},"comment":{"id":1},"repository":{"id":1},"is_pull":true}`,
	"pull_request":         `{"action":"opened","number":1,"pull_request":{"number":1,"head":{"sha":"s"}},"repository":{"id":1}}`,
	"create":               `{"sha":"s","ref":"r","ref_type":"branch","repository":{"id":1}}`,
	"delete":               `{"ref":"r","ref_type":"branch","pusher_type":"user","repository":{"id":1}}`,
	"release":              `{"action":"published","release":{"id":1},"repository":{"id":1}}`,
}

// gitlabEventBodies are GitLab's payloads at the levels the preset shapes
// read; each names itself in object_kind.
var gitlabEventBodies = map[string]string{
	"push":          `{"object_kind":"push","ref":"r","before":"a","after":"b","project":{"id":1}}`,
	"tag_push":      `{"object_kind":"tag_push","ref":"r","before":"a","after":"b","project":{"id":1}}`,
	"merge_request": `{"object_kind":"merge_request","object_attributes":{"id":1,"iid":1,"last_commit":{"id":"c"}},"project":{"id":1}}`,
	"note":          `{"object_kind":"note","object_attributes":{"id":1},"project":{"id":1}}`,
	"issue":         `{"object_kind":"issue","object_attributes":{"id":1,"iid":1},"project":{"id":1}}`,
	"pipeline":      `{"object_kind":"pipeline","object_attributes":{"id":1},"project":{"id":1}}`,
	"build":         `{"object_kind":"build","build_id":1,"project":{"id":1}}`,
}

func decodeBody(t *testing.T, body string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// The header naming an event is not signed, so a preset's shape is what
// keeps a signed body of one event from counting as another. A shape lets
// through any body meeting its rules, listed event or not; so no preset may
// give a shape that another event's body meets, and each preset event's own
// body must meet its shape.
func TestWebhookProviders_NoOtherEventsBodyHasAPresetShape(t *testing.T) {
	// The instrument can fail: without its absence rule, the issues shape is
	// met by an issue_comment body.
	if !ShapeHolds([]string{"$.action", "$.issue.number", "$.repository.id"}, decodeBody(t, githubEventBodies["issue_comment"])) {
		t.Fatal("positive control: an issue_comment body should meet an issues shape of paths alone")
	}
	for _, tc := range []struct {
		provider string
		bodies   map[string]string
	}{{"github", githubEventBodies}, {"gitea", giteaEventBodies}, {"gitlab", gitlabEventBodies}} {
		p, _ := WebhookProviderPreset(tc.provider)
		events := make([]string, 0, len(p.Shapes))
		for e := range p.Shapes {
			events = append(events, e)
		}
		sort.Strings(events)
		for _, e := range events {
			if err := checkShape(p.Shapes[e]); err != nil {
				t.Errorf("%s: %s's shape: %v", tc.provider, e, err)
			}
			own, ok := tc.bodies[e]
			if !ok {
				t.Errorf("%s: no sample body for %s; add one so its shape is checked", tc.provider, e)
				continue
			}
			if !ShapeHolds(p.Shapes[e], decodeBody(t, own)) {
				t.Errorf("%s: %s's own body breaks its shape %v", tc.provider, e, p.Shapes[e])
			}
			for other, body := range tc.bodies {
				if other != e && ShapeHolds(p.Shapes[e], decodeBody(t, body)) {
					t.Errorf("%s: a %s body passes as %s (shape %v)", tc.provider, other, e, p.Shapes[e])
				}
			}
		}
	}
}

// GitLab's Issue Hook carries work items too, so its issue shape must let
// one through.
func TestWebhookProviders_GitLabIssueAdmitsAWorkItem(t *testing.T) {
	p, _ := WebhookProviderPreset("gitlab")
	workItem := `{"object_kind":"work_item","object_attributes":{"id":1,"iid":1},"project":{"id":1}}`
	if !ShapeHolds(p.Shapes["issue"], decodeBody(t, workItem)) {
		t.Errorf("a work item sent on the Issue Hook breaks the issue shape %v", p.Shapes["issue"])
	}
}

// The events the grammar newly tells apart have presets, each refusing the
// body it used to be confused with.
func TestWebhookProviders_ConfusablePairsAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		provider, event, impostor string
		bodies                    map[string]string
	}{
		{"github", "issues", "issue_comment", githubEventBodies},
		{"github", "delete", "create", githubEventBodies},
		{"gitea", "issue_comment", "pull_request_comment", giteaEventBodies},
		{"gitea", "pull_request_comment", "issue_comment", giteaEventBodies},
		{"gitlab", "push", "tag_push", gitlabEventBodies},
		{"gitlab", "tag_push", "push", gitlabEventBodies},
		{"gitlab", "issue", "merge_request", gitlabEventBodies},
	} {
		p, _ := WebhookProviderPreset(tc.provider)
		shape, ok := p.Shapes[tc.event]
		if !ok {
			t.Errorf("%s has no preset shape for %s", tc.provider, tc.event)
			continue
		}
		if ShapeHolds(shape, decodeBody(t, tc.bodies[tc.impostor])) {
			t.Errorf("%s: a %s body passes as %s", tc.provider, tc.impostor, tc.event)
		}
	}
}
