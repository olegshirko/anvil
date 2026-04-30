package terminal

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"golang.org/x/term"
)

var _ io.WriteCloser = (*scrollWriter)(nil)

// scrollWriter renders the last N lines to the terminal, overwriting previous output.
type scrollWriter struct {
	buffer     bytes.Buffer
	history    []string
	maxLines   int
	width      int
	extraLines int
	lastResize time.Time
}

// NewVerboseWriter creates a writer that keeps the most recent `lineHeight` lines on screen.
func NewVerboseWriter(lineHeight int) io.WriteCloser {
	return &scrollWriter{maxLines: lineHeight}
}

func (s *scrollWriter) Write(p []byte) (int, error) {
	if !isTerminal {
		return os.Stdout.Write(p)
	}

	for i, b := range p {
		if b != '\n' {
			s.buffer.WriteByte(b)
			continue
		}
		if err := s.redraw(); err != nil {
			return i + 1, err
		}
	}
	return len(p), nil
}

func (s *scrollWriter) Close() error {
	if s.buffer.Len() > 0 {
		if err := s.redraw(); err != nil {
			return err
		}
	}
	s.erase()
	return nil
}

func (s *scrollWriter) redraw() error {
	s.erase()
	s.pushLine()
	return s.render()
}

func (s *scrollWriter) pushLine() {
	defer s.buffer.Reset()

	if s.maxLines <= 0 {
		s.emitVerbose(s.buffer.String())
		return
	}

	if len(s.history) >= s.maxLines {
		s.history = s.history[1:]
	}
	s.history = append(s.history, s.buffer.String())
}

func (s *scrollWriter) emitVerbose(text string) {
	line := s.cleanLine(text)
	line = color.HiBlackString(line)
	_, _ = fmt.Fprintln(os.Stderr, line)
}

func (s scrollWriter) cleanLine(text string) string {
	// Strip logrus-style prefix noise.
	if strings.HasPrefix(text, "time=") && strings.Contains(text, "msg=") {
		text = text[strings.Index(text, "msg=")+4:]
		if unquoted, err := strconv.Unquote(text); err == nil {
			text = unquoted
		}
	}
	return "> " + text
}

func (s *scrollWriter) render() error {
	if err := s.measure(); err != nil {
		return err
	}

	s.extraLines = 0
	for _, line := range s.history {
		line = s.cleanLine(line)
		if len(line) > s.width {
			s.extraLines += len(line) / s.width
			if len(line)%s.width == 0 {
				s.extraLines--
			}
		}
		line = color.HiBlackString(line)
		fmt.Println(line)
	}
	return nil
}

func (s *scrollWriter) erase() {
	for i := 0; i < len(s.history)+s.extraLines; i++ {
		ClearLine()
	}
}

func (s *scrollWriter) measure() error {
	if time.Since(s.lastResize) < 2*time.Second {
		return nil
	}
	s.lastResize = time.Now().UTC()

	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return fmt.Errorf("cannot read terminal size: %w", err)
	}
	if w <= 0 {
		w = 80
	}
	s.width = w
	return nil
}
