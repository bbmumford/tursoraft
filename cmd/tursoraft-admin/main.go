// Command tursoraft-admin is the operator's tool for the databases a Manager
// opens. It runs on an operator's machine, never at an endpoint's boot:
//
//	tursoraft-admin ensure -org ORG -group GROUP -db NAME
//	    Creates the database in GROUP when it does not exist, refuses one that
//	    exists in another group, and prints the url for the signed document.
//	    Reads the organisation token from TURSO_API_TOKEN.
//	tursoraft-admin token -org ORG -db NAME [-expiration 1d] [-read-only]
//	    Mints a token scoped to that one database and prints it. It expires
//	    after one day unless -expiration names another lifetime; a token that
//	    never expires, such as a host's static token, is asked for with
//	    -expiration never.
//	    Reads the organisation token from TURSO_API_TOKEN.
//	tursoraft-admin fence-adopt -url URL -cluster ID -confirm-old-cluster-gone
//	    Moves every fence row of the database to the cluster ID given, with
//	    epoch 0, so the jobs of a cluster formed again from empty Raft state
//	    can write. Reads the database token from TURSO_DB_TOKEN.
//
// Secrets come from the environment, never from flags, so they do not appear
// in a process listing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bbmumford/tursoraft/fence"
	"github.com/bbmumford/tursoraft/internal/turso"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main with its inputs and outputs passed in. It returns the exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: tursoraft-admin ensure|token|fence-adopt [flags]")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var err error
	switch args[0] {
	case "ensure":
		err = ensure(ctx, args[1:], getenv, stdout, stderr)
	case "token":
		err = token(ctx, args[1:], getenv, stdout, stderr)
	case "fence-adopt":
		err = fenceAdopt(ctx, args[1:], getenv, stdout, stderr)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "tursoraft-admin:", err)
		return 1
	}
	return 0
}

func platform(fs *flag.FlagSet, getenv func(string) string) (*turso.Client, error) {
	org := fs.Lookup("org").Value.String()
	if org == "" {
		return nil, errors.New("-org is required")
	}
	tok := getenv("TURSO_API_TOKEN")
	if tok == "" {
		return nil, errors.New("TURSO_API_TOKEN is not set")
	}
	return &turso.Client{Org: org, Token: tok, BaseURL: getenv("TURSO_API_BASE_URL")}, nil
}

func ensure(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ensure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("org", "", "organisation")
	group := fs.String("group", "", "group the database must be in")
	db := fs.String("db", "", "database name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *group == "" || *db == "" {
		return errors.New("-group and -db are required")
	}
	c, err := platform(fs, getenv)
	if err != nil {
		return err
	}
	d, created, err := c.EnsureDatabase(ctx, *db, *group)
	if err != nil {
		return err
	}
	verb := "exists"
	if created {
		verb = "created"
	}
	fmt.Fprintf(stdout, "%s %s in group %s\nurl: %s\n", *db, verb, *group, d.URL())
	return nil
}

func token(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.String("org", "", "organisation")
	db := fs.String("db", "", "database name")
	expiration := fs.String("expiration", "1d", "token lifetime in the platform's form; never for one that does not expire")
	readOnly := fs.Bool("read-only", false, "limit the token to reads")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return errors.New("-db is required")
	}
	c, err := platform(fs, getenv)
	if err != nil {
		return err
	}
	jwt, err := c.CreateToken(ctx, *db, *expiration, *readOnly)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, jwt)
	return nil
}

func fenceAdopt(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fence-adopt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbURL := fs.String("url", "", "the database's primary url")
	cluster := fs.String("cluster", "", "the cluster id to adopt, from the new cluster's Health")
	confirmed := fs.Bool("confirm-old-cluster-gone", false, "the old cluster's members are stopped and will not return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbURL == "" || *cluster == "" {
		return errors.New("-url and -cluster are required")
	}
	if !*confirmed {
		return errors.New("refusing: a member of the old cluster that returns would write again under its old epochs; stop every member of the old cluster, then pass -confirm-old-cluster-gone")
	}
	n, err := turso.Exec(ctx, *dbURL, getenv("TURSO_DB_TOKEN"), fence.AdoptSQL, *cluster)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "adopted cluster %s on %d fence rows\n", *cluster, n)
	return nil
}
