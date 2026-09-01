package main

import (
	"context"
	"time"
)

// runner.go — drive one strategy through one task instance, accumulating the
// benchmark's primary metrics: cumulative tokens (the O(T^2) vs O(T) curve), peak
// single-prompt size (flatness), and task accuracy from the oracle.

// RunResult is one (arm, model, task) measurement.
type RunResult struct {
	Arm      string `json:"arm"`
	Model    string `json:"model"`
	Horizon  int    `json:"horizon"`
	Seed     int64  `json:"seed"`
	NoisePct int    `json:"noise_pct"`
	Drift    bool   `json:"drift"`

	StepCalls  int `json:"step_calls"`
	RecapCalls int `json:"recap_calls"`
	QueryCalls int `json:"query_calls"`

	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PeakPromptTokens int `json:"peak_prompt_tokens"` // O(T)-flatness witness

	Queries  int     `json:"queries"`
	Correct  int     `json:"correct"`
	Accuracy float64 `json:"accuracy"`

	ElapsedMs int64  `json:"elapsed_ms"`
	Err       string `json:"error,omitempty"`
}

// RunOnce executes the task's instruction stream then its queries under strat,
// driving the model through client. A model error aborts and is reported in-result.
func RunOnce(ctx context.Context, strat Strategy, task Task, client *ModelClient) RunResult {
	res := RunResult{
		Arm: strat.Name(), Model: client.model, Horizon: task.Horizon, Seed: task.Seed,
		Queries: len(task.Queries),
	}
	start := time.Now()

	acc := func(u Usage) {
		res.PromptTokens += u.Prompt
		res.CompletionTokens += u.Completion
		res.TotalTokens += u.Prompt + u.Completion
		if u.Prompt > res.PeakPromptTokens {
			res.PeakPromptTokens = u.Prompt
		}
	}

	for _, insn := range task.Instructions {
		resp, u, err := client.Call(ctx, strat.StepMessages(insn))
		if err != nil {
			res.Err = err.Error()
			res.ElapsedMs = time.Since(start).Milliseconds()
			return res
		}
		acc(u)
		res.StepCalls++
		strat.Observe(insn, resp)
		if rm := strat.PendingRecap(); rm != nil {
			recap, u2, err := client.Call(ctx, rm)
			if err != nil {
				res.Err = err.Error()
				res.ElapsedMs = time.Since(start).Milliseconds()
				return res
			}
			acc(u2)
			res.RecapCalls++
			strat.SetRecap(recap)
		}
	}

	for _, q := range task.Queries {
		ans, u, err := client.Call(ctx, strat.QueryMessages(q))
		if err != nil {
			res.Err = err.Error()
			res.ElapsedMs = time.Since(start).Milliseconds()
			return res
		}
		acc(u)
		res.QueryCalls++
		if q.Grade(ans) {
			res.Correct++
		}
	}

	if res.Queries > 0 {
		res.Accuracy = float64(res.Correct) / float64(res.Queries)
	}
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res
}

// newStrategy builds the strategy for an arm.
func newStrategy(arm string, task Task, keepLastN int) Strategy {
	switch arm {
	case "A0":
		return NewA0()
	case "A1":
		return NewA1(keepLastN)
	case "A2":
		return NewA2(task.Keys)
	}
	return nil
}
