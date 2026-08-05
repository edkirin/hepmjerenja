package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// defaultDBPath is where the SQLite database lives when DB_PATH is unset.
const defaultDBPath = "./data/hepmjerenja.db"

// AppTimezone is the timezone every stored timestamp is interpreted in. The
// SQLite queries reach it through the 'localtime' modifier, which reads the
// process TZ — main() pins both that and time.Local, so the value below is the
// single definition of "local time" for the whole application.
const AppTimezone = "Europe/Zagreb"

// Config holds application-wide configuration values loaded from environment variables.
type Config struct {
	// DBPath is the SQLite database file. Its directory is created on startup.
	DBPath string

	// Port is the address the Echo HTTP server will listen on (e.g. ":8000").
	Port string

	// LogDir is the directory where log files are written.
	LogDir string

	// LogLevel controls the minimum log level ("debug", "info", "warn", "error").
	LogLevel string

	// HepUsername and HepPassword are the mjerenje.hep.hr credentials used for
	// every collection. This is a single-user application — the credentials belong
	// to the installation, not to an account in the database.
	HepUsername string
	HepPassword string

	// SQLEcho, when true, prints every SQL query and its arguments to stdout.
	// Intended for local debugging only — output goes directly to the console,
	// not to the structured log.
	SQLEcho bool

	// Debug, when true, disables HTML/CSS/JS minification.
	Debug bool

	// ConfigFile is the settings file that was loaded, empty when none was found.
	ConfigFile string

	// ConfigFileError is set when a settings file was found but could not be
	// parsed. main() logs it — a typo in the file should not pass unnoticed.
	ConfigFileError error
}

// configFiles are the settings files LoadConfig looks for, in order; the first one
// that exists is used. `.env` is still accepted so existing installations keep
// working after the rename to config.ini.
var configFiles = []string{"config.ini", ".env"}

// LoadConfig reads configuration from environment variables, after applying the
// settings file (see configFiles). Real environment variables take precedence, so
// `docker run -e HEP_PASSWORD=…` overrides the file.
func LoadConfig() Config {
	configFile, configErr := loadConfigFile()

	cfg := Config{
		ConfigFile:      configFile,
		ConfigFileError: configErr,
		DBPath:          os.Getenv("DB_PATH"),
		Port:            os.Getenv("PORT"),
		LogDir:          os.Getenv("LOG_DIR"),
		LogLevel:        os.Getenv("LOG_LEVEL"),
		HepUsername:     os.Getenv("HEP_USERNAME"),
		HepPassword:     os.Getenv("HEP_PASSWORD"),
		SQLEcho:         os.Getenv("SQL_ECHO") == "true",
		Debug:           os.Getenv("DEBUG") == "true",
	}

	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	if cfg.LogDir == "" {
		cfg.LogDir = "."
	}

	// Default to port 8000 if not specified.
	if cfg.Port == "" {
		cfg.Port = ":8000"
	}

	// Ensure the port string starts with ":" for Echo's Start() method.
	if cfg.Port[0] != ':' {
		cfg.Port = ":" + cfg.Port
	}

	return cfg
}

// DSN assembles the SQLite connection string. Both the connection pool and the
// goose migration runner use it, so the database is configured in one place.
//
// The pragmas matter: WAL keeps reads from blocking on the writer, busy_timeout
// waits instead of failing when a write is in flight, and foreign_keys enforces
// the metering_point_code references (SQLite leaves them off by default).
func (c Config) DSN() string {
	return "file:" + c.DBPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
}

// HepCredentials returns the configured HEP credentials.
func (c Config) HepCredentials() HepCredentials {
	return HepCredentials{Username: c.HepUsername, Password: c.HepPassword}
}

// loadConfigFile reads KEY=value settings from the first file in configFiles that
// exists and puts them into the environment, leaving variables that are already
// set untouched. Returns the file that was used ("" when none was found).
//
// The format is one KEY=value per line, the same as a .env file. Because the
// documented name is config.ini, the reader also accepts what an .ini file
// normally contains: `;` starts a comment and `[section]` headers are skipped.
// Keys stay flat — the section a key sits under carries no meaning.
func loadConfigFile() (string, error) {
	for _, path := range configFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // missing or unreadable: try the next candidate
		}

		if enc := rejectedEncoding(raw); enc != "" {
			return path, fmt.Errorf("%s is %s; save it as UTF-8 (Notepad: Save as → Encoding: UTF-8)", path, enc)
		}

		values, err := godotenv.Unmarshal(stripINIDecorations(string(raw)))
		if err != nil {
			return path, fmt.Errorf("parse %s: %w", path, err)
		}

		for key, value := range values {
			if _, alreadySet := os.LookupEnv(key); !alreadySet {
				os.Setenv(key, value)
			}
		}
		return path, nil
	}
	return "", nil
}

// stripINIDecorations drops what godotenv cannot read: a leading UTF-8
// byte-order mark, `[section]` headers and `;` comments. Everything else,
// including `#` comments, is left exactly as it was.
//
// The BOM matters on Windows: saving the file from Notepad as "UTF-8 with BOM"
// prepends one, and it would otherwise become part of the first key's name — the
// parser then rejects the whole file and every setting silently falls back to its
// default. Trailing carriage returns from CRLF files are removed here as well, so
// correctness does not depend on godotenv normalising line endings itself.
func stripINIDecorations(contents string) string {
	contents = strings.TrimPrefix(contents, "\ufeff")

	var kept strings.Builder
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		kept.WriteString(line)
		kept.WriteByte('\n')
	}
	return kept.String()
}

// rejectedEncoding names the encoding of a settings file that cannot be read at
// all, or "" when the file looks like UTF-8 (or ASCII). Notepad's "Save as" offers
// UTF-16, which would otherwise fail with a parse error mentioning a stray
// character — this turns that into an instruction the user can act on.
func rejectedEncoding(raw []byte) string {
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		return "UTF-16 LE encoded"
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return "UTF-16 BE encoded"
	default:
		return ""
	}
}
