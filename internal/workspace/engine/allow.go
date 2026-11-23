package engine

import (
	"fmt"
	"strings"
)

// shellControl are the characters that let one command line run a second
// program, or the first one's output be redirected anywhere.
const shellControl = ";&|<>`$()\n\r"

// allowed checks command against commands, the programs a command may start
// with (LOOMCYCLE_WORKSPACE_COMMANDS); empty allows any. Listed, the command
// must be one simple command: shell control characters are refused, since
// `allowed; other` would otherwise run other. It names programs, not what
// they do: an allowed interpreter runs any code it is given. Every engine
// applies it the same way.
func allowed(commands []string, command string) error {
	if len(commands) == 0 {
		return nil
	}
	if strings.ContainsAny(command, shellControl) {
		return fmt.Errorf("%w: the operator lists the commands this worker runs, so a command may not use any of ; & | < > ` $ ( ) or a newline", ErrRefused)
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return fmt.Errorf("%w: an empty command", ErrRefused)
	}
	for _, c := range commands {
		if fields[0] == c {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not one of the commands this worker runs (%s)", ErrRefused, fields[0], strings.Join(commands, ", "))
}
