package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/pprof"
	"runtime/trace"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/attachment"
	"github.com/dylantirandaz/inklingharness/internal/latency"
)

type filePaths []string

func (paths *filePaths) String() string { return strings.Join(*paths, ", ") }

func (paths *filePaths) Set(path string) error {
	if path == "" {
		return errors.New("-file requires a path")
	}
	*paths = append(*paths, path)
	return nil
}

func printAttachedFiles(output io.Writer, files []attachment.FileInfo) {
	if len(files) == 0 {
		return
	}
	var size int64
	for _, file := range files {
		size += file.Size
	}
	fmt.Fprintf(output, "Context: %d files, %d bytes\n", len(files), size)
}

type profileOutput struct {
	file       *os.File
	writeError error
}

func (output *profileOutput) Write(data []byte) (int, error) {
	count, err := output.file.Write(data)
	if err != nil && output.writeError == nil {
		output.writeError = err
	}
	return count, err
}

func (output *profileOutput) close() error {
	return errors.Join(output.writeError, output.file.Close())
}

type profiles struct {
	cpu       *profileOutput
	execution *profileOutput
}

func startProfiles(options *options) (*profiles, error) {
	if options.cpuProfile == "" && options.runtimeTrace == "" {
		return nil, nil
	}
	active := &profiles{}
	if options.cpuProfile != "" {
		file, err := os.OpenFile(options.cpuProfile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, fmt.Errorf("CPU profile: %w", err)
		}
		output := &profileOutput{file: file}
		if err := pprof.StartCPUProfile(output); err != nil {
			return nil, errors.Join(fmt.Errorf("start CPU profile: %w", err), output.close())
		}
		active.cpu = output
	}
	if options.runtimeTrace != "" {
		file, err := os.OpenFile(options.runtimeTrace, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("runtime trace: %w", err), active.close())
		}
		output := &profileOutput{file: file}
		if err := trace.Start(output); err != nil {
			return nil, errors.Join(fmt.Errorf("start runtime trace: %w", err), output.close(), active.close())
		}
		active.execution = output
	}
	return active, nil
}

func (active *profiles) close() error {
	if active == nil {
		return nil
	}
	var failures []error
	if active.execution != nil {
		trace.Stop()
		failures = append(failures, active.execution.close())
		active.execution = nil
	}
	if active.cpu != nil {
		pprof.StopCPUProfile()
		failures = append(failures, active.cpu.close())
		active.cpu = nil
	}
	return errors.Join(failures...)
}

func timedContext(ctx context.Context, options *options) (context.Context, *latency.Recorder) {
	if options.timings == "" {
		return ctx, nil
	}
	recorder := latency.New(options.model, options.effort)
	return latency.WithRecorder(ctx, recorder), recorder
}

func finishTiming(recorder *latency.Recorder, path string, operationError error) error {
	if recorder == nil {
		return operationError
	}
	recorder.Finish(operationError)
	return errors.Join(operationError, recorder.AppendFile(path))
}

func timingsCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("think timings", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "think timings: exactly one timing JSONL file is required")
		return 2
	}
	file, err := os.Open(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "think timings: %v\n", err)
		return 1
	}
	err = latency.Summarize(file, stdout)
	if err := errors.Join(err, file.Close()); err != nil {
		fmt.Fprintf(stderr, "think timings: %v\n", err)
		return 1
	}
	return 0
}
