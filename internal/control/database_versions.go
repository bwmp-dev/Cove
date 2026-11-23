package control

import (
	"regexp"
	"strings"
)

var databaseImageTag = regexp.MustCompile(`^[0-9][0-9A-Za-z_.-]*\.[0-9A-Za-z_.-]+$`)
var databaseImageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

func databasePinnedImage(database *databaseResource) bool {
	image, digest, hashed := strings.Cut(database.Image, "@")
	if hashed && !databaseImageDigest.MatchString(digest) {
		return false
	}
	repository, tag, ok := strings.Cut(image, ":")
	if !ok || !databaseImageTag.MatchString(tag) {
		return false
	}
	expected := map[string]string{"postgresql": "postgres", "mysql": "mysql", "mariadb": "mariadb", "mongodb": "mongo", "redis": "redis", "valkey": "valkey/valkey", "clickhouse": "clickhouse/clickhouse-server"}[database.Engine]
	if database.Engine == "postgresql" && database.Variant != "" {
		if database.Variant == "vector" {
			expected = "pgvector/pgvector"
		} else if database.Variant == "postgis" {
			expected = "postgis/postgis"
		} else {
			return false
		}
		return hashed && repository == expected
	}
	return repository == expected && database.Version == tag
}

type databaseVersion struct {
	Version string `json:"version"`
	Image   string `json:"image"`
	Variant string `json:"variant"`
	Warning string `json:"warning,omitempty"`
}

func databaseAdditionalVersions(engine string) []databaseVersion {
	switch engine {
	case "postgresql":
		return []databaseVersion{
			{"17.12-alpine", "postgres:17.12-alpine", "", ""},
			{"18.7-vector0.8.7", "pgvector/pgvector:0.8.7-pg18-trixie@sha256:ab49524d18df7200b74ac9a475e1db6297fc1eb57da4c214c63d10ecc4b754fa", "vector", "Changes the image and operating-system libraries; copy and validate application queries before cut-over."},
			{"18.6-postgis3.6", "postgis/postgis:18-3.6@sha256:4a43e3c3e5d55b9208240fc71db543b79c638766280c625a425bd0a2390989c4", "postgis", "Linux amd64 only. This pinned image contains PostgreSQL 18.6 and PostGIS 3.6.4. A separate copy is required; review its older patch version before cut-over."},
		}
	case "mysql":
		return []databaseVersion{{"8.0.46", "mysql:8.0.46@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b", "", "MySQL 8.0 is end of life; use only to recover or prepare an upgrade copy to 8.4."}}
	case "mariadb":
		return []databaseVersion{{"10.11.19", "mariadb:10.11.19@sha256:e5fe75b2b9de0eb9f6f9a4d9c97272fb3a7480e389ccd487a786bab2a12cc0d7", "", "Older LTS source for a logical upgrade copy to 11.8."}}
	case "mongodb":
		return []databaseVersion{{"7.0.48-jammy", "mongo:7.0.48-jammy@sha256:e0da75c955723fb6918111a60e3249079e5ea082c70038dcbc2f5fa32e29ab8c", "", "Upgrade copies require source FCV 7.0 and use matching dump/restore tools."}}
	}
	return nil
}

func databaseCopyAllowed(source *databaseResource, version string) bool {
	if source.Version == version {
		return false
	}
	switch source.Engine {
	case "postgresql":
		return (strings.HasPrefix(source.Version, "17.") || strings.HasPrefix(source.Version, "18.")) && strings.HasPrefix(version, "18.")
	case "mysql":
		return strings.HasPrefix(source.Version, "8.0.") && strings.HasPrefix(version, "8.4.")
	case "mariadb":
		return strings.HasPrefix(source.Version, "10.11.") && strings.HasPrefix(version, "11.8.")
	case "mongodb":
		return strings.HasPrefix(source.Version, "7.0.") && strings.HasPrefix(version, "8.0.")
	}
	return false
}
