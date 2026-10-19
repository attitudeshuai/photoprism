package query

import (
	"github.com/photoprism/photoprism/internal/entity"
	"github.com/photoprism/photoprism/internal/form"
)

// PendingThumbFile groups the pending and failed size results recorded for one file.
type PendingThumbFile struct {
	FileRoot string                `json:"Root"`
	FileName string                `json:"Name"`
	FileHash string                `json:"Hash,omitempty"`
	Sizes    entity.ThumbStatuses `json:"Sizes"`
}

// PendingThumbFiles returns up to limit distinct files that have pending or failed size results,
// together with those rows, ordered by root, name, and size name for stable batch processing.
func PendingThumbFiles(limit int) (files []PendingThumbFile, err error) {
	return PendingThumbFilesFrom(limit, "", "")
}

// PendingThumbFilesFrom returns the next batch of distinct files with pending or failed results
// after the given root/name keyset position, so batch processing never revisits files whose
// sizes still fail within the same run.
func PendingThumbFilesFrom(limit int, afterRoot, afterName string) (files []PendingThumbFile, err error) {
	type fileKey struct {
		FileRoot string
		FileName string
	}

	var keys []fileKey

	keyQuery := Db().Model(&entity.ThumbStatus{}).
		Where("status IN (?)", []string{entity.ThumbStatusPending, entity.ThumbStatusFailed}).
		Select("file_root, file_name").
		Group("file_root, file_name").
		Order("file_root, file_name").
		Limit(limit)

	// Composite keyset predicate, portable across SQLite, MariaDB, and PostgreSQL.
	if afterName != "" {
		keyQuery = keyQuery.Where("file_root > ? OR (file_root = ? AND file_name > ?)", afterRoot, afterRoot, afterName)
	}

	if err = keyQuery.Scan(&keys).Error; err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, nil
	}

	// Combine the distinct keys into one nested OR condition so it stays separate from
	// the status predicate and can be prepared portably across all supported dialects.
	keysCond := Db().Where("file_root = ? AND file_name = ?", keys[0].FileRoot, keys[0].FileName)

	for _, key := range keys[1:] {
		keysCond = keysCond.Or("file_root = ? AND file_name = ?", key.FileRoot, key.FileName)
	}

	var rows entity.ThumbStatuses

	if err = Db().
		Where("status IN (?)", []string{entity.ThumbStatusPending, entity.ThumbStatusFailed}).
		Where(keysCond).
		Order("file_root, file_name, size_name").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	byKey := make(map[string]*PendingThumbFile, len(keys))

	for i := range rows {
		row := &rows[i]
		key := row.FileRoot + "\x00" + row.FileName

		group, ok := byKey[key]
		if !ok {
			group = &PendingThumbFile{FileRoot: row.FileRoot, FileName: row.FileName, FileHash: row.FileHash}
			byKey[key] = group
		}

		// Keep the newest known hash if rows for the same key disagree.
		if group.FileHash == "" {
			group.FileHash = row.FileHash
		}

		group.Sizes = append(group.Sizes, *row)
	}

	files = make([]PendingThumbFile, 0, len(keys))

	for _, key := range keys {
		if group, ok := byKey[key.FileRoot+"\x00"+key.FileName]; ok {
			files = append(files, *group)
		}
	}

	return files, nil
}

// PendingThumbSizes counts the pending and failed size results recorded for one file.
func PendingThumbSizes(root, name string) (count int, err error) {
	if root == "" {
		root = entity.RootOriginals
	}

	var total int64

	err = Db().Model(&entity.ThumbStatus{}).
		Where("file_root = ? AND file_name = ? AND status IN (?)",
			root, name, []string{entity.ThumbStatusPending, entity.ThumbStatusFailed}).
		Count(&total).Error

	return int(total), err
}

// ThumbStatuses returns thumbnail size results matching the search form, ordered for stable pagination.
func ThumbStatuses(f form.ThumbStatusSearch) (result entity.ThumbStatuses, err error) {
	s := Db().Model(&entity.ThumbStatus{})

	if f.Status == "" {
		s = s.Where("status IN (?)", []string{entity.ThumbStatusPending, entity.ThumbStatusFailed})
	} else {
		s = s.Where("status = ?", f.Status)
	}

	if f.Path != "" {
		s = s.Where("file_name LIKE ?", "%"+f.Path+"%")
	}

	s = s.Order("file_root, file_name, size_name")

	if f.Count > 0 {
		s = s.Limit(f.Count).Offset(f.Offset)
	}

	if err = s.Find(&result).Error; err != nil {
		return result, err
	}

	return result, nil
}

// ThumbStatusCounts returns the number of rows per status, including zero counts, sorted by name.
func ThumbStatusCounts() (map[string]int64, error) {
	type statusCount struct {
		Status string
		Count  int64
	}

	var rows []statusCount

	err := Db().Model(&entity.ThumbStatus{}).
		Select("status, COUNT(*) AS count").
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	result := map[string]int64{
		entity.ThumbStatusPending:   0,
		entity.ThumbStatusFailed:    0,
		entity.ThumbStatusSkipped:   0,
		entity.ThumbStatusSucceeded: 0,
	}

	for _, row := range rows {
		result[row.Status] = row.Count
	}

	return result, nil
}
