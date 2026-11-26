package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cloudreve-s3/internal/cloudreve"
	"cloudreve-s3/internal/config"
	"cloudreve-s3/internal/gateway"
)

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: cloudreve-s3 <login|serve|upload-direct> -config gateway.json [upload-direct: -file FILE -bucket BUCKET -key KEY [-overwrite] [-verify]]")
	}
	command := os.Args[1]
	if command != "login" && command != "serve" && command != "upload-direct" {
		return errors.New("command must be login, serve or upload-direct")
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	name := f.String("config", "gateway.json", "configuration JSON file")
	stdin := f.Bool("password-stdin", false, "read Cloudreve password from standard input (login only)")
	filename := f.String("file", "", "existing local source file (upload-direct only)")
	bucket := f.String("bucket", "", "destination bucket (upload-direct only)")
	key := f.String("key", "", "destination object key; defaults to source filename (upload-direct only)")
	overwrite := f.Bool("overwrite", false, "allow replacing an existing object (upload-direct only)")
	verify := f.Bool("verify", false, "download directly after upload and compare SHA-256 (upload-direct only)")
	if e := f.Parse(os.Args[2:]); e != nil {
		return errors.New("invalid command arguments")
	}
	c, e := config.Load(*name)
	if e != nil {
		return e
	}
	client := cloudreve.New(c.Site, c.SessionFile, time.Duration(c.TimeoutSeconds)*time.Second)
	if command == "upload-direct" {
		if *filename == "" || f.NArg() != 0 {
			return errors.New("upload-direct requires -file FILE and no positional arguments")
		}
		if *bucket == "" {
			*bucket = c.Bucket
		}
		if *bucket == "" {
			return errors.New("upload-direct requires -bucket in directory or multi-bucket mode")
		}
		if *key == "" {
			*key = filepath.Base(*filename)
		}
		unlock, err := directStateLock(c.UploadDir)
		if err != nil {
			return err
		}
		defer unlock()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		client = cloudreve.NewDirect(c.Site, c.SessionFile, time.Duration(c.TimeoutSeconds)*time.Second)
		if err := client.Load(); err != nil {
			return err
		}
		be := gateway.New(c, client, "") // Native upload does not need S3 credentials.
		if err := be.RecoverWrites(ctx); err != nil {
			return err
		}
		result, err := be.UploadLocalFile(ctx, *bucket, *key, *filename, *overwrite, *verify)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if command == "login" {
		email, password := os.Getenv("CLOUDREVE_EMAIL"), os.Getenv("CLOUDREVE_PASSWORD")
		if *stdin {
			data, e := io.ReadAll(io.LimitReader(os.Stdin, 8193))
			if e != nil || len(data) > 8192 {
				return errors.New("cannot read password")
			}
			password = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		}
		if email == "" || password == "" {
			return errors.New("login requires CLOUDREVE_EMAIL and CLOUDREVE_PASSWORD or -password-stdin")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutSeconds)*time.Second)
		defer cancel()
		if e = client.Login(ctx, email, password); e != nil {
			return e
		}
		fmt.Println("Login succeeded; session saved. Password was not persisted.")
		return nil
	}
	access, secret := os.Getenv("S3_ACCESS_KEY_ID"), os.Getenv("S3_SECRET_ACCESS_KEY")
	if len(access) < 3 || len(secret) < 32 {
		return errors.New("serve requires S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY (at least 32 characters)")
	}
	if e = client.Load(); e != nil {
		return e
	}
	be := gateway.New(c, client, secret)
	// Validate configured roots or discover bucket directories before recovery.
	if e = be.CheckBuckets(context.Background()); e != nil {
		return e
	}
	if e = be.RecoverWrites(context.Background()); e != nil {
		return e
	}
	server, e := gateway.NewServer(be, access, secret)
	if e != nil {
		return errors.New("cannot initialize S3 server")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); server.ShutDown() }()
	fmt.Println("Starting Cloudreve S3 gateway; request details and credentials are not logged.")
	if e = server.ServeMultiPort([]string{c.Listen}); e != nil {
		return errors.New("S3 listener stopped or could not bind")
	}
	return nil
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
