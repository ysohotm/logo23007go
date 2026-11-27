package coverpool

import "context"

// SizeProgress reports how many WebP covers exist for one size vs the target.
type SizeProgress struct {
	Width  int `json:"w"`
	Height int `json:"h"`
	Count  int `json:"count"`
	Target int `json:"target"`
}

// KeywordStatus is per-keyword WebP progress across configured sizes.
type KeywordStatus struct {
	Text  string         `json:"text"`
	Sizes []SizeProgress `json:"sizes"`
}

// StatusFor builds per-size progress for each text using Pool.CountByTextSize
// (aggregates all backgrounds/formats) and Config sizes/count.
// When pool has a Registry, prefers SumCount(..., emptyBackgroundOnly=true);
// on registry error falls back to CountByTextSize.
// Missing pool/config → empty sizes or zero counts without panic.
func StatusFor(pool *Pool, cfg *Config, texts []string) []KeywordStatus {
	out := make([]KeywordStatus, 0, len(texts))
	if len(texts) == 0 {
		return out
	}
	if cfg == nil {
		for _, text := range texts {
			out = append(out, KeywordStatus{Text: text, Sizes: nil})
		}
		return out
	}

	sizes := cfg.Sizes()
	target := cfg.Count()
	var reg EntryRegistry
	if pool != nil {
		reg = pool.Registry()
	}
	for _, text := range texts {
		progress := make([]SizeProgress, 0, len(sizes))
		for _, size := range sizes {
			count := 0
			if pool != nil {
				count = pool.countForStatus(reg, text, size[0], size[1])
			}
			progress = append(progress, SizeProgress{
				Width:  size[0],
				Height: size[1],
				Count:  count,
				Target: target,
			})
		}
		out = append(out, KeywordStatus{Text: text, Sizes: progress})
	}
	return out
}

func (p *Pool) countForStatus(reg EntryRegistry, text string, width, height int) int {
	if reg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
		n, err := reg.SumCount(ctx, text, width, height, true)
		cancel()
		if err == nil {
			return n
		}
	}
	return p.CountByTextSize(text, width, height)
}
