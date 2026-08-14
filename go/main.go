package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"time"
)

// Where warnf writes — which is this machine's own record of what was done to
// it, and the one account of a console command that does not pass through us.
//
// An interface rather than *os.File because on Windows there is nowhere for a
// background process's stderr to go: systemd has the journal and launchd has a
// log, Task Scheduler discards both streams. log_windows.go tees this into a
// file beside the config so the promise console.go makes holds on every
// platform rather than on two of them.
var stderr io.Writer = os.Stderr

const usage = `subnsub-monitor — AI coding quota for machines you can't reach

  subnsub-monitor                        one snapshot as JSON, exit
  subnsub-monitor watch [SEC]            reprint every SEC seconds
  subnsub-monitor serve [PORT]           serve /quota + /events on 127.0.0.1
  subnsub-monitor token                  mint a relay token
  subnsub-monitor connect URL [TOKEN]    dial out and push to the relay
  subnsub-monitor name [LABEL]           show or set this machine's dashboard name
  subnsub-monitor console [on|off]       show or set whether the dashboard may run commands here
  subnsub-monitor update [on|off]        show or set whether the dashboard may replace this helper
  subnsub-monitor diagnostics [on|off]   show or set read-only on-demand diagnostics
  subnsub-monitor sessions install       install the pinned AgentsView session index here
  subnsub-monitor trust [KEY]            list, or add, a dashboard key allowed to command this machine
  subnsub-monitor untrust KEY            stop accepting commands signed by that key
  subnsub-monitor version                print this build's version and exit
  subnsub-monitor selftest               show what the collectors can and cannot open

serve only ever shows you THIS machine. connect is the real shape:
outbound-only, so a browser anywhere can watch a box it has no route to.
The token may also come from SUBNSUB_MONITOR_TOKEN, or from the file the
installer wrote — both keep it out of ps and out of the task's arguments.

Every machine you paste the same token on gets its own dashboard, told apart
by a random id created on first run. Give it a name and the dashboard says
that instead; the name is yours to type and is never taken from the hostname.

console is OFF until you turn it on here, on this machine. With it on, the
dashboard can run a command as this user and show you the output; each command
is its own /bin/sh -c, so nothing persists between them, and every one of them
is written to this machine's own log before it runs. Turn it off with
'console off' and the helper stops asking the relay for work at all.

update is how a helper gets replaced without a shell here. The dashboard says
WHEN; what gets installed comes from the release bucket and is checked against
the published checksum, so the relay cannot point this machine at anything else.
Turning the console on implies it — a console can already run the installer —
and 'update on' is for a machine that wants the button and no console. There is
no timer: nothing is downloaded until somebody presses it.

trust is who may command this machine, as opposed to whether it takes commands
at all. The installer pins the dashboard's public key when you paste its line;
after that, 'trust <key>' adds another (a second browser, a colleague) and
'untrust' removes one. With at least one key pinned, a command must carry a
signature made by the matching private key — which lives in a browser and never
reaches the relay — so a relay can drop or delay a command but cannot write one.
With no key pinned this machine behaves as it always did and runs what the relay
hands it. Both states are shown on the dashboard, because they are not equally
safe.

diagnostics is OFF until you turn it on here. It is read-only and independent
of the console: the dashboard may request a bounded top-process resource list,
but receives no command lines, environment, working directories or open files.
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println(string(dumpIndent(collectAll())))
		return
	}

	switch args[0] {
	case "watch":
		every := 30.0
		if len(args) > 1 {
			if v, err := strconv.ParseFloat(args[1], 64); err == nil {
				// ParseFloat accepts "NaN" and "-1"; both make time.Sleep
				// return immediately and turn this into a hot loop.
				every = clampInterval(v)
			}
		}
		// Same series the daemon pushes, so what `watch` prints is what the
		// relay would receive rather than a second, thinner shape of it.
		startSysSampler()
		for {
			fmt.Println(string(dump(collectAll())))
			time.Sleep(time.Duration(every * float64(time.Second)))
		}

	case "serve":
		port := 8787
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil {
				port = v
			}
		}
		serve(port)

	case "token":
		fmt.Println(newToken())

	case "connect":
		if len(args) < 2 {
			fmt.Print(usage)
			os.Exit(2)
		}
		token := os.Getenv(tokenEnv)
		rest := args[2:]
		if token == "" && len(args) >= 3 {
			token = args[2]
			rest = args[3:]
		}
		if token == "" {
			// The file the installer wrote, which on Windows is the ordinary
			// path rather than a fallback: Task Scheduler runs a program with
			// arguments and carries no environment, and the two places a token
			// could otherwise go there — the command line, or the user's
			// persistent environment — are both readable by every other program
			// that user runs. See installedEnv.
			token = installedEnv()[tokenEnv]
		}
		if token == "" {
			warnf("no token: pass one, or set SUBNSUB_MONITOR_TOKEN")
			os.Exit(2)
		}
		every := 30.0
		if len(rest) > 0 {
			if v, err := strconv.ParseFloat(rest[0], 64); err == nil {
				every = v // connect clamps it
			}
		}
		// A token renewed on an earlier run outlives that run. The installed one
		// is the bootstrap and stays where the service manager put it; whichever
		// of the two lasts longer is the one to push with, which is also what
		// lets a reinstall override a renewal rather than be ignored by it.
		//
		// Scoped to the relay being dialled, and only consulted for a relay we
		// could have renewed for: `connect` takes any URL, and a credential
		// obtained for one relay must not be presented to another.
		connect(args[1], resolveStartToken(args[1], token), every)

	case "name":
		if len(args) > 1 {
			if err := setLabel(args[1]); err != nil {
				warnf("could not save the name")
				os.Exit(1)
			}
		}
		// Printed from the loader rather than from the argument, so what you
		// see is what the dashboard will show after cleaning — a name that got
		// trimmed should say so here, not surprise you on the page.
		if label := agentLabel(); label != "" {
			fmt.Printf("%s  (%s)\n", label, agentID())
		} else {
			fmt.Printf("unnamed  (%s)\n", agentID())
		}

	case "console":
		if len(args) > 1 {
			on := args[1] == "on" || args[1] == "1" || args[1] == "true" || args[1] == "yes"
			off := args[1] == "off" || args[1] == "0" || args[1] == "false" || args[1] == "no"
			if !on && !off {
				// Not guessed. "console maybe" turning it ON because the word
				// was not "off" is the failure mode this whole file is built
				// to avoid.
				warnf("say 'console on' or 'console off'")
				os.Exit(2)
			}
			if err := setConsole(on); err != nil {
				warnf("could not change the console setting")
				os.Exit(1)
			}
		}
		// Printed from the same reader the loop consults, so what you see is
		// what the running helper will do — including the case where MON_CONSOLE
		// in the environment overrides the file you just wrote.
		on := consoleEnabled()
		if on {
			fmt.Printf("on   (%s)\n", agentID())
		} else {
			fmt.Printf("off  (%s)\n", agentID())
		}
		// The unit's sandbox was decided at install time and this switch cannot
		// change it. Saying so here is the difference between a console that
		// obviously needs one more step and one that appears to work until the
		// first command that writes something.
		if note := consoleSandboxNote(on); note != "" {
			fmt.Println(note)
		}

	case "update":
		if len(args) > 1 {
			on := args[1] == "on" || args[1] == "1" || args[1] == "true" || args[1] == "yes"
			off := args[1] == "off" || args[1] == "0" || args[1] == "false" || args[1] == "no"
			if !on && !off {
				// Not guessed, for the same reason `console` does not guess.
				warnf("say 'update on' or 'update off'")
				os.Exit(2)
			}
			if err := setRemoteUpdate(on); err != nil {
				warnf("could not change the update setting")
				os.Exit(1)
			}
		}
		// From the same reader the loop consults, so what is printed is what
		// the running helper will do — including the two cases where the answer
		// does not come from the file just written: MON_UPDATE in the
		// environment, and a console that is on and therefore already grants
		// this.
		on := updateAllowed()
		if on {
			fmt.Printf("on   (%s)\n", agentID())
			if consoleEnabled() {
				fmt.Println("note: the console is on, which allows this on its own —\n" +
					"      a console can already run the installer. 'update off' will not\n" +
					"      take that away; 'console off' is what does.")
			}
		} else {
			fmt.Printf("off  (%s)\n", agentID())
		}
		// The unit's sandbox was decided at install time and this switch cannot
		// change it, exactly as with the console.
		if note := updateSandboxNote(on); note != "" {
			fmt.Println(note)
		}

	case "diagnostics":
		if len(args) > 1 {
			on := args[1] == "on" || args[1] == "1" || args[1] == "true" || args[1] == "yes"
			off := args[1] == "off" || args[1] == "0" || args[1] == "false" || args[1] == "no"
			if !on && !off {
				warnf("say 'diagnostics on' or 'diagnostics off'")
				os.Exit(2)
			}
			if on && !diagnosticsAvailable() {
				warnf("read-only process diagnostics are currently available on Linux only")
				os.Exit(1)
			}
			if err := setDiagnostics(on); err != nil {
				warnf("could not change the diagnostics setting: %v", err)
				os.Exit(1)
			}
		}
		if !diagnosticsAvailable() {
			fmt.Printf("unsupported  (%s)\n", agentID())
		} else if diagnosticsEnabled() {
			fmt.Printf("on   (%s)\n", agentID())
		} else {
			fmt.Printf("off  (%s)\n", agentID())
		}

	case "trust":
		// Granting authority is a thing you do AT the machine, like every other
		// switch here. There is deliberately no way to add a key over the
		// network: an endpoint that could would hand back the property this
		// whole mechanism buys.
		if len(args) < 2 {
			t := loadTrust()
			keys := t.keys
			if len(keys) == 0 {
				if t.sealed {
					// Not the same sentence as "nothing pinned", because this
					// machine is refusing every command right now and the reason
					// is a file somebody can look at.
					fmt.Printf("a trusted-keys file exists but holds no usable key,\n"+
						"so EVERY command is being refused. Fix or remove %s\n", trustedKeysPath())
					break
				}
				fmt.Println("no keys trusted — this machine runs what the relay hands it")
				break
			}
			fmt.Printf("%d key(s) trusted; commands must be signed by one of them\n", len(keys))
			for _, k := range keys {
				fmt.Println("  " + base64.StdEncoding.EncodeToString(k))
			}
			break
		}
		if err := trustKey(args[1]); err != nil {
			warnf("could not trust that key: %v", err)
			os.Exit(1)
		}
		fmt.Printf("trusted; this machine now runs only commands signed by a trusted key (%s)\n", agentID())

	case "untrust":
		if len(args) < 2 {
			warnf("say 'untrust <key>' — 'trust' with no argument lists them")
			os.Exit(2)
		}
		if err := untrustKey(args[1]); err != nil {
			warnf("could not remove that key: %v", err)
			os.Exit(1)
		}
		if !loadTrust().sealed {
			fmt.Println("removed; NO keys are trusted now — this machine runs what the relay hands it")
		} else {
			fmt.Println("removed")
		}

	case "sessions":
		// Install (or reinstall) AgentsView, the per-machine session index the
		// dashboard's fleet search asks. `sessions install` is the only verb,
		// and it takes no further arguments — a mistyped verb or a stray word
		// exits non-zero rather than silently doing nothing or installing
		// anyway. See sessions.go for why WE pin the version and check the
		// bytes.
		if len(args) != 2 || args[1] != "install" {
			warnf("usage: subnsub-monitor sessions install")
			warnf("(installs the session index the dashboard's fleet search runs on this machine)")
			if !sessPlatformSupported() {
				warnf("note: no published agentsview build for %s/%s", runtime.GOOS, runtime.GOARCH)
			}
			os.Exit(2)
		}
		res, _ := runSessionsInstall()
		if res.Out != "" {
			fmt.Print(res.Out)
		}
		if res.Code != 0 {
			os.Exit(1)
		}

	case "version":
		// Deliberately nothing but the version, on one line. The update path
		// runs this on a freshly downloaded binary and compares the output
		// against what the release claimed to be — a banner, a warning, or a
		// trailing note would all have to be parsed around, and every one of
		// those is a way for the check to start passing by accident.
		fmt.Println(helperVersion)

	case "selftest":
		selftest()

	case "-h", "--help", "help":
		fmt.Print(usage)

	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

// A token is a bearer secret: whoever holds it can push readings and read
// them. Long, from a CSPRNG, in the relay's accepted alphabet.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not something to paper over with a weaker
		// source — a predictable token is worse than no token.
		warnf("cannot generate a token: system RNG unavailable")
		os.Exit(1)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
