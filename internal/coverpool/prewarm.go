package coverpool

import (
	"context"
	"strings"
)

// EnqueueResult summarizes a manual prewarm request.
type EnqueueResult struct {
	Keywords int      `json:"keywords"`
	Specs    int      `json:"specs"`
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Failed   int      `json:"failed"`
	Errors   []string `json:"errors,omitempty"`
}

// Prewarmer fills pools for pasted keywords using configured sizes and WebP only.
type Prewarmer struct {
	Config    *Config
	Pool      *Pool
	Scheduler *Scheduler
	Generate  Generator
}

// MaxEnqueueKeywords caps a single manual prewarm request.
const MaxEnqueueKeywords = 200

// NormalizeKeywords trims, drops empties, dedupes preserving order. Cap at MaxEnqueueKeywords.
func NormalizeKeywords(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= MaxEnqueueKeywords {
			break
		}
	}
	return out
}

// Enqueue schedules (or sync-fills when count==1) WebP covers for each keyword × Config.Sizes().
func (p *Prewarmer) Enqueue(ctx context.Context, raw []string) EnqueueResult {
	keywords := NormalizeKeywords(raw)
	result := EnqueueResult{Keywords: len(keywords)}
	if p == nil || p.Pool == nil || p.Config == nil || p.Generate == nil {
		result.Failed = 1
		result.Errors = []string{"coverpool: prewarm unavailable"}
		return result
	}

	sizes := p.Config.Snapshot().Sizes
	target := p.Config.Count()
	result.Specs = len(keywords) * len(sizes)

	for _, keyword := range keywords {
		for _, size := range sizes {
			spec := Spec{
				Text:       keyword,
				Width:      size[0],
				Height:     size[1],
				Background: "",
				Format:     FormatWebP,
			}
			count, err := p.Pool.Count(spec)
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, shortErr(err))
				continue
			}
			if count >= target {
				result.Skipped++
				continue
			}
			if target == 1 {
				if err := p.Generate.Generate(ctx, spec, 0); err != nil {
					result.Failed++
					result.Errors = append(result.Errors, shortErr(err))
					continue
				}
				result.Accepted++
				_ = p.Pool.SaveMeta(spec)
				continue
			}
			if p.Scheduler.Ensure(spec, target) {
				result.Accepted++
				_ = p.Pool.SaveMeta(spec)
			} else {
				result.Skipped++
			}
		}
	}
	return result
}

func shortErr(err error) string {
	msg := err.Error()
	const maxLen = 120
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen]
}
