package turso

import (
	"context"
	"fmt"
	"strings"
)

// GroupConfig represents configuration for a database group (local copy to avoid import cycle).
type GroupConfig struct {
	GroupName     string
	AuthToken     string
	DBNames       []string
	EncryptionKey string
	PureLocal     bool
}

// DBInfo carries the metadata callers need to pick a compatible connector.
// The server Version is reported by the Turso Platform API and lets us
// distinguish legacy sqld (e.g. "0.24.29") from the new tursodb platform
// (e.g. "2026.6.0"); the two speak different sync protocols.
type DBInfo struct {
	URL     string
	Version string
}

// IsLegacy reports whether the database is hosted on legacy sqld, whose sync
// endpoints are not compatible with tursogo's sync engine. We treat any
// version that starts with "0." as legacy. An empty Version (e.g. pure-local
// or unknown) is treated as non-legacy so the caller's primary code path is
// exercised.
func (i DBInfo) IsLegacy() bool {
	return strings.HasPrefix(i.Version, "0.")
}

// ensureDB ensures that a database exists in the specified Turso group.
// If the database doesn't exist, it creates it. Returns DBInfo (URL + version).
func ensureDB(ctx context.Context, tc *TursoClient, groupName, dbName string) (DBInfo, error) {
	// First, try to get the existing database
	db, err := tc.getDatabase(ctx, dbName)
	if err == nil {
		url, err := dbURL(db)
		if err != nil {
			return DBInfo{}, err
		}
		return DBInfo{URL: url, Version: db.Version}, nil
	}

	// If database doesn't exist, create it
	if fmt.Sprintf("database %s not found", dbName) == err.Error() {
		db, err = tc.createDatabase(ctx, groupName, dbName)
		if err != nil {
			return DBInfo{}, fmt.Errorf("failed to create database %s in group %s: %w", dbName, groupName, err)
		}
		url, err := dbURL(db)
		if err != nil {
			return DBInfo{}, fmt.Errorf("created database %s but %w", dbName, err)
		}
		return DBInfo{URL: url, Version: db.Version}, nil
	}

	// Some other error occurred
	return DBInfo{}, fmt.Errorf("failed to ensure database %s in group %s: %w", dbName, groupName, err)
}

// dbURL prefers the primary URL the API hands back and falls back to the
// hostname (which is what the API returns for legacy DBs).
func dbURL(db *Database) (string, error) {
	if db.PrimaryURL != "" {
		return db.PrimaryURL, nil
	}
	if db.Hostname != "" {
		return fmt.Sprintf("libsql://%s", db.Hostname), nil
	}
	return "", fmt.Errorf("database has no valid URL")
}

// validateGroupExists checks if a group exists and is accessible
func validateGroupExists(ctx context.Context, tc *TursoClient, groupName string) error {
	_, err := tc.getGroup(ctx, groupName)
	if err != nil {
		return fmt.Errorf("failed to validate group %s: %w", groupName, err)
	}
	return nil
}

// EnsureAllDBs ensures all databases in all configured groups exist and
// returns their connection metadata keyed by "group.dbname". For pure-local
// groups the entry is a zero-value DBInfo (empty URL, empty version).
func EnsureAllDBs(ctx context.Context, tc *TursoClient, groups []GroupConfig) (map[string]DBInfo, error) {
	dbInfo := make(map[string]DBInfo)

	for _, group := range groups {
		if group.PureLocal {
			for _, dbName := range group.DBNames {
				dbKey := fmt.Sprintf("%s.%s", group.GroupName, dbName)
				dbInfo[dbKey] = DBInfo{}
			}
			continue
		}

		// Validate that the group exists
		if err := validateGroupExists(ctx, tc, group.GroupName); err != nil {
			return nil, err
		}

		// Ensure all databases in the group exist
		for _, dbName := range group.DBNames {
			dbKey := fmt.Sprintf("%s.%s", group.GroupName, dbName)

			info, err := ensureDB(ctx, tc, group.GroupName, dbName)
			if err != nil {
				return nil, fmt.Errorf("failed to ensure database %s: %w", dbKey, err)
			}

			dbInfo[dbKey] = info
		}
	}

	return dbInfo, nil
}
