// Package ui implements kiac's terminal output: a step runner with a
// spinner, colors, and consistent symbols. No heavy TUI dependencies on
// purpose; plain ANSI keeps output clean in CI logs and pipes.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	colorEnabled = os.Getenv("NO_COLOR") == "" && isTTY()

	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	green  = "\033[32m"
	red    = "\033[31m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	accent = "\033[38;5;33m" // kubernetes blue
)

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func c(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + reset
}

// Banner prints the product header once per command invocation.
func Banner(version string) {
	fmt.Printf("%s %s\n", c(bold+accent, "⬢ kiac"), c(dim, version+" · Kubernetes in Apple Containers"))
}

// Infof prints a dim informational line aligned with step output.
func Infof(format string, args ...any) {
	fmt.Printf("   %s\n", c(dim, fmt.Sprintf(format, args...)))
}

// Successf prints a bold closing line.
func Successf(format string, args ...any) {
	fmt.Printf("\n%s\n", c(bold, fmt.Sprintf(format, args...)))
}

// Hintf prints an indented command hint.
func Hintf(format string, args ...any) {
	fmt.Printf("  %s\n", c(cyan, fmt.Sprintf(format, args...)))
}

// Warnf prints a prominent, non-fatal warning line.
func Warnf(format string, args ...any) {
	fmt.Printf(" %s %s\n", c(bold+yellow, "!"), c(yellow, fmt.Sprintf(format, args...)))
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Step runs fn under a live spinner line, replacing it with ✓/✗ on finish.
func Step(title string, fn func() error) error {
	start := time.Now()
	done := make(chan struct{})
	var wg sync.WaitGroup

	if colorEnabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-done:
					return
				case <-time.After(90 * time.Millisecond):
					frame := spinnerFrames[i%len(spinnerFrames)]
					fmt.Printf("\r %s %s", c(accent, frame), title)
					i++
				}
			}
		}()
	} else {
		fmt.Printf(" - %s\n", title)
	}

	err := fn()
	close(done)
	wg.Wait()

	elapsed := time.Since(start).Round(100 * time.Millisecond)
	if colorEnabled {
		fmt.Print("\r\033[K")
	}
	if err != nil {
		fmt.Printf(" %s %s\n", c(red, "✗"), title)
		return err
	}
	fmt.Printf(" %s %s %s\n", c(green, "✓"), title, c(dim, fmt.Sprintf("(%s)", elapsed)))
	return nil
}

// Fail prints a formatted error block and returns the error unchanged so
// callers can bubble it up after reporting.
func Fail(err error) error {
	msg := strings.TrimSpace(err.Error())
	fmt.Fprintf(os.Stderr, "\n%s %s\n", c(red+bold, "Error:"), msg)
	return err
}

// Check prints a doctor-style pass/fail line.
func Check(ok bool, label, detail string) {
	sym := c(green, "✓")
	if !ok {
		sym = c(red, "✗")
	}
	if detail != "" {
		detail = " " + c(dim, detail)
	}
	fmt.Printf(" %s %s%s\n", sym, label, detail)
}
