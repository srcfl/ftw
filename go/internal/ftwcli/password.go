package ftwcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// runPassword turns the house password off, for an owner who forgot it or
// finds someone else set it. Only the box itself may do this without the
// password, so it reaches Core on loopback. Afterwards the box's page asks
// for a new one.
func runPassword(args []string, out io.Writer, e env) error {
	if len(args) == 0 || args[0] != "reset" {
		return usageError("usage: ftw password reset [--url URL]")
	}
	base, err := parse(args[1:], out, "ftw password reset [--url URL]", nil)
	if err != nil {
		return err
	}
	c := newClient(base, e)
	ctx := context.Background()
	err = c.call(ctx, http.MethodPost, "/api/auth/password", map[string]bool{"enabled": false}, nil, e.requestTimeout)
	var api *apiError
	switch {
	case err == nil:
		fmt.Fprintln(out, "The house password is off, and every browser is signed out.")
		fmt.Fprintln(out, "Open the box's page to choose a new one, under Settings → System or Settings → Access.")
		return nil
	case errors.As(err, &api) && api.status == http.StatusUnauthorized:
		return fmt.Errorf("%s asked for the house password; run this on the box itself, against http://127.0.0.1", base)
	case c.waitingForSetup(ctx):
		return fmt.Errorf("Core is waiting for setup and has no house password yet; open %s/setup", base)
	}
	return err
}
