// Package dbtool runs the MariaDB and MySQL command line programs. lockbox
// shells out to the vendor tools on purpose: they are the same programs a
// database administrator would run by hand, so a dump made by lockbox is a
// perfectly ordinary dump that anybody can restore without lockbox.
package dbtool

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/romano-pavan/lockbox/internal/config"
)

// Tools holds the resolved paths of the two programs lockbox needs.
type Tools struct {
	Dump   string // mariadb-dump or mysqldump
	Client string // mariadb or mysql
}

// Find locates the dump and client programs, preferring the MariaDB names and
// falling back to the MySQL ones, since both projects ship both spellings.
func Find(dbType string) (Tools, error) {
	dumpNames := []string{"mariadb-dump", "mysqldump"}
	clientNames := []string{"mariadb", "mysql"}
	if dbType == "mysql" {
		dumpNames = []string{"mysqldump", "mariadb-dump"}
		clientNames = []string{"mysql", "mariadb"}
	}

	tools := Tools{}
	for _, name := range dumpNames {
		if path, err := exec.LookPath(name); err == nil {
			tools.Dump = path
			break
		}
	}
	for _, name := range clientNames {
		if path, err := exec.LookPath(name); err == nil {
			tools.Client = path
			break
		}
	}
	if tools.Dump == "" {
		return tools, fmt.Errorf("neither %s is installed, try: apt install mariadb-client",
			strings.Join(dumpNames, " nor "))
	}
	return tools, nil
}

// connectionArgs turns the configuration into command line arguments shared by
// the dump program and the client program.
//
// When the database is local and no defaults file is given, no user or password
// is passed at all. The tools then connect over the local socket and the
// operating system vouches for the identity of the caller, which is why lockbox
// needs no database password on a normal single server installation.
func connectionArgs(cfg *config.Config) []string {
	var args []string
	if cfg.Database.DefaultsFile != "" {
		args = append(args, "--defaults-extra-file="+cfg.Database.DefaultsFile)
	}
	if !isLocal(cfg.Database.Host) {
		args = append(args, "--host="+cfg.Database.Host)
		args = append(args, "--port="+strconv.Itoa(cfg.Database.Port))
	}
	return args
}

func isLocal(host string) bool {
	return host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Ping runs a trivial query to prove the database is up and reachable.
func Ping(cfg *config.Config, tools Tools) error {
	if tools.Client == "" {
		return fmt.Errorf("no database client program found")
	}
	args := append(connectionArgs(cfg), "--batch", "--skip-column-names", "--execute=SELECT 1")

	cmd := exec.Command(tools.Client, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s", firstLine(stderr.String(), err))
	}
	if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("unexpected answer from the database: %q", strings.TrimSpace(string(out)))
	}
	return nil
}

// EstimateSize asks the database how many bytes its tables occupy. The answer
// is approximate, which is fine: it only feeds the cost estimate.
func EstimateSize(cfg *config.Config, tools Tools) (int64, error) {
	query := "SELECT IFNULL(SUM(data_length + index_length), 0) FROM information_schema.tables"
	if cfg.Database.Name != "" {
		query += " WHERE table_schema = '" + cfg.Database.Name + "'"
	} else {
		query += " WHERE table_schema NOT IN ('mysql','information_schema','performance_schema','sys')"
	}
	args := append(connectionArgs(cfg), "--batch", "--skip-column-names", "--execute="+query)

	cmd := exec.Command(tools.Client, args...)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// ListEngines reports which storage engines are in use. MyISAM matters: the
// consistent snapshot that lockbox relies on covers transactional tables only,
// so the user deserves a warning rather than a silently partial backup.
func ListEngines(cfg *config.Config, tools Tools) ([]string, error) {
	query := "SELECT DISTINCT engine FROM information_schema.tables WHERE engine IS NOT NULL"
	if cfg.Database.Name != "" {
		query += " AND table_schema = '" + cfg.Database.Name + "'"
	} else {
		query += " AND table_schema NOT IN ('mysql','information_schema','performance_schema','sys')"
	}
	args := append(connectionArgs(cfg), "--batch", "--skip-column-names", "--execute="+query)

	cmd := exec.Command(tools.Client, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var engines []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			engines = append(engines, line)
		}
	}
	return engines, nil
}

// Dump writes a logical backup to w.
//
// --single-transaction is the important flag. It takes a consistent snapshot
// inside one transaction, so applications keep reading and writing normally
// while the dump runs. Stopping the database is never necessary for InnoDB.
func Dump(cfg *config.Config, tools Tools, w io.Writer) error {
	args := connectionArgs(cfg)
	args = append(args,
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--default-character-set=utf8mb4",
	)
	if cfg.Database.Name != "" {
		args = append(args, "--databases", cfg.Database.Name)
	} else {
		args = append(args, "--all-databases")
	}

	cmd := exec.Command(tools.Dump, args...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dump failed: %s", firstLine(stderr.String(), err))
	}
	return nil
}

// Restore feeds a plain SQL stream back into the database.
func Restore(cfg *config.Config, tools Tools, r io.Reader) error {
	if tools.Client == "" {
		return fmt.Errorf("no database client program found")
	}
	cmd := exec.Command(tools.Client, connectionArgs(cfg)...)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore failed: %s", firstLine(stderr.String(), err))
	}
	return nil
}

// firstLine picks the most useful line out of a program's error output.
func firstLine(stderr string, fallback error) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Warning:") {
			continue
		}
		return line
	}
	return fallback.Error()
}
