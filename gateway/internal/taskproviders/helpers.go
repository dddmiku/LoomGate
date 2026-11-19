package taskproviders

import (
	"errors"
	"time"

	"workbuddy2api/internal/taskengine"
)

func observation(phase, summary string) taskengine.Observation {
	return taskengine.Observation{Phase: phase, Summary: summary}
}

// businessDate parses the upstream's own business date. A malformed date is an
// error, never a silently coerced local date: period keys must match upstream.
func businessDate(v string) (time.Time, error) {
	if len(v) != 10 {
		return time.Time{}, errors.New("business_date_invalid")
	}
	d, e := time.Parse("2006-01-02", v)
	if e != nil {
		return d, errors.New("business_date_invalid")
	}
	return d, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
