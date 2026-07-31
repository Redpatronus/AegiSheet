// Command aegisheet collects Google Workspace administrative data and writes it
// to an Excel workbook, one worksheet per configured report section.
//
// This file is deliberately thin: it wires the packages together, owns the
// process-level concerns (signals, exit codes) and does no reporting work of its
// own. Everything it does is ordered so that the cheapest failures happen first —
// configuration is fully validated before a credential is read, and credentials
// are resolved before the first API call.
//
// Exit codes are stable and intended for schedulers:
//
//	0    every configured section succeeded
//	1    the run failed
//	2    the configuration or command line was rejected
//	3    authentication failed
//	4    the report was produced but at least one section failed
//	130  interrupted by a signal
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Redpatronus/AegiSheet/internal/buildinfo"
	"github.com/Redpatronus/AegiSheet/internal/config"
	"github.com/Redpatronus/AegiSheet/internal/gsuite"
	"github.com/Redpatronus/AegiSheet/internal/logging"
	"github.com/Redpatronus/AegiSheet/internal/runner"
	"github.com/Redpatronus/AegiSheet/internal/xlsx"
)

// Process exit codes. See the package comment.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsage       = 2
	exitAuth        = 3
	exitPartial     = 4
	exitInterrupted = 130
)

func main() {
	// All real work happens in run so that deferred cleanup (closing the log file,
	// releasing the workbook's temporary files) still executes before the process
	// exits.
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp),
			errors.Is(err, config.ErrVersionRequested),
			errors.Is(err, config.ErrTasksRequested),
			errors.Is(err, config.ErrConfigRequested):
			// The operator asked for information and Load already printed it.
			return exitOK
		default:
			// The logger does not exist yet, and a configuration error is a message
			// for the person at the terminal.
			fmt.Fprintf(os.Stderr, "%s: configuration error:\n%v\n", buildinfo.AppName, err)
			return exitUsage
		}
	}

	logger, logFile, err := logging.New(cfg.Log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot initialise logging: %v\n", buildinfo.AppName, err)
		return exitUsage
	}
	defer func() { _ = logFile.Close() }()

	// A signal cancels the context, which propagates into every in-flight HTTP
	// request, so an interrupted run stops in seconds instead of finishing its
	// current API sweep.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if cfg.Run.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, cfg.Run.Timeout)
		defer cancelTimeout()
	}

	return execute(ctx, cfg, logger)
}

// execute performs the run and translates its outcome into an exit code.
func execute(ctx context.Context, cfg *config.Config, logger *slog.Logger) int {
	info := buildinfo.Get()
	logger.Info("aegisheet starting",
		slog.String("version", info.Version),
		slog.String("commit", info.Commit),
		slog.String("go_version", info.GoVersion),
		slog.String("platform", info.Platform))
	logger.LogAttrs(ctx, slog.LevelInfo, "configuration resolved", cfg.LogAttrs()...)

	// Configuration warnings are collected during loading, before the logger
	// exists, and emitted here.
	for _, warning := range cfg.Warnings {
		logger.Warn("configuration warning", slog.String("detail", warning))
	}

	client, err := gsuite.NewHTTPClient(ctx, cfg.Auth, cfg.API, logger)
	if err != nil {
		logger.Error("authentication failed", slog.String("error", err.Error()))
		return exitAuth
	}

	services, err := gsuite.NewServices(ctx, client, cfg.API, logger)
	if err != nil {
		logger.Error("cannot initialise Google API clients", slog.String("error", err.Error()))
		return exitFailure
	}

	book, err := xlsx.New(logger)
	if err != nil {
		logger.Error("cannot create workbook", slog.String("error", err.Error()))
		return exitFailure
	}
	defer func() {
		if err := book.Close(); err != nil {
			logger.Warn("releasing workbook resources failed", slog.String("error", err.Error()))
		}
	}()

	summary, runErr := runner.New(cfg, services, book, logger).Run(ctx)
	logger.LogAttrs(ctx, slog.LevelInfo, "report run finished", summary.LogAttrs()...)

	if runErr != nil {
		// The run was abandoned. The workbook is deliberately not written: replacing
		// a complete previous report with a partial one loses information that the
		// operator may need.
		if errors.Is(runErr, context.Canceled) {
			logger.Warn("interrupted; the workbook was not written",
				slog.String("output", cfg.Output.Path))
			return exitInterrupted
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			logger.Error("the run exceeded its time budget; the workbook was not written",
				slog.String("timeout", cfg.Run.Timeout.String()))
			return exitFailure
		}
		logger.Error("run abandoned", slog.String("error", runErr.Error()))
		return exitFailure
	}

	// Every section failed: there is nothing worth writing, and the cause is
	// systemic (wrong credentials, missing scopes, wrong customer).
	if summary.Succeeded == 0 {
		logger.Error("every section failed; the workbook was not written",
			slog.String("error", summary.Err().Error()))
		return exitFailure
	}

	if cfg.Output.DryRun {
		logger.Info("dry run: the workbook was not written",
			slog.Int("sheets", len(book.Sheets())), slog.Int("rows", summary.Rows()))
	} else if err := book.Save(cfg.Output.Path, cfg.Output.FileMode, cfg.Output.Overwrite); err != nil {
		logger.Error("cannot write the workbook",
			slog.String("output", cfg.Output.Path), slog.String("error", err.Error()))
		return exitFailure
	}

	if summary.Failed > 0 {
		// A partial report is worth keeping, but the exit code has to say so, or a
		// scheduled run would report success while a sheet is missing.
		logger.Warn("the report is incomplete: some sections failed",
			slog.Int("failed", summary.Failed),
			slog.Int("succeeded", summary.Succeeded),
			slog.String("error", summary.Err().Error()))
		return exitPartial
	}

	logger.Info("report complete",
		slog.String("output", cfg.Output.Path),
		slog.Int("sections", summary.Succeeded),
		slog.Int("rows", summary.Rows()))
	return exitOK
}
