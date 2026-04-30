package terminal

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

var isTerminal = term.IsTerminal(int(os.Stdout.Fd()))

// ClearLine clears the previous line of the terminal
func ClearLine() {
	if !isTerminal {
		return
	}

	fmt.Print("\033[1A \033[2K \r")
}

// Progress returns a string of the progress
func Progress(current, total int64) string {
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f%%", float64(current)*100/float64(total))
}

// HumanBytes returns a human-readable byte size string.
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
