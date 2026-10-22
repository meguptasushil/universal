package portal

import "strings"

// friendly turns a technical error into a plain-language message and a hint
// about what to do next.
func friendly(err error) (msg, hint string) {
	if err == nil {
		return "", ""
	}
	e := err.Error()
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(strings.ToLower(e), strings.ToLower(s)) {
				return true
			}
		}
		return false
	}
	switch {
	case has("invalid token", "401 Unauthorized"):
		return "The relay didn't accept your access code.",
			"Copy the connection code again from whoever runs the relay and paste it under Connect."
	case has("no such host"):
		return "That relay address doesn't exist.",
			"Check the address for typos. It should look like https://example.com."
	case has("connection refused"):
		return "The relay server isn't answering.",
			"Make sure the relay is running and ports 80 and 443 are open in its firewall."
	case has("i/o timeout", "deadline exceeded", "timed out"):
		return "The relay server didn't answer in time.",
			"It may be offline, or a firewall is blocking it. Try again in a minute."
	case has("x509", "certificate"):
		return "The relay's HTTPS certificate isn't ready.",
			"If the relay just started, wait a minute and try again. Its DNS must point at the server."
	case has("already in use"):
		return "That public name is already taken.",
			"Pick a different public name and try again."
	case has("TCP tunnels are disabled"):
		return "This relay doesn't accept databases or other TCP services.",
			"Ask the relay owner to start it with --tcp-ports 20000-20100."
	case has("custom domains are disabled"):
		return "This relay doesn't allow your own domain names.",
			"Use a public name instead, or ask the relay owner to enable custom domains."
	case has("no free TCP port"):
		return "The relay has no free ports left for TCP services.",
			"Stop another TCP service, or ask the relay owner for a bigger port range."
	case has("outside this relay's TCP range"):
		return "That public port isn't available on this relay.",
			"Leave the public port empty to get any free one."
	case has("executable file not found", "command not found", "is not recognized"):
		return "A program your project needs isn't installed on this computer.",
			"Open Logs to see which one (for example node, npm, python or docker), install it, and press Start."
	}
	return e, ""
}
