// Package all links every built-in driver into the binary.
package all

import (
	_ "rowsmith/internal/driver/bigquery"
	_ "rowsmith/internal/driver/mysql"
	_ "rowsmith/internal/driver/postgres"
)
