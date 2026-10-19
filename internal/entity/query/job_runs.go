package query

import (
	"errors"

	"gorm.io/gorm"

	"github.com/photoprism/photoprism/internal/entity"
)

// JobRuns returns the most recent logged attempts of the named background job,
// newest first, including every retry attempt of a triggered cycle.
func JobRuns(jobName string, limit, offset int) (results entity.JobRuns, err error) {
	stmt := Db().Where("job_name = ?", jobName)

	if limit > 0 {
		stmt = stmt.Limit(limit)
	}

	if offset > 0 {
		stmt = stmt.Offset(offset)
	}

	err = stmt.Order("started_at DESC, id DESC").Find(&results).Error

	return results, err
}

// LatestJobRun returns the most recent logged attempt of the named job, or nil
// without an error if it has never run.
func LatestJobRun(jobName string) (result *entity.JobRun, err error) {
	result = &entity.JobRun{}

	if err = Db().Where("job_name = ?", jobName).Order("started_at DESC, id DESC").First(result).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}

		return nil, err
	}

	return result, nil
}

// JobConsecutiveFailures returns the number of consecutive failed cycles of the
// named job, with skipped cycles treated as neutral.
func JobConsecutiveFailures(jobName string) int {
	rows := entity.JobRuns{}

	if err := Db().Where("job_name = ? AND final = ?", jobName, true).
		Order("started_at DESC, id DESC").Limit(100).Find(&rows).Error; err != nil {
		return 0
	}

	return rows.ConsecutiveFailures()
}
