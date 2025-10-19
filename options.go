package cligate

import (
	"context"

	"github.com/urfave/cli/v3"
)

type Options struct {
	User           string
	IgnoreCommands []string

	FlagHost     string
	FlagPort     string
	FlagPassword string

	Before func(context.Context, *cli.Command) (context.Context, error)
	After  func(context.Context, *cli.Command) (context.Context, error)
}

type Option func(*Options)

func WithUser(user string) Option {
	return func(opts *Options) {
		opts.User = user
	}
}

func WithIgnoreCommands(cmds ...string) Option {
	return func(opts *Options) {
		opts.IgnoreCommands = cmds
	}
}

func WithBefore(fn func(context.Context, *cli.Command) (context.Context, error)) Option {
	return func(opts *Options) {
		opts.Before = fn
	}
}

func WithAfter(fn func(context.Context, *cli.Command) (context.Context, error)) Option {
	return func(opts *Options) {
		opts.After = fn
	}
}

func toOpts(opts []Option) Options {
	options := Options{
		User:         "user",
		FlagHost:     "host",
		FlagPort:     "port",
		FlagPassword: "password",
	}

	for _, opt := range opts {
		opt(&options)
	}

	return options
}
