// Package app implements the four commands lockbox offers.
package app

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/romano-pavan/lockbox/internal/awsx"
	"github.com/romano-pavan/lockbox/internal/config"
)

// load reads the configuration and builds a signed client for it.
func load() (*config.Config, *awsx.Client, error) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return nil, nil, err
	}
	creds, err := awsx.LoadCredentials()
	if err != nil {
		return cfg, nil, err
	}
	return cfg, awsx.NewClient(creds, cfg.Storage.Region, cfg.Storage.Endpoint), nil
}

// hostLabel is the machine name used inside the object key, reduced to
// characters that are pleasant in a path.
func hostLabel() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	if short, _, found := strings.Cut(name, "."); found {
		name = short
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// objectPrefix is the folder inside the bucket that holds this machine's backups.
func objectPrefix(cfg *config.Config) string {
	return strings.TrimSuffix(cfg.Storage.Prefix, "/") + "/" + hostLabel() + "/"
}

// timestamp is the sortable, filename safe moment used in object keys.
func timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15-04-05Z")
}

// confirm asks a yes or no question unless the caller already passed --yes.
func confirm(question string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	fmt.Printf("%s [y/N]: ", question)
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// ask reads one line of input with a default value.
func ask(question, fallback string) string {
	if fallback != "" {
		fmt.Printf("%s [%s]: ", question, fallback)
	} else {
		fmt.Printf("%s: ", question)
	}
	reader := bufio.NewReader(os.Stdin)
	answer, err := reader.ReadString('\n')
	if err != nil {
		return fallback
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return fallback
	}
	return answer
}
