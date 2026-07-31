package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"time"
)

var stderr = os.Stderr

const usage = `subnsub-monitor — AI coding quota for machines you can't reach

  subnsub-monitor                        one snapshot as JSON, exit
  subnsub-monitor watch [SEC]            reprint every SEC seconds
  subnsub-monitor serve [PORT]           serve /quota + /events on 127.0.0.1
  subnsub-monitor token                  mint a relay token
  subnsub-monitor connect URL [TOKEN]    dial out and push to the relay
  subnsub-monitor name [LABEL]           show or set this machine's dashboard name
  subnsub-monitor console [on|off]       show or set whether the dashboard may run commands here
  subnsub-monitor selftest               show what the collectors can and cannot open

serve only ever shows you THIS machine. connect is the real shape:
outbound-only, so a browser anywhere can watch a box it has no route to.
The token may also come from SUBNSUB_MONITOR_TOKEN, which keeps it out of ps.

Every machine you paste the same token on gets its own dashboard, told apart
by a random id created on first run. Give it a name and the dashboard says
that instead; the name is yours to type and is never taken from the hostname.

console is OFF until you turn it on here, on this machine. With it on, the
dashboard can run a command as this user and show you the output; each command
is its own /bin/sh -c, so nothing persists between them, and every one of them
is written to this machine's own log before it runs. Turn it off with
'console off' and the helper stops asking the relay for work at all.
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
		token := os.Getenv("SUBNSUB_MONITOR_TOKEN")
		rest := args[2:]
		if token == "" {
			if len(args) < 3 {
				warnf("no token: pass one, or set SUBNSUB_MONITOR_TOKEN")
				os.Exit(2)
			}
			token = args[2]
			rest = args[3:]
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
		if consoleEnabled() {
			fmt.Printf("on   (%s)\n", agentID())
		} else {
			fmt.Printf("off  (%s)\n", agentID())
		}

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
