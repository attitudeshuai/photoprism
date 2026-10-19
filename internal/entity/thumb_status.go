package entity

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ThumbStatuses represents a list of thumbnail size results.
type ThumbStatuses []ThumbStatus

// Thumbnail size generation statuses.
const (
	ThumbStatusPending   = "pending"
	ThumbStatusSucceeded = "succeeded"
	ThumbStatusSkipped   = "skipped"
	ThumbStatusFailed    = "failed"
)

// ThumbStatusErrorLen is the byte budget of the error column for a size result.
const ThumbStatusErrorLen = 512

// ThumbStatus records the generation result of a single thumbnail size for one file.
// Rows are keyed by the file identity (root and relative name, matching the files table)
// plus the size name; the file hash invalidates stale results when the original changes.
type ThumbStatus struct {
	FileRoot string `gorm:"primaryKey;autoIncrement:false;type:bytes;size:16;not null;default:'/';index:idx_thumbs_status_file,priority:1;index:idx_thumbs_status_group,priority:2" json:"Root" yaml:"Root,omitempty"`
	FileName string `gorm:"primaryKey;autoIncrement:false;type:bytes;size:1024;not null;index:idx_thumbs_status_file,priority:2;index:idx_thumbs_status_group,priority:3" json:"Name" yaml:"Name,omitempty"`
	SizeName string `gorm:"primaryKey;autoIncrement:false;column:size_name;type:bytes;size:32;not null;index:idx_thumbs_status_group,priority:4" json:"Size" yaml:"Size"`
	FileHash string `gorm:"column:file_hash;type:bytes;size:128;index" json:"Hash,omitempty" yaml:"Hash,omitempty"`
	Status   string `gorm:"type:bytes;size:16;not null;default:'pending';index:idx_thumbs_status_group,priority:1" json:"Status" yaml:"Status"`
	Error    string `gorm:"type:bytes;size:512" json:"Error,omitempty" yaml:"Error,omitempty"`
	Attempts int       `gorm:"default:0;size:32" json:"Attempts" yaml:"Attempts,omitempty"`
	CreatedAt time.Time `json:"CreatedAt" yaml:"-"`
	UpdatedAt time.Time `json:"UpdatedAt" yaml:"UpdatedAt"`
}

// TableName returns the entity table name.
func (ThumbStatus) TableName() string {
	return "thumbs_status"
}

// NewThumbStatus returns a thumbnail size status record with the given identity and state.
func NewThumbStatus(root, name, sizeName, fileHash, status string) *ThumbStatus {
	return &ThumbStatus{
		FileRoot: root,
		FileName: name,
		SizeName: sizeName,
		FileHash: fileHash,
		Status:   status,
	}
}

// Validate returns an error if the record misses its identity or carries an unknown status.
func (m ThumbStatus) Validate() error {
	if m.FileName == "" || m.SizeName == "" {
		return errors.New("thumb status: file name and size must not be empty")
	}

	switch m.Status {
	case ThumbStatusPending, ThumbStatusSucceeded, ThumbStatusSkipped, ThumbStatusFailed:
		return nil
	default:
		return errors.New("thumb status: invalid status " + m.Status)
	}
}

// RegisterPendingThumbSizes inserts pending rows for size results that have not been recorded yet.
// Existing rows, including prior failures with their error and attempt count, are left untouched.
func RegisterPendingThumbSizes(rows []ThumbStatus) error {
	for i := range rows {
		if rows[i].Status == "" {
			rows[i].Status = ThumbStatusPending
		}

		if rows[i].FileRoot == "" {
			rows[i].FileRoot = RootOriginals
		}

		if err := rows[i].Validate(); err != nil {
			return err
		}
	}

	if len(rows) == 0 {
		return nil
	}

	insert := func(batch []ThumbStatus) error {
		return UnscopedDb().Clauses(clause.OnConflict{DoNothing: true}).Create(&batch).Error
	}

	// Keep individual statements short so one oversized batch cannot exceed placeholder limits.
	const maxBatch = 200

	for len(rows) > 0 {
		n := min(len(rows), maxBatch)

		if err := insert(rows[:n]); err != nil {
			return err
		}

		rows = rows[n:]
	}

	return nil
}

// ReportThumbSize stores the terminal result of one thumbnail size. With the same hash, a
// succeeded row is never downgraded to a pending, skipped, or failed result; a changed file hash
// means the original was replaced, so the new result takes over. Failed attempts increment the
// attempt counter, while a success clears the error and counter.
func ReportThumbSize(row ThumbStatus) error {
	if row.FileRoot == "" {
		row.FileRoot = RootOriginals
	}

	row.Error = Clip(row.Error, ThumbStatusErrorLen)

	if err := row.Validate(); err != nil {
		return err
	}

	if row.Status == ThumbStatusPending {
		return errors.New("thumb status: pending is not a terminal result")
	}

	// Cover the case where no pending row was registered before the result arrived.
	// The counter starts at zero in every case; the conditional update below is the only
	// place that increments it, so a first failure is counted once.
	first := row
	first.Attempts = 0

	if err := UnscopedDb().Clauses(clause.OnConflict{DoNothing: true}).Create(&first).Error; err != nil {
		return err
	}

	values := Values{
		"status":    row.Status,
		"file_hash": row.FileHash,
	}

	switch row.Status {
	case ThumbStatusFailed:
		values["error"] = row.Error
		values["attempts"] = gorm.Expr("attempts + 1")
	case ThumbStatusSucceeded:
		values["error"] = ""
		values["attempts"] = 0
	default:
		// Skipped rows may carry a human-readable reason, but never a failure counter.
		values["error"] = row.Error
	}

	// Take over rows recorded for a different hash; with the same hash, do not touch a
	// succeeded row. Both predicates are portable across SQLite, MariaDB, and PostgreSQL.
	return UnscopedDb().Model(&ThumbStatus{}).
		Where("file_root = ? AND file_name = ? AND size_name = ?", row.FileRoot, row.FileName, row.SizeName).
		Where("file_hash <> ? OR status <> ?", row.FileHash, ThumbStatusSucceeded).
		Updates(values).Error
}

// DeleteThumbStatuses removes every size result recorded for the given file.
func DeleteThumbStatuses(root, name string) error {
	if root == "" {
		root = RootOriginals
	}

	return UnscopedDb().Where("file_root = ? AND file_name = ?", root, name).
		Delete(&ThumbStatus{}).Error
}
