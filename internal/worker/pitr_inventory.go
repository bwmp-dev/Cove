package worker

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const pitrInventoryBytes = 64 << 20
const pitrInventoryBatch = 128

var pitrBaseName = regexp.MustCompile(`^base_[0-9A-F]{24}$`)
var pitrWALName = regexp.MustCompile(`^[0-9A-F]{24}$`)
var pitrSystemID = regexp.MustCompile(`^[0-9]{1,24}$`)

type pitrObject struct {
	Path    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

type pitrInventory struct {
	Complete  bool
	Bytes     int64
	Objects   int
	LastWAL   time.Time
	WALNames  []string
	BaseBytes map[string]int64
}

type pitrStoredBase struct {
	Name    string    `json:"backup_name"`
	Time    time.Time `json:"time"`
	WAL     string    `json:"wal_file_name"`
	Storage string    `json:"storage_name"`
}

func readPITRInventory(input io.Reader) (pitrInventory, error) {
	result := pitrInventory{WALNames: []string{}, BaseBytes: map[string]int64{}}
	bounded := &io.LimitedReader{R: input, N: pitrInventoryBytes + 1}
	decoder := json.NewDecoder(bounded)
	first, err := decoder.Token()
	if err != nil || first != json.Delim('[') {
		return result, errors.New("invalid PITR archive inventory")
	}
	seen := map[string]bool{}
	for decoder.More() {
		var object pitrObject
		if err := decoder.Decode(&object); err != nil {
			return result, errors.New("invalid PITR archive object")
		}
		result.Objects++
		if result.Objects > pitrObjectLimit+pitrInventoryBatch || bounded.N <= 0 {
			return result, errors.New("PITR inventory exceeds its bounded object or byte limit")
		}
		if object.IsDir || object.Size < 0 || object.Size > MaxTransferBytes || !pitrRelativeObject(object.Path) || seen[object.Path] {
			return result, errors.New("PITR archive contains an unexpected path or size")
		}
		seen[object.Path] = true
		if result.Bytes > math.MaxInt64-object.Size {
			return result, errors.New("PITR archive accounting overflow")
		}
		result.Bytes += object.Size
		if name, ok := strings.CutPrefix(object.Path, "basebackups_005/"); ok && len(name) >= 29 && pitrBaseName.MatchString(name[:29]) && (len(name) == 29 || name[29] == '/' || name[29] == '_') {
			result.BaseBytes[name[:29]] += object.Size
		}
		if strings.HasPrefix(object.Path, "wal_005/") {
			name := path.Base(object.Path)
			wal, suffix, _ := strings.Cut(name, ".")
			if pitrWALName.MatchString(wal) && (suffix == "" || suffix == "lz4" || suffix == "lzma" || suffix == "zst" || suffix == "br") {
				result.WALNames = append(result.WALNames, wal)
				if object.ModTime.After(result.LastWAL) {
					result.LastWAL = object.ModTime
				}
			}
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
		return result, errors.New("incomplete PITR archive inventory")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || bounded.N <= 0 {
		return result, errors.New("unexpected PITR inventory trailing data")
	}
	sort.Strings(result.WALNames)
	result.Complete = true
	return result, nil
}

func retainedPITRBases(bases []pitrStoredBase, settings PITRSettings, now time.Time) ([]pitrStoredBase, error) {
	if err := ValidatePITR(settings); err != nil {
		return nil, err
	}
	if len(bases) > 128 {
		return nil, errors.New("PITR base inventory exceeds its limit")
	}
	result := append([]pitrStoredBase{}, bases...)
	seen := map[string]bool{}
	for _, base := range result {
		if !pitrBaseName.MatchString(base.Name) || !pitrWALName.MatchString(base.WAL) || seen[base.Name] || base.Time.IsZero() || base.Time.After(now.Add(time.Minute)) || base.Storage != "default" {
			return nil, errors.New("invalid PITR base backup metadata")
		}
		seen[base.Name] = true
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Time.Equal(result[j].Time) {
			return result[i].Name > result[j].Name
		}
		return result[i].Time.After(result[j].Time)
	})
	if len(result) == 0 {
		return result, nil
	}
	cutoff := now.Add(-time.Duration(settings.RetentionDays) * 24 * time.Hour)
	keep := 1
	for keep < len(result) && keep < settings.KeepLast {
		if result[keep-1].Time.Before(cutoff) {
			break
		}
		keep++
	}
	return result[:keep], nil
}
