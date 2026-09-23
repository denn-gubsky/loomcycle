package teamrun

import "context"

// textSpawn adapts a spawner that only produces text — every fixture here, none
// of which has a run behind its calls — to SpawnFunc.
func textSpawn(f func(ctx context.Context, agent string, p Prompt, defID string) (string, error)) SpawnFunc {
	return func(ctx context.Context, agent string, p Prompt, defID string) (SpawnResult, error) {
		out, err := f(ctx, agent, p, defID)
		return SpawnResult{Output: out}, err
	}
}
