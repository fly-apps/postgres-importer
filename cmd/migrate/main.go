package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

type migrationOpts struct {
	sourceURI string
	targetURI string
	noOwner   bool
	clean     bool
	create    bool
	dataOnly  bool
}

func main() {
	ctx := context.Background()
	log.SetFlags(0)

	noOwner := flag.Bool("no-owner", true, "")
	clean := flag.Bool("clean", true, "")
	create := flag.Bool("create", true, "")
	dataOnly := flag.Bool("data-only", false, "")

	flag.Parse()

	sourceURI := os.Getenv("SOURCE_DATABASE_URI")
	if sourceURI == "" {
		log.Printf("[error] SOURCE_DATABASE_URI secret must be set")
		os.Exit(1)
		return
	}

	var targetURI string
	operatorPass := os.Getenv("OPERATOR_PASSWORD")
	appName := os.Getenv("FLY_APP_NAME")

	if appName != "" {
		targetURI = fmt.Sprintf("postgres://postgres:%s@%s.internal:5432", operatorPass, appName)
		if operatorPass == "" {
			log.Println("[error] OPERATOR_PASSWORD secret must be set when FLY_APP_NAME is set")
			os.Exit(1)
			return
		}
	}

	targetURI = os.Getenv("TARGET_DATABASE_URI")

	if targetURI == "" {
		log.Println("[error] FLY_APP_NAME or TARGET_DATABASE_URI environment variable must be set")
		os.Exit(1)
		return
	}


	opts := migrationOpts{
		sourceURI: sourceURI,
		targetURI: targetURI,
		noOwner:   *noOwner,
		clean:     *clean,
		create:    *create,
		dataOnly:  *dataOnly,
	}

	log.Println("[info] Running pre-checks...")
	if err := runPreChecks(ctx, opts); err != nil {
		log.Printf("[error] %s", err)
		os.Exit(1)
		return
	}
	log.Println("[info] Pre-checks completed without issue")

	log.Println("[info] Starting import process... (This could take a while)")
	if err := runMigration(ctx, opts); err != nil {
		log.Printf("[error] %s", err)
		os.Exit(1)
		return
	}
	log.Println("[info] Import complete!")
}

func runPreChecks(ctx context.Context, opts migrationOpts) error {
	// Verify source URI specifies a database.
	sourceConf, err := pgx.ParseConfig(opts.sourceURI)
	if err != nil {
		return fmt.Errorf("failed to parse source uri: %s", err)
	}
	if sourceConf.Database == "" {
		return fmt.Errorf("source-uri must contain a database reference (e.g. postgres://<user>:<pass>@<host>:<port>/<database>)")
	}

	// Check source connectivity
	sourceConn, err := openConnection(ctx, opts.sourceURI)
	if err != nil {
		return fmt.Errorf("failed to connect to source: %s", err)
	}
	defer func() { _ = sourceConn.Close(ctx) }()

	// Check target connectivity
	targetConn, err := openConnection(ctx, opts.targetURI)
	if err != nil {
		return fmt.Errorf("failed to connect to target: %s", err)
	}
	defer func() { _ = targetConn.Close(ctx) }()

	// Verify source version is not greater than the target
	var sourceVersion string
	if err := sourceConn.QueryRow(ctx, "SHOW server_version;").Scan(&sourceVersion); err != nil {
		return fmt.Errorf("failed to query source version: %s", err)
	}
	log.Println("[info] Source Postgres version: " + sourceVersion)

	var targetVersion string
	if err := targetConn.QueryRow(ctx, "SHOW server_version;").Scan(&targetVersion); err != nil {
		return fmt.Errorf("failed to query target version: %s", err)
	}
	log.Println("[info] Target Postgres version: " + targetVersion)

	sourceSlice := strings.Split(sourceVersion, ".")
	targetSlice := strings.Split(targetVersion, ".")

	if sourceSlice[0] > targetSlice[0] {
		return fmt.Errorf("source is running a more recent version than target. expected >= %s, got %s", targetVersion, sourceVersion)
	}

	return nil
}

func runMigration(ctx context.Context, opts migrationOpts) error {
	dumpStr := fmt.Sprintf("pg_dump -d %s", opts.sourceURI)
	if opts.noOwner {
		dumpStr = dumpStr + " --no-owner"
	}
	if opts.clean {
		dumpStr = dumpStr + " --clean"
	}
	if opts.create {
		dumpStr = dumpStr + " --create"
	}
	if opts.dataOnly {
		dumpStr = dumpStr + " --data-only"
	}

	restoreStr := fmt.Sprintf("psql -d %s", opts.targetURI)
	cmd := fmt.Sprintf("%s | %s", dumpStr, restoreStr)

	if _, err := runCommand(cmd); err != nil {
		return fmt.Errorf("failed to import database: %s", err)
	}

	return nil
}

func openConnection(parentCtx context.Context, uri string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()

	conf, err := pgx.ParseConfig(uri)
	if err != nil {
		return nil, fmt.Errorf("failed to parse uri: %s", err)
	}

	conf.ConnectTimeout = 5 * time.Second

	return pgx.ConnectConfig(ctx, conf)
}

func runCommand(cmdStr string) ([]byte, error) {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{}

	return cmd.Output()
}
