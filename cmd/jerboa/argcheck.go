package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// explainArgCounts wraps the argument check of every command under root so
// that a wrong number of arguments says which arguments the command got
// and why there may be one too many, instead of cobra's bare count. Cobra's
// own sentence is kept first, so scripts and tests that match it still do.
func explainArgCounts(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if check := c.Args; check != nil {
			c.Args = func(cmd *cobra.Command, args []string) error {
				if err := check(cmd, args); err != nil {
					return argCountError(cmd, args, err)
				}
				return nil
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// argCountError adds to a failed argument check, in at most three lines as
// every CLI error: the arguments as the command received them, quoted so a
// blank one shows; the likely cause, when there is one; and the usage line.
func argCountError(cmd *cobra.Command, args []string, err error) error {
	var b strings.Builder
	b.WriteString(err.Error())
	if len(args) > 0 {
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = strconv.Quote(a)
		}
		fmt.Fprintf(&b, ": %s", strings.Join(quoted, ", "))
	}
	if hint := argHint(cmd, args, tooMany(err.Error())); hint != "" {
		b.WriteString("\n  " + hint)
	}
	fmt.Fprintf(&b, "\n  usage: %s (see '%s --help')", cmd.UseLine(), cmd.CommandPath())
	return errors.New(b.String())
}

// argHint names the likely cause of an unexpected argument: one that is only
// whitespace (most often a space after the "\" meant to continue the line),
// one that is empty (an unset variable), or a second value given to a flag.
func argHint(cmd *cobra.Command, args []string, extra bool) string {
	var blank, empty []string
	for i, a := range args {
		switch {
		case a == "":
			empty = append(empty, strconv.Itoa(i+1))
		case strings.TrimSpace(a) == "":
			blank = append(blank, strconv.Itoa(i+1))
		}
	}
	switch {
	case len(blank) > 0:
		return fmt.Sprintf("argument %s is only whitespace: a \"\\\" continues the command on the next line only as the last character of the line; remove the space after it", strings.Join(blank, ", "))
	case len(empty) > 0:
		return fmt.Sprintf("argument %s is empty: a shell variable that is not set?", strings.Join(empty, ", "))
	case extra && cmd.HasAvailableFlags():
		hint := "a flag takes one value: to give it several, repeat the flag"
		if cmd.Flags().Lookup("port") != nil {
			hint += " (-p 80:80 -p 443:443)"
		}
		return hint + "; quote a value that contains spaces"
	}
	return ""
}

// tooMany reports whether cobra's message says the command got more
// arguments than it takes.
func tooMany(msg string) bool {
	if strings.HasPrefix(msg, "accepts at most ") || strings.HasPrefix(msg, "unknown command ") {
		return true
	}
	var want, got int
	if _, err := fmt.Sscanf(msg, "accepts %d arg(s), received %d", &want, &got); err == nil {
		return got > want
	}
	return false
}
