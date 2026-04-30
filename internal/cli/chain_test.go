package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestPipeline_StageTiming(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetLevel(log.TraceLevel)
	l.SetFormatter(&log.TextFormatter{DisableQuote: true})

	chain := New("test").Init(context.Background())
	chain.SetLogger(l.WithField("context", "test"))

	chain.Stage("stage one")
	chain.Add(func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	chain.Stage("stage two")
	chain.Add(func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	if err := chain.Exec(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, `stage "stage one" took`) {
		t.Errorf("expected timing for stage one, got:\n%s", out)
	}
	if !strings.Contains(out, `stage "stage two" took`) {
		t.Errorf("expected timing for stage two, got:\n%s", out)
	}
	if !strings.Contains(out, "stage one ...") {
		t.Errorf("expected stage one label, got:\n%s", out)
	}
	if !strings.Contains(out, "stage two ...") {
		t.Errorf("expected stage two label, got:\n%s", out)
	}
}

func TestPipeline_StageTiming_SingleStage(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetLevel(log.TraceLevel)
	l.SetFormatter(&log.TextFormatter{DisableQuote: true})

	chain := New("test").Init(context.Background())
	chain.SetLogger(l.WithField("context", "test"))

	chain.Stage("only stage")
	chain.Add(func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	if err := chain.Exec(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `stage "only stage" took`) {
		t.Errorf("expected timing for single stage, got:\n%s", out)
	}
}

func TestPipeline_StageTiming_NoStages(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetLevel(log.TraceLevel)
	l.SetFormatter(&log.TextFormatter{DisableQuote: true})

	chain := New("test").Init(context.Background())
	chain.SetLogger(l.WithField("context", "test"))

	chain.Add(func() error { return nil })
	chain.Add(func() error { return nil })

	if err := chain.Exec(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, `stage "" took`) {
		t.Errorf("unexpected timing log for empty stage, got:\n%s", out)
	}
}

func TestPipeline_StageTiming_ErrorMidChain(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetLevel(log.TraceLevel)
	l.SetFormatter(&log.TextFormatter{DisableQuote: true})

	chain := New("test").Init(context.Background())
	chain.SetLogger(l.WithField("context", "test"))

	chain.Stage("before error")
	chain.Add(func() error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	chain.Stage("error stage")
	chain.Add(func() error {
		return errors.New("boom")
	})

	chain.Stage("after error")
	chain.Add(func() error { return nil })

	err := chain.Exec()
	if err == nil {
		t.Fatal("expected error")
	}

	out := buf.String()
	if !strings.Contains(out, `stage "before error" took`) {
		t.Errorf("expected timing for stage before error, got:\n%s", out)
	}
	if strings.Contains(out, `stage "error stage" took`) {
		t.Errorf("unexpected timing for error stage, got:\n%s", out)
	}
}

func TestPipeline_StageTiming_NonFatalError(t *testing.T) {
	var buf bytes.Buffer
	l := log.New()
	l.SetOutput(&buf)
	l.SetLevel(log.TraceLevel)
	l.SetFormatter(&log.TextFormatter{DisableQuote: true})

	chain := New("test").Init(context.Background())
	chain.SetLogger(l.WithField("context", "test"))

	chain.Stage("warn stage")
	chain.Add(func() error {
		return MarkNonFatal(errors.New("soft error"))
	})

	chain.Stage("after warn")
	chain.Add(func() error { return nil })

	if err := chain.Exec(); err != nil {
		t.Fatalf("unexpected fatal error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `stage "warn stage" took`) {
		t.Errorf("expected timing for warn stage, got:\n%s", out)
	}
	if !strings.Contains(out, `stage "after warn" took`) {
		t.Errorf("expected timing for after warn stage, got:\n%s", out)
	}
}
