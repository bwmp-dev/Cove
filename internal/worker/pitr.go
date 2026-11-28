package worker

import (
	"errors"
	"time"
)

const pitrPolicyLimit = 32
const pitrObjectLimit = 100000
const pitrMetadataLimit = 4 << 20
const pitrSlotBytes int64 = 512 << 20

type PITRSettings struct {
	Enabled       bool   `json:"enabled"`
	Service       string `json:"service"`
	IntervalHours int    `json:"interval_hours"`
	KeepLast      int    `json:"keep_last"`
	RetentionDays int    `json:"retention_days"`
	MaxBytes      int64  `json:"max_bytes"`
}

type PITRPolicy struct {
	Settings PITRSettings `json:"settings"`
	Key      string       `json:"key"`
	Archive  string       `json:"archive"`
}

type PITRBase struct {
	ID            string     `json:"id"`
	CreatedAt     time.Time  `json:"created_at"`
	AvailableTo   *time.Time `json:"available_to,omitempty"`
	Bytes         int64      `json:"bytes"`
	PostgresMajor int        `json:"postgres_major"`
}

type PITRStatus struct {
	verificationCursor int
	Enabled            bool       `json:"enabled"`
	State              string     `json:"state"`
	CheckedAt          time.Time  `json:"checked_at"`
	MeasuredAt         *time.Time `json:"measured_at,omitempty"`
	LastArchiveAt      *time.Time `json:"last_archive_at,omitempty"`
	Bytes              int64      `json:"bytes"`
	Objects            int        `json:"objects"`
	Receiver           bool       `json:"receiver"`
	NeedsBase          bool       `json:"needs_base"`
	Gaps               bool       `json:"gaps"`
	LastError          string     `json:"last_error,omitempty"`
	Bases              []PITRBase `json:"bases"`
}

type PITRRestore struct {
	Source    string    `json:"source"`
	Base      string    `json:"base"`
	Timestamp time.Time `json:"timestamp"`
}

func ValidatePITR(settings PITRSettings) error {
	if !serviceIdentifier.MatchString(settings.Service) {
		return errors.New("select the PostgreSQL Compose service")
	}
	if settings.IntervalHours < 1 || settings.IntervalHours > 168 || settings.KeepLast < 1 || settings.KeepLast > 32 || settings.RetentionDays < 1 || settings.RetentionDays > 30 || settings.MaxBytes < 1<<30 || settings.MaxBytes > MaxTransferBytes {
		return errors.New("PITR requires 1–168 hours between base backups, 1–32 retained bases, 1–30 days and 1–64 GiB of archive storage")
	}
	return nil
}
