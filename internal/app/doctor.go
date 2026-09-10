package app

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/config"
	"github.com/romano-pavan/lockbox/internal/dbtool"
)

// report collects the outcome of every check so the command can end with a
// single verdict and a matching exit status.
type report struct {
	failures int
	warnings int
}

func (r *report) pass(format string, a ...any) {
	fmt.Printf("  \u2713 "+format+"\n", a...)
}

func (r *report) warn(format string, a ...any) {
	r.warnings++
	fmt.Printf("  \u26a0 "+format+"\n", a...)
}

func (r *report) fail(format string, a ...any) {
	r.failures++
	fmt.Printf("  \u2717 "+format+"\n", a...)
}

// Doctor checks every link in the chain, from the database being reachable to
// the storage refusing to let this machine delete its own history.
//
// The two probes at the end are the point of the whole command. A backup tool
// that only reports "upload succeeded" proves nothing about ransomware. These
// probes ask Amazon to delete and to read, and treat a refusal as success.
func Doctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	maxAge := flags.Duration("max-age", 26*time.Hour,
		"how old the newest backup may be before this is reported as a failure")
	flags.Parse(args)

	rep := &report{}

	fmt.Println("Configuration")
	cfg, err := config.Load(config.Path())
	if err != nil {
		rep.fail("%v", err)
		return verdict(rep)
	}
	rep.pass("read from %s", config.Path())
	fmt.Printf("      database %s on %s, bucket %s in %s, %s %s lock\n",
		cfg.DatabaseLabel(), cfg.Database.Host, cfg.Storage.Bucket,
		cfg.Storage.Region, plural(cfg.Retention.Days, "day"), cfg.Retention.Mode)

	fmt.Println("\nDatabase")
	tools, err := dbtool.Find(cfg.Database.Type)
	if err != nil {
		rep.fail("%v", err)
	} else {
		rep.pass("dump program found at %s", tools.Dump)

		if err := dbtool.Ping(cfg, tools); err != nil {
			rep.fail("database does not answer: %v", err)
		} else {
			rep.pass("database answers")

			if size, err := dbtool.EstimateSize(cfg, tools); err == nil && size > 0 {
				rep.pass("roughly %s of table data", awsx.FormatSize(size))
			}
			if engines, err := dbtool.ListEngines(cfg, tools); err == nil {
				var risky []string
				for _, engine := range engines {
					if strings.EqualFold(engine, "MyISAM") || strings.EqualFold(engine, "Aria") {
						risky = append(risky, engine)
					}
				}
				if len(risky) > 0 {
					rep.warn("%s tables are present; the consistent snapshot covers transactional tables only",
						strings.Join(risky, " and "))
				} else {
					rep.pass("all tables use a transactional engine")
				}
			}
		}
	}

	fmt.Println("\nCredentials")
	creds, err := awsx.LoadCredentials()
	if err != nil {
		rep.fail("%v", err)
		return verdict(rep)
	}
	rep.pass("loaded from %s", creds.Source)

	client := awsx.NewClient(creds, cfg.Storage.Region, cfg.Storage.Endpoint)
	// The Security Token Service only exists on real Amazon, so this check is
	// skipped when the user points lockbox at S3 compatible storage.
	if cfg.Storage.Endpoint == "" {
		identity, err := client.WhoAmI()
		if err != nil {
			rep.warn("cannot confirm which identity these keys belong to: %v", err)
		} else {
			rep.pass("identity %s", identity.Arn)
		}
	}

	fmt.Println("\nStorage")
	if err := client.HeadBucket(cfg.Storage.Bucket); err != nil {
		rep.fail("bucket %s is not reachable: %v", cfg.Storage.Bucket, err)
		return verdict(rep)
	}
	rep.pass("bucket %s is reachable", cfg.Storage.Bucket)

	lock, err := client.GetObjectLockConfiguration(cfg.Storage.Bucket)
	switch {
	case err != nil && awsx.IsAccessDenied(err):
		rep.warn("these credentials may not read the lock settings")
	case err != nil:
		rep.fail("cannot read the lock settings: %v", err)
	case !lock.Enabled:
		rep.fail("Object Lock is NOT enabled on this bucket, backups can be deleted")
	case lock.Days == 0:
		rep.fail("Object Lock is enabled but no default retention is set, uploads are not locked")
	default:
		rep.pass("Object Lock active: %s mode, %s", lock.Mode, plural(lock.Days, "day"))
		if lock.Days != cfg.Retention.Days || !strings.EqualFold(lock.Mode, cfg.Retention.Mode) {
			rep.warn("the bucket says %s %d days, the configuration file says %s %d days",
				lock.Mode, lock.Days, cfg.Retention.Mode, cfg.Retention.Days)
		}
		if strings.EqualFold(lock.Mode, "GOVERNANCE") {
			rep.warn("GOVERNANCE mode can be bypassed by a privileged account; use COMPLIANCE in production")
		}
	}

	fmt.Println("\nBackups")
	objects, err := client.ListObjects(cfg.Storage.Bucket, objectPrefix(cfg))
	if err != nil {
		rep.fail("cannot list backups: %v", err)
	} else if len(objects) == 0 {
		rep.warn("no backups from this machine yet, run 'lockbox backup'")
	} else {
		newest := objects[0]
		for _, obj := range objects {
			if obj.LastModified.After(newest.LastModified) {
				newest = obj
			}
		}
		age := time.Since(newest.LastModified)
		line := fmt.Sprintf("%s stored, newest %s old (%s)",
			plural(len(objects), "backup"), age.Round(time.Minute), awsx.FormatSize(newest.Size))
		if age > *maxAge {
			rep.fail("%s — older than the %s limit", line, *maxAge)
		} else {
			rep.pass("%s", line)
		}
		checkReadRefused(rep, client, cfg.Storage.Bucket, newest.Key)
	}

	fmt.Println("\nProtection")
	checkDeleteRefused(rep, client, cfg.Storage.Bucket)

	return verdict(rep)
}

// checkDeleteRefused asks Amazon to delete a name that does not exist. Being
// refused is the correct outcome and the strongest single piece of evidence
// that a compromised server cannot erase its own backups.
func checkDeleteRefused(rep *report, client *awsx.Client, bucket string) {
	probe := "lockbox-permission-probe/does-not-exist"

	err := client.DeleteObject(bucket, probe)
	switch {
	case err == nil:
		rep.fail("these credentials ARE allowed to delete objects — that defeats the protection")
	case awsx.IsAccessDenied(err):
		rep.pass("delete refused by Amazon, as intended")
	default:
		rep.warn("delete probe was inconclusive: %v", err)
	}
}

// checkReadRefused verifies the same key cannot read older backups, which is
// what keeps an attacker from stealing the data as well as encrypting it.
func checkReadRefused(rep *report, client *awsx.Client, bucket, key string) {
	body, _, err := client.GetObject(bucket, key)
	switch {
	case err == nil:
		body.Close()
		rep.warn("these credentials can read stored backups — expected for an administrator key, wrong for the backup server")
	case awsx.IsAccessDenied(err):
		rep.pass("reading stored backups refused, as intended")
	default:
		rep.warn("read probe was inconclusive: %v", err)
	}
}

func verdict(rep *report) error {
	fmt.Println()
	switch {
	case rep.failures > 0:
		return fmt.Errorf("%s failed, %s", plural(rep.failures, "check"), plural(rep.warnings, "warning"))
	case rep.warnings > 0:
		fmt.Printf("All checks passed with %s.\n", plural(rep.warnings, "warning"))
	default:
		fmt.Println("All checks passed.")
	}
	return nil
}

// plural formats a count with its noun, so output reads "1 day" and "2 days".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
