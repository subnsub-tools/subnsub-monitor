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
  subnsub-monitor selftest               show what the collectors can and cannot open

serve only ever shows you THIS machine. connect is the real shape:
outbound-only, so a browser anywhere can watch a box it has no route to.
The token may also come from SUBNSUB_MONITOR_TOKEN, which keeps it out of ps.
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
		connect(args[1], token, every)

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
