package worker

import (
	"fmt"
	"sort"
)

func retainedBackups(records []BackupRecord, policy *BackupConfig) map[string]bool {
	ordered := append([]BackupRecord{}, records...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Created.Equal(ordered[j].Created) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Created.After(ordered[j].Created)
	})
	keep := map[string]bool{}
	days, weeks, months := map[string]bool{}, map[string]bool{}, map[string]bool{}
	last := 0
	for _, record := range ordered {
		if record.Repository != policy.Repository {
			continue
		}
		if last < policy.KeepLast {
			keep[record.ID] = true
			last++
		}
		stamp := record.Created.UTC()
		year, week := stamp.ISOWeek()
		for _, period := range []struct {
			key   string
			seen  map[string]bool
			limit int
		}{
			{stamp.Format("2006-01-02"), days, policy.KeepDaily},
			{fmt.Sprintf("%04d-%02d", year, week), weeks, policy.KeepWeekly},
			{stamp.Format("2006-01"), months, policy.KeepMonthly},
		} {
			if !period.seen[period.key] && len(period.seen) < period.limit {
				keep[record.ID] = true
				period.seen[period.key] = true
			}
		}
	}
	return keep
}
