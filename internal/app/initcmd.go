package app

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/config"
	"github.com/romano-pavan/lockbox/internal/dbtool"
)

// Init prepares everything on the Amazon side and leaves the machine holding a
// key that can only ever add data.
//
// It runs with an administrator key that the user supplies once. At the end it
// tells the user to delete that key. This is the whole security model in one
// sentence: powerful credentials are used for a minute and thrown away, and
// what stays behind cannot delete, cannot read, and cannot weaken anything.
func Init(args []string) error {
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	bucket := flags.String("bucket", "", "globally unique bucket name to create")
	region := flags.String("region", "eu-central-1", "Amazon region")
	days := flags.Int("days", 30, "how many days an uploaded backup cannot be deleted")
	mode := flags.String("mode", "GOVERNANCE", "GOVERNANCE for testing, COMPLIANCE for production")
	database := flags.String("database", "", "database to back up, empty means all databases")
	dbType := flags.String("db-type", "mariadb", "mariadb or mysql")
	dbHost := flags.String("db-host", "localhost", "database host")
	dbPort := flags.Int("db-port", 3306, "database port")
	defaultsFile := flags.String("defaults-file", "", "optional my.cnf style file with database user and password")
	prefix := flags.String("prefix", "lockbox", "folder inside the bucket")
	assumeYes := flags.Bool("yes", false, "do not ask for confirmation")
	flags.Parse(args)

	if *bucket == "" {
		*bucket = ask("Bucket name to create (must be globally unique)", "")
	}
	if *bucket == "" {
		return fmt.Errorf("a bucket name is required")
	}

	cfg := &config.Config{
		Database: config.Database{
			Type:         *dbType,
			Host:         *dbHost,
			Port:         *dbPort,
			Name:         *database,
			DefaultsFile: *defaultsFile,
		},
		Storage: config.Storage{
			Bucket: *bucket,
			Region: *region,
			Prefix: *prefix,
		},
		Retention: config.Retention{
			Days: *days,
			Mode: strings.ToUpper(*mode),
		},
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	adminCreds, err := awsx.LoadCredentials()
	if err != nil {
		return err
	}
	client := awsx.NewClient(adminCreds, cfg.Storage.Region, cfg.Storage.Endpoint)

	identity, err := client.WhoAmI()
	if err != nil {
		return fmt.Errorf("the credentials from %s do not work: %w", adminCreds.Source, err)
	}

	// Show the plan, including what it will cost and what cannot be undone,
	// before anything irreversible happens.
	fmt.Println("Plan")
	fmt.Printf("  Account:    %s\n", identity.Account)
	fmt.Printf("  Identity:   %s\n", identity.Arn)
	fmt.Printf("  Bucket:     %s in %s\n", cfg.Storage.Bucket, cfg.Storage.Region)
	fmt.Printf("  Lock:       %s mode, %s\n", cfg.Retention.Mode, plural(cfg.Retention.Days, "day"))
	fmt.Printf("  Database:   %s on %s\n", cfg.DatabaseLabel(), cfg.Database.Host)
	fmt.Printf("  New user:   %s\n", writerUserName(cfg.Storage.Bucket))
	printCostEstimate(cfg)

	if cfg.Retention.Mode == "COMPLIANCE" {
		fmt.Println("\n  COMPLIANCE mode means no one, including the Amazon account root user,")
		fmt.Printf("  can delete an uploaded backup for %d days. There is no undo.\n", cfg.Retention.Days)
	}

	if !confirm("\nCreate this?", *assumeYes) {
		return fmt.Errorf("cancelled")
	}

	fmt.Println("\nCreating")

	// Repeating init after a failure part way through must not be a dead end,
	// so a bucket this account already owns is reused rather than rejected.
	switch err := client.CreateBucket(cfg.Storage.Bucket); {
	case err == nil:
		fmt.Printf("  bucket %s created with Object Lock enabled\n", cfg.Storage.Bucket)
	case awsx.ErrorCode(err) == "BucketAlreadyOwnedByYou":
		fmt.Printf("  bucket %s already exists in this account, reusing it\n", cfg.Storage.Bucket)
	case awsx.ErrorCode(err) == "BucketAlreadyExists":
		return fmt.Errorf("the name %s is taken by another Amazon customer, choose a different one", cfg.Storage.Bucket)
	default:
		return fmt.Errorf("could not create the bucket: %w", err)
	}

	if err := client.PutBucketVersioning(cfg.Storage.Bucket); err != nil {
		return fmt.Errorf("could not enable versioning: %w", err)
	}
	fmt.Println("  versioning enabled")

	if err := client.PutPublicAccessBlock(cfg.Storage.Bucket); err != nil {
		return fmt.Errorf("could not block public access: %w", err)
	}
	fmt.Println("  public access blocked")

	if err := client.PutBucketEncryption(cfg.Storage.Bucket); err != nil {
		return fmt.Errorf("could not enable encryption: %w", err)
	}
	fmt.Println("  server side encryption enabled")

	if err := client.PutObjectLockConfiguration(cfg.Storage.Bucket, cfg.Retention.Mode, cfg.Retention.Days); err != nil {
		return fmt.Errorf("could not set the default retention: %w", err)
	}
	fmt.Printf("  default retention set to %s, %s\n", cfg.Retention.Mode, plural(cfg.Retention.Days, "day"))

	if err := client.PutBucketLifecycle(cfg.Storage.Bucket, cfg.Retention.Days); err != nil {
		return fmt.Errorf("could not set the lifecycle rule: %w", err)
	}
	fmt.Printf("  lifecycle rule set to expire objects after %d days\n", cfg.Retention.Days+5)

	user := writerUserName(cfg.Storage.Bucket)
	exists, path, err := client.UserInfo(user)
	if err != nil {
		return fmt.Errorf("could not check whether user %s exists: %w", user, err)
	}
	switch {
	case exists && path != "/lockbox/":
		return fmt.Errorf("user %s already exists outside the /lockbox/ path, refusing to touch an identity that may belong to something else", user)
	case exists:
		fmt.Printf("  user %s already exists, reusing it\n", user)
	default:
		if err := client.CreateUser(user); err != nil {
			return fmt.Errorf("could not create the user: %w", err)
		}
		fmt.Printf("  user %s created\n", user)
	}

	if err := client.PutUserPolicy(user, "lockbox-write-only", awsx.WriterPolicy(cfg.Storage.Bucket)); err != nil {
		return fmt.Errorf("could not attach the policy: %w", err)
	}
	fmt.Println("  write only policy attached")

	writerCreds, err := client.CreateAccessKey(user)
	if err != nil {
		return fmt.Errorf("could not create an access key: %w", err)
	}
	fmt.Println("  access key created")

	configPath := config.Path()
	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("could not write %s: %w", configPath, err)
	}
	fmt.Printf("  configuration written to %s\n", configPath)

	credPath := credentialsPath()
	if err := writeCredentials(credPath, writerCreds, cfg.Storage.Region); err != nil {
		return fmt.Errorf("could not write %s: %w", credPath, err)
	}
	fmt.Printf("  upload key written to %s\n", credPath)

	fmt.Printf(`
Done.

Next, in this order:

  1. Drop the administrator key from this shell, so that everything below
     runs as the restricted key that was just created:

       unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY

  2. Take the first backup:

       lockbox backup

  3. Check the whole chain, including that this machine is refused when it
     tries to delete or read stored backups:

       lockbox doctor

  4. Test a restore on a spare machine before you trust any of this. Use an
     administrator key for that, because the key written above deliberately
     cannot read what it uploads.

  5. Once the restore has worked, delete the administrator key itself in the
     Amazon console, under Identity and Access Management, Users, Security
     credentials. It is the only credential in this account that can undo any
     of the above, and it has no reason to stay on this machine.
     Its identity was: %s
`, identity.Arn)

	return nil
}

// writerUserName derives the Identity and Access Management user name, keeping
// inside the sixty four character limit Amazon enforces.
func writerUserName(bucket string) string {
	name := "lockbox-" + bucket + "-writer"
	if strings.HasPrefix(bucket, "lockbox-") {
		name = bucket + "-writer"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func credentialsPath() string {
	if p := os.Getenv("LOCKBOX_CREDENTIALS"); p != "" {
		return p
	}
	return awsx.DefaultCredentialsPath
}

// writeCredentials stores the new key readable only by its owner.
func writeCredentials(path string, creds awsx.Credentials, region string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(awsx.RenderCredentialsFile(creds, region)), 0o600)
}

// printCostEstimate gives a rough monthly figure so nobody locks a terabyte in
// for ten years by accident. Amazon prices change, so this is an estimate and
// says so.
func printCostEstimate(cfg *config.Config) {
	tools, err := dbtool.Find(cfg.Database.Type)
	if err != nil {
		return
	}
	raw, err := dbtool.EstimateSize(cfg, tools)
	if err != nil || raw == 0 {
		return
	}
	// A logical dump compresses to roughly a quarter of the table data.
	perBackup := raw / 4
	total := perBackup * int64(cfg.Retention.Days)
	gigabytes := float64(total) / (1024 * 1024 * 1024)

	fmt.Printf("  Estimate:   about %s per backup\n", awsx.FormatSize(perBackup))
	fmt.Printf("              about %s held at once, at one backup per day for %s\n",
		awsx.FormatSize(total), plural(cfg.Retention.Days, "day"))

	dollars := gigabytes * 0.0135
	if dollars < 0.01 {
		fmt.Println("              under one US cent per month at infrequent access rates")
	} else {
		fmt.Printf("              roughly %.2f US dollars per month at infrequent access rates\n", dollars)
	}
	fmt.Println("              Every backup is a separate object with its own storage charge,")
	fmt.Println("              and they add up for as long as the retention keeps them. Backing")
	fmt.Println("              up more often multiplies this figure by the number of runs per day.")
	fmt.Println("              Rough estimate only. Watch the real bill in the Amazon console")
	fmt.Println("              under Billing and Cost Management, and set a Budget alert there.")
}
