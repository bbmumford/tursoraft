package turso

import (
	"context"
	"fmt"
)

// GroupConfig represents configuration for a database group (local copy to avoid import cycle).
type GroupConfig struct {
	GroupName     string
	AuthToken     string
	DBNames       []string
	EncryptionKey string
	PureLocal     bool
}

// ensureDB ensures that a database exists in the specified Turso group.
// If the database doesn't exist, it creates it. Returns the primary libraryQL URL.
func ensureDB(ctx context.Context, tc *TursoClient, groupName, dbName string) (primaryURL string, err error) {
	// First, try to get the existing database
	db, err := tc.getDatabase(ctx, dbName)
	if err == nil {
		// Database exists, return its primary URL
		if db.PrimaryURL != "" {
			return db.PrimaryURL, nil
		}
		// Fallback to constructing URL from hostname
		if db.Hostname != "" {
			return fmt.Sprintf("libsql://%s", db.Hostname), nil
		}
		return "", fmt.Errorf("database %s exists but has no valid URL", dbName)
	}

	// If database doesn't exist, create it
	if fmt.Sprintf("database %s not found", dbName) == err.Error() {
		db, err = tc.createDatabase(ctx, groupName, dbName)
		if err != nil {
			return "", fmt.Errorf("failed to create database %s in group %s: %w", dbName, groupName, err)
		}

		if db.PrimaryURL != "" {
			return db.PrimaryURL, nil
		}

		// Fallback to constructing URL from hostname
		if db.Hostname != "" {
			return fmt.Sprintf("libsql://%s", db.Hostname), nil
		}

		return "", fmt.Errorf("created database %s but received no valid URL", dbName)
	}

	// Some other error occurred
	return "", fmt.Errorf("failed to ensure database %s in group %s: %w", dbName, groupName, err)
}

// validateGroupExists checks if a group exists and is accessible
func validateGroupExists(ctx context.Context, tc *TursoClient, groupName string) error {
	_, err := tc.getGroup(ctx, groupName)
	if err != nil {
		return fmt.Errorf("failed to validate group %s: %w", groupName, err)
	}
	return nil
}

// ensureAllDBs ensures all databases in all configured groups exist
func EnsureAllDBs(ctx context.Context, tc *TursoClient, groups []GroupConfig) (map[string]string, error) {
	dbURLs := make(map[string]string)

	for _, group := range groups {
		if group.PureLocal {
			for _, dbName := range group.DBNames {
				dbKey := fmt.Sprintf("%s.%s", group.GroupName, dbName)
				dbURLs[dbKey] = ""
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

			primaryURL, err := ensureDB(ctx, tc, group.GroupName, dbName)
			if err != nil {
				return nil, fmt.Errorf("failed to ensure database %s: %w", dbKey, err)
			}

			dbURLs[dbKey] = primaryURL
		}
	}

	return dbURLs, nil
}
