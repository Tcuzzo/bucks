package summary

import "bucks/internal/analyst"

// config holds the optional knobs for SummarizeAccount (currently the failover
// logger). It mirrors the chat surface's option pattern.
type config struct {
	log analyst.Logger
}

// Option configures a summary call.
type Option func(*config)

// WithLogger wires the failover logger (the human-visible echo of a downgrade). The
// structured Failovers trail on the returned summary is always present regardless.
func WithLogger(l analyst.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.log = l
		}
	}
}

// nopLogger is the default failover sink: the structured Failovers trail is the
// asserted record; the logger is the human-facing echo.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func newConfig(opts ...Option) config {
	c := config{log: nopLogger{}}
	for _, o := range opts {
		o(&c)
	}
	return c
}
