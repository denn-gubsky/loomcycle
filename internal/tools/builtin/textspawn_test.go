package builtin

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// textSpawn adapts a spawner that only produces text — every fixture here, none
// of which has a run behind its calls — to teamrun.SpawnFunc.
func textSpawn(f func(ctx context.Context, agent string, p teamrun.Prompt, defID string) (string, error)) teamrun.SpawnFunc {
	return func(ctx context.Context, agent string, p teamrun.Prompt, defID string) (teamrun.SpawnResult, error) {
		out, err := f(ctx, agent, p, defID)
		return teamrun.SpawnResult{Output: out}, err
	}
}
