package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
)

// CtxKeyQuiet mutes pipeline output when set to true in the context.
var CtxKeyQuiet = struct{ key string }{key: "quiet"}

// nonFatalError indicates that a pipeline step failed but execution should continue.
type nonFatalError struct{ inner error }

func (e nonFatalError) Error() string { return e.inner.Error() }
func (e nonFatalError) Unwrap() error { return e.inner }

// MarkNonFatal wraps an error so that the pipeline logs a warning instead of aborting.
func MarkNonFatal(err error) error {
	if err == nil {
		return nil
	}
	return nonFatalError{err}
}

// IsNonFatal reports whether err (or any error in its chain) is non-fatal.
func IsNonFatal(err error) bool {
	var nf nonFatalError
	return errors.As(err, &nf)
}

// CommandChain creates and configures pipeline instances.
type CommandChain interface {
	Init(ctx context.Context) *ActiveCommandChain
	Logger(ctx context.Context) *log.Entry
}

// ActiveCommandChain is a backwards-compatible alias for Pipeline.
type ActiveCommandChain = Pipeline

var _ CommandChain = (*PipelineBuilder)(nil)

// step represents a single unit of work in a pipeline.
type step struct {
	label string
	fn    func() error
}

// PipelineBuilder creates configured pipeline instances.
type PipelineBuilder struct {
	name string
}

// New returns a PipelineBuilder for the given logical name.
func New(name string) *PipelineBuilder {
	return &PipelineBuilder{name: name}
}

// Logger creates a contextual logger for this pipeline.
func (b *PipelineBuilder) Logger(ctx context.Context) *log.Entry {
	if quiet, _ := ctx.Value(CtxKeyQuiet).(bool); quiet {
		dummy := log.New()
		dummy.SetOutput(io.Discard)
		return dummy.WithContext(ctx)
	}
	return log.WithField("pipeline", b.name).WithContext(ctx)
}

// Init starts a new Pipeline using the builder's configuration.
func (b *PipelineBuilder) Init(ctx context.Context) *Pipeline {
	return &Pipeline{logger: b.Logger(ctx)}
}

// Pipeline executes a sequence of steps with stage tracking and timing.
type Pipeline struct {
	steps     []step
	logger    *log.Entry
	lastLabel string
	startTime time.Time
	executing bool
}

// Logger returns the pipeline's contextual logger.
func (p *Pipeline) Logger() *log.Entry { return p.logger }

// SetLogger replaces the pipeline's logger. Used primarily in tests.
func (p *Pipeline) SetLogger(l *log.Entry) { p.logger = l }

// Add appends a function to the pipeline.
func (p *Pipeline) Add(fn func() error) {
	p.steps = append(p.steps, step{fn: fn})
}

// Stage records a named stage marker. If the pipeline is already executing,
// the label is printed immediately; otherwise it is queued for execution.
func (p *Pipeline) Stage(label string) {
	if p.executing {
		p.logger.Println(label, "...")
		return
	}
	p.steps = append(p.steps, step{label: label})
}

// Stagef is a formatted variant of Stage.
func (p *Pipeline) Stagef(format string, args ...any) {
	p.Stage(fmt.Sprintf(format, args...))
}

// Exec runs all steps sequentially. The first non-recoverable error aborts
// the pipeline and is returned wrapped with the current stage label.
func (p *Pipeline) Exec() error {
	p.executing = true
	defer func() { p.executing = false }()

	for _, s := range p.steps {
		// label-only step
		if s.fn == nil {
			if s.label != "" {
				if !p.startTime.IsZero() {
					p.logger.Printf("stage %q took %s", p.lastLabel, time.Since(p.startTime))
				}
				p.logger.Println(s.label, "...")
				p.lastLabel = s.label
				p.startTime = time.Now()
			}
			continue
		}

		// execute step
		err := s.fn()
		if err == nil {
			continue
		}

		if IsNonFatal(err) {
			if p.lastLabel == "" {
				p.logger.Warnln(err)
			} else {
				p.logger.Warnln(fmt.Errorf("warning at %q: %w", p.lastLabel, err))
			}
			continue
		}

		if p.lastLabel == "" {
			return err
		}
		return fmt.Errorf("error at %q: %w", p.lastLabel, err)
	}

	if !p.startTime.IsZero() {
		p.logger.Printf("stage %q took %s", p.lastLabel, time.Since(p.startTime))
	}
	return nil
}

// Retry adds a retried step to the pipeline. The function is invoked up to
// count times with the specified interval between attempts.
func (p *Pipeline) Retry(label string, interval time.Duration, count int, fn func(attempt int) error) {
	p.Add(func() error {
		var err error
		for i := 0; i < count; i++ {
			err = fn(i + 1)
			if err == nil {
				return nil
			}
			if label != "" {
				p.logger.Println(label, "...")
			}
			if i < count-1 {
				time.Sleep(interval)
			}
		}
		return err
	})
}
