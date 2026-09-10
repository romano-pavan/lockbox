// Package config loads, validates and writes the lockbox configuration file.
//
// The file format is a deliberately small subset of YAML: top level sections,
// two space indented "key: value" pairs, and "#" comments. A full YAML parser
// is not worth an external dependency for six settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DefaultPath is where lockbox looks for its configuration unless the
// LOCKBOX_CONFIG environment variable points somewhere else.
const DefaultPath = "/etc/lockbox/config.yml"

// Config is the whole configuration file.
type Config struct {
	Database  Database
	Storage   Storage
	Retention Retention
}

// Database describes which database to dump and how to reach it.
type Database struct {
	Type         string // "mariadb" or "mysql"
	Host         string
	Port         int
	Name         string // empty means every database on the server
	DefaultsFile string // optional my.cnf style file holding user and password
}

// Storage describes the Amazon Simple Storage Service (Amazon S3) destination.
// Access keys are deliberately absent, see package awsx for where they live.
type Storage struct {
	Bucket   string
	Region   string
	Prefix   string
	Endpoint string // empty means real Amazon S3, set for S3 compatible storage
}

// Retention describes how long an uploaded object stays locked.
type Retention struct {
	Days int
	Mode string // "GOVERNANCE" or "COMPLIANCE"
}

// Path returns the configuration path, honouring the LOCKBOX_CONFIG override.
func Path() string {
	if p := os.Getenv("LOCKBOX_CONFIG"); p != "" {
		return p
	}
	return DefaultPath
}

// Load reads, parses and validates the configuration. Anything that would fail
// later is rejected here, so every other package may assume sane values.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("configuration file %s not found, run 'lockbox init' first", path)
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	sections, err := parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := &Config{}
	cfg.Database.Type = sections.str("database", "type")
	cfg.Database.Host = sections.str("database", "host")
	cfg.Database.Name = sections.str("database", "name")
	cfg.Database.DefaultsFile = sections.str("database", "defaults_file")
	if cfg.Database.Port, err = sections.num("database", "port"); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg.Storage.Bucket = sections.str("storage", "bucket")
	cfg.Storage.Region = sections.str("storage", "region")
	cfg.Storage.Prefix = sections.str("storage", "prefix")
	cfg.Storage.Endpoint = sections.str("storage", "endpoint")

	cfg.Retention.Mode = sections.str("retention", "mode")
	if cfg.Retention.Days, err = sections.num("retention", "days"); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// applyDefaults fills in values a user may reasonably leave out.
func (c *Config) applyDefaults() {
	if c.Database.Type == "" {
		c.Database.Type = "mariadb"
	}
	if c.Database.Host == "" {
		c.Database.Host = "localhost"
	}
	if c.Database.Port == 0 {
		c.Database.Port = 3306
	}
	if c.Storage.Region == "" {
		c.Storage.Region = "eu-central-1"
	}
	if c.Storage.Prefix == "" {
		c.Storage.Prefix = "lockbox"
	}
	if c.Retention.Mode == "" {
		c.Retention.Mode = "GOVERNANCE"
	}
	if c.Retention.Days == 0 {
		c.Retention.Days = 30
	}
}

// Validate rejects configurations that cannot possibly work.
func (c *Config) Validate() error {
	if c.Database.Type != "mariadb" && c.Database.Type != "mysql" {
		return fmt.Errorf("database.type must be 'mariadb' or 'mysql', got %q", c.Database.Type)
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("database.port must be between 1 and 65535, got %d", c.Database.Port)
	}
	if c.Database.DefaultsFile != "" && !filepath.IsAbs(c.Database.DefaultsFile) {
		return fmt.Errorf("database.defaults_file must be an absolute path")
	}
	if c.Storage.Bucket == "" {
		return fmt.Errorf("storage.bucket is required")
	}
	if err := ValidateBucketName(c.Storage.Bucket); err != nil {
		return err
	}
	if c.Storage.Region == "" {
		return fmt.Errorf("storage.region is required")
	}
	mode := strings.ToUpper(c.Retention.Mode)
	if mode != "GOVERNANCE" && mode != "COMPLIANCE" {
		return fmt.Errorf("retention.mode must be GOVERNANCE or COMPLIANCE, got %q", c.Retention.Mode)
	}
	c.Retention.Mode = mode
	if c.Retention.Days < 1 || c.Retention.Days > 36500 {
		return fmt.Errorf("retention.days must be between 1 and 36500, got %d", c.Retention.Days)
	}
	return nil
}

// ValidateBucketName applies the Amazon S3 naming rules locally, so a typo is
// caught in a millisecond instead of after a round trip to Amazon.
func ValidateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be 3 to 63 characters long, got %d", len(name))
	}
	for _, r := range name {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '-' && r != '.' {
			return fmt.Errorf("bucket name may only contain lowercase letters, digits, hyphens and dots")
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("bucket name may not start or end with a hyphen")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("bucket name may not contain two consecutive dots")
	}
	return nil
}

// DatabaseLabel is a human readable name of what gets dumped.
func (c *Config) DatabaseLabel() string {
	if c.Database.Name == "" {
		return "all databases"
	}
	return c.Database.Name
}

// Render turns the configuration back into file contents. Used by 'lockbox init'.
func (c *Config) Render() string {
	var b strings.Builder
	b.WriteString("# lockbox configuration\n")
	b.WriteString("#\n")
	b.WriteString("# Amazon Web Services access keys are NOT stored here. lockbox reads them\n")
	b.WriteString("# from /etc/lockbox/credentials, from ~/.aws/credentials, or from the\n")
	b.WriteString("# AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables.\n\n")

	b.WriteString("database:\n")
	fmt.Fprintf(&b, "  type: %s\n", c.Database.Type)
	fmt.Fprintf(&b, "  host: %s\n", c.Database.Host)
	fmt.Fprintf(&b, "  port: %d\n", c.Database.Port)
	fmt.Fprintf(&b, "  name: %s\n", c.Database.Name)
	fmt.Fprintf(&b, "  defaults_file: %s\n", c.Database.DefaultsFile)

	b.WriteString("\nstorage:\n")
	fmt.Fprintf(&b, "  bucket: %s\n", c.Storage.Bucket)
	fmt.Fprintf(&b, "  region: %s\n", c.Storage.Region)
	fmt.Fprintf(&b, "  prefix: %s\n", c.Storage.Prefix)
	fmt.Fprintf(&b, "  endpoint: %s\n", c.Storage.Endpoint)

	b.WriteString("\nretention:\n")
	fmt.Fprintf(&b, "  days: %d\n", c.Retention.Days)
	fmt.Fprintf(&b, "  mode: %s\n", c.Retention.Mode)
	return b.String()
}

// Save writes the configuration, creating the parent directory if needed.
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(c.Render()), 0o644)
}

// --- minimal YAML subset parser ---------------------------------------------

type document map[string]map[string]string

func (d document) str(section, key string) string {
	if s, ok := d[section]; ok {
		return s[key]
	}
	return ""
}

func (d document) num(section, key string) (int, error) {
	v := d.str(section, key)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s.%s must be a whole number, got %q", section, key, v)
	}
	return n, nil
}

// Sections returns the section names, sorted. Used by tests.
func (d document) Sections() []string {
	out := make([]string, 0, len(d))
	for k := range d {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func parse(text string) (document, error) {
	doc := document{}
	section := ""

	for i, line := range strings.Split(text, "\n") {
		lineNo := i + 1

		clean := stripComment(line)
		if strings.TrimSpace(clean) == "" {
			continue
		}

		indented := strings.HasPrefix(clean, " ") || strings.HasPrefix(clean, "\t")
		trimmed := strings.TrimSpace(clean)

		if !indented {
			if !strings.HasSuffix(trimmed, ":") {
				return nil, fmt.Errorf("line %d: expected a section such as \"database:\", got %q", lineNo, trimmed)
			}
			section = strings.TrimSuffix(trimmed, ":")
			if _, exists := doc[section]; !exists {
				doc[section] = map[string]string{}
			}
			continue
		}

		if section == "" {
			return nil, fmt.Errorf("line %d: %q is indented but no section was opened above it", lineNo, trimmed)
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			return nil, fmt.Errorf("line %d: expected \"key: value\", got %q", lineNo, trimmed)
		}
		doc[section][strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return doc, nil
}

// stripComment removes a trailing comment, leaving quoted hashes alone.
func stripComment(line string) string {
	inSingle, inDouble := false, false
	for i, r := range line {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:i]
			}
		}
	}
	return line
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
