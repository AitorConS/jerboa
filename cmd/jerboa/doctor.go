package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/AitorConS/jerboa/internal/api"
	"github.com/AitorConS/jerboa/internal/config"
	"github.com/AitorConS/jerboa/internal/registry/credentials"
	"github.com/spf13/cobra"
)

// doctorEnv is what the client-side checks look at, replaceable in tests.
type doctorEnv struct {
	goos, goarch string
	cliVersion   string
	configPath   string
	loadConfig   func(path string) error
	credentials  func() (path string, mode os.FileMode, exists bool, err error)
	kvmPresent   func() bool
}

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		goos: runtime.GOOS, goarch: runtime.GOARCH, cliVersion: version,
		configPath: config.DefaultPath(),
		loadConfig: func(path string) error {
			_, err := config.Load(path)
			return err //nolint:wrapcheck // shown as is
		},
		credentials: func() (string, os.FileMode, bool, error) {
			path, err := credentials.Path()
			if err != nil {
				return "", 0, false, err //nolint:wrapcheck // shown as is
			}
			info, serr := os.Stat(path)
			if serr != nil {
				return path, 0, false, nil //nolint:nilerr // no file means no logins, which is fine
			}
			_, lerr := credentials.Load()
			return path, info.Mode().Perm(), true, lerr //nolint:wrapcheck // shown as is
		},
		kvmPresent: func() bool {
			_, err := os.Stat("/dev/kvm")
			return err == nil
		},
	}
}

func finding(check, severity, hint, format string, args ...any) api.SystemFinding {
	return api.SystemFinding{Check: check, Severity: severity, Hint: hint, Message: fmt.Sprintf(format, args...)}
}

// clientFindings checks what the daemon cannot see: this machine's CLI, its
// configuration and credentials, and whether the host can virtualize at all.
func clientFindings(env doctorEnv) []api.SystemFinding {
	out := []api.SystemFinding{finding("cli", api.SeverityOK, "", "jerboa %s on %s/%s", env.cliVersion, env.goos, env.goarch)}

	if err := env.loadConfig(env.configPath); err != nil {
		out = append(out, finding("config", api.SeverityError, "fix the file or delete it to go back to the defaults", "%v", err))
	} else {
		out = append(out, finding("config", api.SeverityOK, "", "%s", env.configPath))
	}

	path, mode, exists, err := env.credentials()
	switch {
	case err != nil:
		out = append(out, finding("credentials", api.SeverityWarning, "delete the file and run `jerboa login` again", "%v", err))
	case !exists:
		out = append(out, finding("credentials", api.SeverityOK, "", "not logged in to any registry"))
	case env.goos != "windows" && mode&0o077 != 0:
		out = append(out, finding("credentials", api.SeverityWarning, "chmod 600 "+path, "%s is readable by other users (mode %04o)", path, mode))
	default:
		out = append(out, finding("credentials", api.SeverityOK, "", "%s", path))
	}

	switch env.goos {
	case "linux":
		if env.kvmPresent() {
			out = append(out, finding("virtualization", api.SeverityOK, "", "/dev/kvm is present"))
		} else {
			out = append(out, finding("virtualization", api.SeverityWarning,
				"enable virtualization in the firmware, or nested virtualization if this is itself a VM",
				"/dev/kvm is missing: VMs run without hardware acceleration, or not at all"))
		}
	case "darwin":
		if env.goarch == "arm64" {
			out = append(out, finding("virtualization", api.SeverityOK, "", "Apple Silicon (Hypervisor.framework)"))
		} else {
			out = append(out, finding("virtualization", api.SeverityWarning, "", "Intel Macs are not supported by the native daemon"))
		}
	}
	return out
}

// daemonHint says how to get a daemon running on each platform.
func daemonHint(goos string) string {
	switch goos {
	case "windows":
		return "jerboa daemon status shows the WSL2 distro; jerboa daemon install sets it up"
	case "darwin":
		return "jerboa daemon status, then jerboa daemon start (or open Jerboa Desktop)"
	default:
		return "systemctl status jerboad, then sudo systemctl start jerboad"
	}
}

// doctorDaemon is the part of the daemon client the doctor uses.
type doctorDaemon interface {
	DaemonInfo(ctx context.Context) (api.DaemonInfo, error)
	SystemCheck(ctx context.Context, p api.SystemCheckParams) (api.SystemCheckResult, error)
}

// daemonFindings asks the daemon about itself. dialErr is why no daemon could
// be reached, when none could.
func daemonFindings(ctx context.Context, env doctorEnv, d doctorDaemon, dialErr error, deep bool) []api.SystemFinding {
	if dialErr != nil {
		return []api.SystemFinding{finding("daemon", api.SeverityError, daemonHint(env.goos), "not reachable: %v", dialErr)}
	}
	var out []api.SystemFinding
	info, err := d.DaemonInfo(ctx)
	switch {
	case err != nil:
		out = append(out, finding("daemon", api.SeverityError, daemonHint(env.goos), "does not answer: %v", err))
		return out
	case info.Version != "" && env.cliVersion != "dev" && info.Version != "dev" && strings.TrimPrefix(info.Version, "v") != strings.TrimPrefix(env.cliVersion, "v"):
		out = append(out, finding("daemon", api.SeverityWarning, "restart the daemon so it runs the installed version",
			"running %s while the CLI is %s", info.Version, env.cliVersion))
	default:
		out = append(out, finding("daemon", api.SeverityOK, "", "running %s", info.Version))
	}

	res, err := d.SystemCheck(ctx, api.SystemCheckParams{Deep: deep})
	var rpc *api.RPCError
	switch {
	case errors.As(err, &rpc) && rpc.Code == -32601:
		out = append(out, finding("daemon state", api.SeverityInfo, "restart the daemon to get the full check",
			"the running daemon is older than this CLI and cannot check its own state"))
	case err != nil:
		out = append(out, finding("daemon state", api.SeverityError, "", "the daemon could not check its state: %v", err))
	default:
		out = append(out, res.Findings...)
	}
	return out
}

// doctorMarks is how each severity shows in the report.
var doctorMarks = map[string]string{
	api.SeverityOK:      "[ ok ]",
	api.SeverityInfo:    "[info]",
	api.SeverityWarning: "[warn]",
	api.SeverityError:   "[FAIL]",
}

func countSeverity(findings []api.SystemFinding, severity string) int {
	n := 0
	for _, f := range findings {
		if f.Severity == severity {
			n++
		}
	}
	return n
}

func renderDoctor(out io.Writer, findings []api.SystemFinding) {
	width := 0
	for _, f := range findings {
		width = max(width, len(f.Check))
	}
	for _, f := range findings {
		mark := doctorMarks[f.Severity]
		if mark == "" {
			mark = "[" + f.Severity + "]"
		}
		fmt.Fprintf(out, "%s %-*s  %s\n", mark, width, f.Check, f.Message)
		if f.Hint != "" {
			fmt.Fprintf(out, "       %-*s  -> %s\n", width, "", f.Hint)
		}
	}
	errs, warns := countSeverity(findings, api.SeverityError), countSeverity(findings, api.SeverityWarning)
	fmt.Fprintln(out)
	switch {
	case errs == 0 && warns == 0:
		fmt.Fprintln(out, "Everything looks right.")
	default:
		fmt.Fprintf(out, "%d problem(s), %d warning(s).\n", errs, warns)
	}
}

func newDoctorCmd(endpoint *string, outputFmt *string) *cobra.Command {
	var deep bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine, the daemon and its stored data are in order",
		Long: `Check this machine's CLI, configuration and virtualization support, that the
daemon is reachable and current, and that everything the daemon stores is
consistent: images, VMs, volumes, networks and snapshots.

Every problem comes with what to do about it. The command changes nothing and
exits non-zero when it finds a problem.

  --deep  also re-reads every image disk and compares it with its digest`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := defaultDoctorEnv()
			findings := clientFindings(env)

			dial := *endpoint
			var dialErr error
			// The doctor must be able to report a daemon that is not there, so
			// it brings the Windows daemon up itself instead of failing early.
			if env.goos == "windows" && dial == config.DefaultEndpoint() {
				dial, dialErr = ensureDaemon(cmd.Context(), "")
			}
			var client *api.Client
			if dialErr == nil {
				client, dialErr = api.Dial(dial)
			}
			if client != nil {
				defer func() { _ = client.Close() }()
			}
			findings = append(findings, daemonFindings(cmd.Context(), env, client, dialErr, deep)...)

			problems := countSeverity(findings, api.SeverityError)
			if *outputFmt == "json" {
				if err := printJSON(cmd.OutOrStdout(), map[string]any{"ok": problems == 0, "findings": findings}); err != nil {
					return err
				}
			} else {
				renderDoctor(cmd.OutOrStdout(), findings)
			}
			if problems > 0 {
				return fmt.Errorf("doctor: %d problem(s) found", problems)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "also verify every image disk against its digest (reads them all)")
	return cmd
}
