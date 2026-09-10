// Command lockbox makes immutable, offsite backups of a MySQL or MariaDB
// database on Amazon Simple Storage Service, so that a compromised server
// cannot destroy its own backup history.
//
// It has no external dependencies. Request signing, storage calls and identity
// management are all implemented against the Go standard library.
package main

import (
	"fmt"
	"os"

	"github.com/romano-pavan/lockbox/internal/app"
	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/config"
)

// version is stamped into release builds with:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0"

// main dispatches to one command and turns its error into an exit status.
// Zero means success, one means failure, which is what a scheduler such as
// systemd needs in order to know whether to raise an alarm.
func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error

	switch os.Args[1] {
	case "init":
		err = app.Init(os.Args[2:])
	case "backup":
		err = app.Backup(os.Args[2:])
	case "restore":
		err = app.Restore(os.Args[2:])
	case "doctor":
		err = app.Doctor(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("lockbox %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "\nlockbox: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Printf(`lockbox %s — immutable offsite database backups on Amazon S3

Usage:
  lockbox <command> [options]

Commands:
  init      Create the bucket, turn on Object Lock, and issue an upload only key
  backup    Dump the database, upload it, and verify that it arrived
  restore   List stored backups, or load one back into the database
  doctor    Check the whole chain, including that deletion is refused
  version   Print the version
  help      Print this message

Typical first run:
  export AWS_ACCESS_KEY_ID=...        # an administrator key, used once
  export AWS_SECRET_ACCESS_KEY=...
  lockbox init --bucket my-unique-name --days 1
  lockbox backup
  lockbox doctor

Files:
  %s   settings
  %s   the upload only key created by init

Environment:
  LOCKBOX_CONFIG        override the settings path
  LOCKBOX_CREDENTIALS   override the key path
  AWS_ACCESS_KEY_ID     used before either file, handy for init and restore
  AWS_SECRET_ACCESS_KEY

Documentation: https://github.com/romano-pavan/lockbox
`, version, config.DefaultPath, awsx.DefaultCredentialsPath)
}
