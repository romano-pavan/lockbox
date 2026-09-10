package app

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/dbtool"
)

// Restore lists stored backups, or streams one of them back into the database.
//
// This command needs permission to read objects, which the key created by
// 'lockbox init' does not have on purpose. Restores are therefore run with an
// administrator key, from a machine of your choosing. That asymmetry is the
// point: the server that writes backups can never read them back.
func Restore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ExitOnError)
	list := flags.Bool("list", false, "only list the available backups")
	host := flags.String("host-label", "", "restore a backup taken on a different machine")
	toFile := flags.String("to-file", "", "write the decompressed dump to a file instead of loading it")
	assumeYes := flags.Bool("yes", false, "do not ask for confirmation")
	flags.Parse(args)

	cfg, client, err := load()
	if err != nil {
		return err
	}

	prefix := objectPrefix(cfg)
	if *host != "" {
		prefix = strings.TrimSuffix(cfg.Storage.Prefix, "/") + "/" + *host + "/"
	}

	objects, err := client.ListObjects(cfg.Storage.Bucket, prefix)
	if err != nil {
		return err
	}
	if len(objects) == 0 {
		return fmt.Errorf("no backups found under s3://%s/%s", cfg.Storage.Bucket, prefix)
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].LastModified.Before(objects[j].LastModified)
	})

	if *list || flags.NArg() == 0 {
		fmt.Printf("Backups in s3://%s/%s\n\n", cfg.Storage.Bucket, prefix)
		for _, obj := range objects {
			name := strings.TrimPrefix(obj.Key, prefix)
			name = strings.TrimSuffix(name, ".sql.gz")
			fmt.Printf("  %-24s  %10s  %s\n", name,
				awsx.FormatSize(obj.Size), obj.LastModified.Local().Format("2006-01-02 15:04"))
		}
		fmt.Printf("\nRestore one with:\n  lockbox restore %s\n",
			strings.TrimSuffix(strings.TrimPrefix(objects[len(objects)-1].Key, prefix), ".sql.gz"))
		return nil
	}

	wanted := flags.Arg(0)
	key := prefix + wanted
	if !strings.HasSuffix(key, ".sql.gz") {
		key += ".sql.gz"
	}

	var chosen *awsx.Object
	for i := range objects {
		if objects[i].Key == key {
			chosen = &objects[i]
			break
		}
	}
	if chosen == nil {
		return fmt.Errorf("no backup named %q, run 'lockbox restore --list' to see what exists", wanted)
	}

	if *toFile == "" {
		fmt.Printf("This will load %s (%s) into %s on %s.\n",
			chosen.Key, awsx.FormatSize(chosen.Size), cfg.DatabaseLabel(), cfg.Database.Host)
		fmt.Println("Existing tables with the same names will be overwritten.")
		if !confirm("Continue?", *assumeYes) {
			return fmt.Errorf("cancelled")
		}
	}

	body, _, err := client.GetObject(cfg.Storage.Bucket, chosen.Key)
	if err != nil {
		if awsx.IsAccessDenied(err) {
			return fmt.Errorf("these credentials may not read backups; use an administrator key for restores (%w)", err)
		}
		return err
	}
	defer body.Close()

	decompressor, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("the stored object is not valid gzip data: %w", err)
	}
	defer decompressor.Close()

	if *toFile != "" {
		out, err := os.Create(*toFile)
		if err != nil {
			return err
		}
		defer out.Close()
		written, err := io.Copy(out, decompressor)
		if err != nil {
			return err
		}
		fmt.Printf("Wrote %s of plain SQL to %s\n", awsx.FormatSize(written), *toFile)
		return nil
	}

	tools, err := dbtool.Find(cfg.Database.Type)
	if err != nil {
		return err
	}
	fmt.Printf("Restoring %s ...\n", chosen.Key)
	if err := dbtool.Restore(cfg, tools, decompressor); err != nil {
		return err
	}
	fmt.Println("Restore finished. Check row counts against what you expect before trusting it.")
	return nil
}
