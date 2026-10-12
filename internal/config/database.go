package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DatabaseKind is one of the databases the embedded actor runtime can persist state, jobs, and alarms to
type DatabaseKind string

const (
	// DatabaseKindSQLite is a SQLite database file, which only supports a single instance of the service
	DatabaseKindSQLite DatabaseKind = "sqlite"
	// DatabaseKindPostgres is a Postgres database, which is what more than one instance shares to work on the same workflows
	DatabaseKindPostgres DatabaseKind = "postgres"
)

// Prefixes that select each database from a connection string
const (
	sqliteScheme         = "sqlite://"
	sqliteBareScheme     = "sqlite:"
	postgresScheme       = "postgres://"
	postgresAliasScheme  = "postgresql://"
	sqliteFileConnPrefix = "file:"
)

// Database is the database the embedded actor runtime persists to, as parsed from the `actors.database` connection string
type Database struct {
	// Which database this is, which selects the Francis provider
	Kind DatabaseKind
	// Connection string to hand to the provider, with the `sqlite://` prefix removed for SQLite
	ConnectionString string
}

// IsFilePath reports whether the SQLite connection string is a plain path, rather than one of the URI forms the driver also accepts
// The folder of a plain path is created at startup, since the driver creates the database file but not the folder that contains it
func (d Database) IsFilePath() bool {
	return d.Kind == DatabaseKindSQLite &&
		!strings.HasPrefix(d.ConnectionString, sqliteFileConnPrefix) &&
		!strings.Contains(d.ConnectionString, "?")
}

// ParseDatabase parses the connection string of the database the embedded actor runtime persists to
//
// Postgres is selected by a `postgres://` or `postgresql://` URL, optionally carrying connection options
// Everything else is a SQLite database, either as `sqlite://<path-to-file>` or as a bare path
func ParseDatabase(dsn string) (Database, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return Database{}, errors.New("the connection string is empty")
	}

	// Postgres connection strings are handed to the driver as they are, since it's the one that understands their options
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, postgresScheme) || strings.HasPrefix(lower, postgresAliasScheme) {
		u, err := url.Parse(dsn)
		if err != nil {
			return Database{}, fmt.Errorf("the Postgres connection string is not a valid URL: %w", err)
		}
		if u.Host == "" {
			return Database{}, errors.New("the Postgres connection string must include a host")
		}

		return Database{
			Kind:             DatabaseKindPostgres,
			ConnectionString: dsn,
		}, nil
	}

	// SQLite is the catch-all, so a connection string that's just a path selects it too
	// The scheme is stripped because the driver takes a path, and it's accepted without the double slash as well since that's an easy thing to write by hand
	connectionString := dsn
	switch {
	case strings.HasPrefix(lower, sqliteScheme):
		connectionString = dsn[len(sqliteScheme):]
	case strings.HasPrefix(lower, sqliteBareScheme):
		connectionString = dsn[len(sqliteBareScheme):]
	}
	if connectionString == "" {
		return Database{}, errors.New("the SQLite connection string must include the path of the database file")
	}

	return Database{
		Kind:             DatabaseKindSQLite,
		ConnectionString: connectionString,
	}, nil
}
