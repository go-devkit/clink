// Package clink connects CLI invocations to a long-running daemon instance of
// the same binary. One binary runs in two modes: a daemon that listens for
// commands, and short-lived CLI invocations that forward their arguments to it.
// SSH is used as the transport but is fully hidden from the consumer — nothing
// outside this package imports an SSH library.
//
// The daemon calls [Listen] with a [Handler]; each client calls [Connect] with
// the arguments to forward. A Handler can run an ordinary command, set a remote
// exit code via [ExitError], or launch a Bubble Tea TUI via [Interactive]. When
// Connect is given no arguments it opens an interactive session against the
// daemon's main TUI.
//
// clink also forwards files from the client's filesystem on the daemon's
// request (see [Session]), local-command dispatch that bypasses the daemon (see
// [LocalCommand], registered with [WithLocalCommand] or [WithLocalFallback]),
// client signals, and terminal resizes.
//
// clink assumes the daemon and its clients share one trust domain — typically a
// loopback-bound daemon serving processes of the same user. It does no rate
// limiting and caps neither connections nor sessions; an empty [Config] Password
// disables authentication. Do not expose the port. See the Config, Handler, and
// Session documentation and the README for the full security model.
package clink
