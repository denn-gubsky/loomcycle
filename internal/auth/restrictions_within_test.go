package auth

import (
	"reflect"
	"testing"
)

// A minted record is held to its minter's: an agents list is narrowed to what
// both allow, a minter's list fills in for none, and nothing in common is
// refused — never an empty list, which would read as unrestricted.
func TestRestrictionsWithin_AgentsAreTheIntersectionAndNeverEmpty(t *testing.T) {
	minter := Restrictions{Agents: []string{"score", "judge"}}
	got, err := Restrictions{Agents: []string{"judge", "other"}}.Within(minter)
	if err != nil || !reflect.DeepEqual(got.Agents, []string{"judge"}) {
		t.Errorf("intersection = %v (err %v), want [judge]", got.Agents, err)
	}
	got, err = Restrictions{}.Within(minter)
	if err != nil || !reflect.DeepEqual(got.Agents, []string{"score", "judge"}) {
		t.Errorf("no list of its own = %v (err %v), want the minter's", got.Agents, err)
	}
	if _, err := (Restrictions{Agents: []string{"other"}}).Within(minter); err == nil {
		t.Error("a list with nothing in common was accepted")
	}
	// An unrestricted minter changes nothing.
	own := Restrictions{Agents: []string{"x"}, Rate: &RateLimit{PerMinute: 3}}
	if got, err := own.Within(Restrictions{}); err != nil || !reflect.DeepEqual(got, own) {
		t.Errorf("within an unrestricted minter = %+v (err %v), want unchanged", got, err)
	}
}

// RFC GA: a minted record never holds a surface its confined minter does not.
// A confined record keeps the surfaces both name; an unconfined one, made
// confined by its minter, gets the minter's; an unconfined minter changes
// nothing.
func TestRestrictionsWithin_SurfacesNeverExceedTheMinters(t *testing.T) {
	minter := Restrictions{Agents: []string{"chat"}, Surfaces: []string{SurfaceTranscription}}
	got, err := Restrictions{Agents: []string{"chat"}, Surfaces: []string{SurfaceSpeech, SurfaceTranscription}}.Within(minter)
	if err != nil || !reflect.DeepEqual(got.Surfaces, []string{SurfaceTranscription}) {
		t.Errorf("confined record = %v (err %v), want [transcription]", got.Surfaces, err)
	}
	got, err = Restrictions{}.Within(minter)
	if err != nil || !reflect.DeepEqual(got.Surfaces, []string{SurfaceTranscription}) || !reflect.DeepEqual(got.Agents, []string{"chat"}) {
		t.Errorf("unconfined record = %+v (err %v), want the minter's agents and surfaces", got, err)
	}
	got, err = Restrictions{Agents: []string{"chat"}, Surfaces: []string{SurfaceSpeech}}.Within(Restrictions{Agents: []string{"chat"}})
	if err != nil || len(got.Surfaces) != 0 {
		t.Errorf("a minter with no grant passed one on: %v (err %v)", got.Surfaces, err)
	}
	own := Restrictions{Agents: []string{"chat"}, Surfaces: []string{SurfaceSpeech}}
	if got, err := own.Within(Restrictions{}); err != nil || !reflect.DeepEqual(got, own) {
		t.Errorf("within an unrestricted minter = %+v (err %v), want unchanged", got, err)
	}
}
