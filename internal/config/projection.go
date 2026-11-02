package config

import (
	"fmt"
	"strings"
	"time"
)

// ProjectionConfig is the memory.projection block: the 3D layout of a scope's
// document chunks (RFC FQ P2). Each chunk's stored embedding is placed on the
// scope's top three principal components, fitted from a sample of the scope's
// own vectors, so a viewer can draw where a chunk sits by meaning. Only the
// three coordinates ever leave the runtime.
//
// OFF BY DEFAULT, and off means nothing runs: no table is read, no sweeper
// starts, graph_snapshot answers its projection block empty and search ignores
// with_projection. On, the operator pass POST /v1/_document/project works; the
// background sweeper also needs a positive interval.
type ProjectionConfig struct {
	Enabled bool `yaml:"enabled"`
	// Interval is how often the background sweeper walks every scope with
	// document vectors (a Go duration, e.g. "1h"). Empty or "0" starts no
	// sweeper; the operator pass still works. Env
	// LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL overrides it, and a positive value
	// there also turns the projection on.
	Interval string `yaml:"interval"`
	// RefitAfter is the age past which a basis is refitted, once RefitMinNew
	// chunks were placed since it was fitted. Default 168h (7 days).
	RefitAfter string `yaml:"refit_after"`
	// RefitMinNew is the number of chunks placed since the last fit a refit
	// waits for. Default 500.
	RefitMinNew int `yaml:"refit_min_new"`
	// SampleMax caps the vectors a fit reads (a deterministic uniform reservoir
	// sample). Default 20000.
	SampleMax int `yaml:"sample_max"`
	// Method is how a scope's map is fitted (RFC FQ §11): "pca" (the default)
	// keeps the three directions of largest variance — exact, instant, and a new
	// chunk is placed by a formula; "umap" keeps neighbourhoods, so a document's
	// chunks stay together on the map, for a heavier fit (seconds to a minute at
	// 10,000 chunks, off the request path). A scope whose map was fitted with the
	// other method is refitted on its next pass.
	Method string `yaml:"method"`
	// UMAPSampleMax caps the chunks a UMAP fit lays out — its k-NN graph costs
	// the square of it — and the rest are placed among them. Default 10000.
	UMAPSampleMax int `yaml:"umap_sample_max"`
}

const (
	projectionDefaultRefitAfter  = 7 * 24 * time.Hour
	projectionDefaultRefitMinNew = 500
	projectionDefaultSampleMax   = 20000
	projectionDefaultUMAPSample  = 10000
)

// Projection methods: memory.projection.method.
const (
	ProjectionMethodPCA  = "pca"
	ProjectionMethodUMAP = "umap"
)

// ProjectionSettings is ProjectionConfig resolved: env applied, defaults
// filled, durations parsed. Built by Config.DocumentProjection.
type ProjectionSettings struct {
	Enabled     bool
	Interval    time.Duration
	RefitAfter  time.Duration
	RefitMinNew int
	SampleMax   int
	// Method is ProjectionMethodPCA or ProjectionMethodUMAP.
	Method        string
	UMAPSampleMax int
}

func parseProjectionDuration(field, v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. 1h, 30m): %w", field, v, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %q is negative", field, v)
	}
	return d, nil
}

// resolve applies the env interval and the defaults. An invalid duration is an
// error here and a boot failure through validate.
func (p ProjectionConfig) resolve(envInterval string) (ProjectionSettings, error) {
	s := ProjectionSettings{Enabled: p.Enabled}
	var err error
	if s.Interval, err = parseProjectionDuration("memory.projection.interval", p.Interval); err != nil {
		return s, err
	}
	if strings.TrimSpace(envInterval) != "" {
		if s.Interval, err = parseProjectionDuration("LOOMCYCLE_DOCUMENT_PROJECTION_INTERVAL", envInterval); err != nil {
			return s, err
		}
		if s.Interval > 0 {
			s.Enabled = true
		}
	}
	if s.RefitAfter, err = parseProjectionDuration("memory.projection.refit_after", p.RefitAfter); err != nil {
		return s, err
	}
	if s.RefitAfter == 0 {
		s.RefitAfter = projectionDefaultRefitAfter
	}
	if p.RefitMinNew < 0 {
		return s, fmt.Errorf("memory.projection.refit_min_new: %d is negative", p.RefitMinNew)
	}
	s.RefitMinNew = p.RefitMinNew
	if s.RefitMinNew == 0 {
		s.RefitMinNew = projectionDefaultRefitMinNew
	}
	if p.SampleMax < 0 {
		return s, fmt.Errorf("memory.projection.sample_max: %d is negative", p.SampleMax)
	}
	s.SampleMax = p.SampleMax
	if s.SampleMax == 0 {
		s.SampleMax = projectionDefaultSampleMax
	}
	switch m := strings.TrimSpace(p.Method); m {
	case "", ProjectionMethodPCA:
		s.Method = ProjectionMethodPCA
	case ProjectionMethodUMAP:
		s.Method = m
	default:
		return s, fmt.Errorf("memory.projection.method: %q is not %s or %s", p.Method, ProjectionMethodPCA, ProjectionMethodUMAP)
	}
	if p.UMAPSampleMax < 0 {
		return s, fmt.Errorf("memory.projection.umap_sample_max: %d is negative", p.UMAPSampleMax)
	}
	s.UMAPSampleMax = p.UMAPSampleMax
	if s.UMAPSampleMax == 0 {
		s.UMAPSampleMax = projectionDefaultUMAPSample
	}
	return s, nil
}

// DocumentProjection is the resolved memory.projection block. A nil Config, or
// a value validate would refuse, resolves to off.
func (c *Config) DocumentProjection() ProjectionSettings {
	if c == nil {
		return ProjectionSettings{}
	}
	s, err := c.Memory.Projection.resolve(c.Env.DocumentProjectionInterval)
	if err != nil {
		return ProjectionSettings{}
	}
	return s
}
