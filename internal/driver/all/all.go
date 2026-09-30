// Package all links every built-in driver into the binary.
package all

import (
	_ "rowsmith/internal/driver/mongodb"
	_ "rowsmith/internal/driver/mssql"
	_ "rowsmith/internal/driver/bigquery"
	_ "rowsmith/internal/driver/mysql"
	_ "rowsmith/internal/driver/oracle"
	_ "rowsmith/internal/driver/postgres"
	_ "rowsmith/internal/driver/sqlite"
)
