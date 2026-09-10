package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFullFile(t *testing.T) {
	path := writeTemp(t, `# a comment
database:
  type: mariadb
  host: localhost
  port: 3307
  name: erp          # inline comment

storage:
  bucket: adria-trade-erp-backup-2026
  region: eu-central-1
  prefix: lockbox

retention:
  days: 7
  mode: compliance
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Port != 3307 || cfg.Database.Name != "erp" {
		t.Errorf("database section wrong: %+v", cfg.Database)
	}
	if cfg.Storage.Bucket != "adria-trade-erp-backup-2026" {
		t.Errorf("bucket wrong: %q", cfg.Storage.Bucket)
	}
	if cfg.Retention.Days != 7 || cfg.Retention.Mode != "COMPLIANCE" {
		t.Errorf("retention wrong: %+v", cfg.Retention)
	}
}

func TestDefaultsFillIn(t *testing.T) {
	path := writeTemp(t, "storage:\n  bucket: only-the-bucket-is-required\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Type != "mariadb" || cfg.Database.Host != "localhost" || cfg.Database.Port != 3306 {
		t.Errorf("database defaults wrong: %+v", cfg.Database)
	}
	if cfg.Retention.Days != 30 || cfg.Retention.Mode != "GOVERNANCE" {
		t.Errorf("retention defaults wrong: %+v", cfg.Retention)
	}
	if cfg.DatabaseLabel() != "all databases" {
		t.Errorf("empty database name should mean all databases")
	}
}

func TestRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"missing bucket":    "database:\n  type: mariadb\n",
		"uppercase bucket":  "storage:\n  bucket: NoUpperCaseAllowed\n",
		"short bucket":      "storage:\n  bucket: ab\n",
		"bad mode":          "storage:\n  bucket: fine-bucket-name\nretention:\n  mode: whatever\n",
		"bad type":          "storage:\n  bucket: fine-bucket-name\ndatabase:\n  type: postgres\n",
		"bad port":          "storage:\n  bucket: fine-bucket-name\ndatabase:\n  port: 99999\n",
		"port not a number": "storage:\n  bucket: fine-bucket-name\ndatabase:\n  port: soon\n",
	}
	for name, contents := range cases {
		if _, err := Load(writeTemp(t, contents)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestMissingFileMentionsInit(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err == nil || !strings.Contains(err.Error(), "lockbox init") {
		t.Fatalf("the error should point at 'lockbox init', got: %v", err)
	}
}

func TestRenderRoundTrip(t *testing.T) {
	original := &Config{
		Database:  Database{Type: "mysql", Host: "10.0.0.5", Port: 3306, Name: "erp"},
		Storage:   Storage{Bucket: "round-trip-bucket", Region: "eu-west-1", Prefix: "lockbox"},
		Retention: Retention{Days: 14, Mode: "COMPLIANCE"},
	}
	path := writeTemp(t, original.Render())

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *reloaded != *original {
		t.Fatalf("round trip changed the configuration\n got %+v\nwant %+v", reloaded, original)
	}
}

func TestBucketNameRules(t *testing.T) {
	good := []string{"abc", "adria-trade-erp-backup-2026", "a.b.c"}
	bad := []string{"ab", "-leading", "trailing-", "UPPER", "under_score", "double..dot"}

	for _, name := range good {
		if err := ValidateBucketName(name); err != nil {
			t.Errorf("%q should be valid: %v", name, err)
		}
	}
	for _, name := range bad {
		if err := ValidateBucketName(name); err == nil {
			t.Errorf("%q should be rejected", name)
		}
	}
}
