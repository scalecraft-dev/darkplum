package handler

import (
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
)

var uuidRe = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
)

func isValidUUID(s string) bool {
	return uuidRe.MatchString(s)
}

// isUniqueViolation returns true for Postgres error code 23505.
func isUniqueViolation(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23505"
	}
	return false
}

// isForeignKeyViolation returns true for Postgres error code 23503.
func isForeignKeyViolation(err error) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code == "23503"
	}
	return false
}
