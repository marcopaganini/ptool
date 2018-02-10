// This file is part of ptool
// (C) 2017 by Marco Paganini <paganini@paganini.net>
//
// Check the main repository at http://github.com/marcopaganini/ptool
// for more details.

package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/marcopaganini/logger"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	// The PID used by init.
	initPID = 1
)

var (
	// The time to wait between each iteration on the loop when checking
	// if we became an orphan.
	orphanWaitTime = 500 * time.Millisecond

	log = logger.New("")
)

func main() {
	var (
		optKillOrphan bool
		optLogFile    string
		optShell      bool
		optShellCmd   string
		optTimeout    int
		optVerbose    bool
	)

	flag.BoolVar(&optKillOrphan, "kill-orphan", false, "Kill the program if parent becomes init.")
	flag.StringVar(&optLogFile, "logfile", "", "Also write log messages to this file.")
	flag.BoolVar(&optShell, "shell", false, "Use shell to execute command.")
	flag.StringVar(&optShellCmd, "shell-command", "/bin/bash", "Path to shell binary to use.")
	flag.IntVar(&optTimeout, "timeout", 0, "Program execution timeout (in seconds).")
	flag.BoolVar(&optVerbose, "verbose", false, "Verbose log messages.")

	// Custom usage.
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Use: %s [flags] -- command line to run...\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	if len(flag.Args()) == 0 {
		fmt.Fprintf(os.Stderr, "Error: No command to execute\n")
		flag.Usage()
		os.Exit(2)
	}

	if optLogFile != "" {
		logw, err := os.Create(optLogFile)
		if err != nil {
			log.Fatalf("Error creating log file %q: %v\n", optLogFile, err)
		}
		defer logw.Close()
		log.SetMirrorOutput(logw)
	}

	if optVerbose {
		log.SetVerboseLevel(1)
	}

	cmdline := flag.Args()
	if optShell {
		cmdline = append([]string{optShellCmd, "-c"}, strings.Join(flag.Args(), " "))
	}

	// Create a background context. Add a timeout if specified.
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	ctx = context.Background()
	if optTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(optTimeout)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, cmdline[0], cmdline[1:]...)

	// Inherit our std pipes.
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command in a separate goroutine and report to a channel when
	// the process is complete. This allows us to do a non-blockig wait
	// since Go's Wait() is always blocking.
	retchan := make(chan error)
	go func() {
		retchan <- cmd.Run()
	}()
	setSignals(cmd)
	defer resetSignals()

	log.Verbosef(1, "Executing command: %q\n", cmdline)

	var (
		errproc  error
		orphaned bool
	)
	if optKillOrphan {
		orphaned, errproc = waitOrphan(retchan)
		// If we became an orphan (that is, our parent became init), we
		// summarily kill the child and exit.
		if orphaned {
			log.Verbosef(1, "Init (pid=1) is our father. killing our process group\n")
			if err := syscall.Kill(0, syscall.SIGTERM); err != nil {
				log.Fatal(err)
			}
			time.Sleep(2 * time.Second)
			if err := syscall.Kill(0, syscall.SIGKILL); err != nil {
				log.Fatal(err)
			}
			os.Exit(1)
		}
	} else {
		errproc = <-retchan
	}
	ret, err := retcodeFromError(errproc)
	if err != nil {
		log.Fatal(err)
	}
	log.Verbosef(1, "Exiting with retcode = %d\n", ret)
	os.Exit(ret)
}

// setSignals traps common signals and re-sends them to the child process.
func setSignals(cmd *exec.Cmd) {
	// This channel has to be large enough to accomodate simultaneous signals.
	sigs := make(chan os.Signal, 1)

	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)

	go func() {
		sig := <-sigs
		//log.Verbosef(1, "Received signal: %s\n", sig.String())
		cmd.Process.Signal(sig)
	}()
}

// resetSignals resets a list of common signals on a given signal channel to
// their default handlers.
func resetSignals() {
	signal.Reset(syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
}

// waitOrphan performs a non-blocking wait on the return channel and returns if
// the child task dies or if our PPID becomes 1 (that is, our parent died and
// we got adopted by init).
func waitOrphan(retchan <-chan error) (bool, error) {
	var err error
OutaLoop:
	for {
		select {
		case err = <-retchan:
			log.Verbosef(1, "Child process finished\n")
			break OutaLoop
		default:
			time.Sleep(orphanWaitTime)
			ppid := os.Getppid()
			if ppid == initPID {
				log.Verbosef(1, "Process ppid=1 and kill-orphan requested. Exiting.\n")
				return true, nil
			}
		}
	}
	return false, err
}

// retcodeFromError returns the process return code inside exec.ExitError, as
// returned by cmd.Run() or cmd.Wait().
func retcodeFromError(err error) (int, error) {
	// No error, return zero.
	if err == nil {
		return 0, nil
	}

	// If error satisfies exec.ExitError, it contains the return code from the
	// finished process. In that case, fetch the status code and return.
	var ret int
	if exiterr, ok := err.(*exec.ExitError); ok {
		if s, ok := exiterr.Sys().(syscall.WaitStatus); ok {
			ret = s.ExitStatus()
		}
	}

	// Some other error executing command. This indicates an error in the
	// wait() call, not a process error.
	return ret, err
}
