package cloud

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/j3ssie/osmedeus/v5/internal/terminal"
)

const pulumiPrefix = "  │ "

// PulumiWriter is an io.Writer that colorizes Pulumi progress output
type PulumiWriter struct {
	out io.Writer
	buf []byte // buffer for incomplete lines
}

// NewPulumiWriter creates a writer that colorizes Pulumi output lines
func NewPulumiWriter() *PulumiWriter {
	return &PulumiWriter{out: os.Stdout}
}

func (w *PulumiWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.buf = append(w.buf, p...)

	for {
		idx := bytes.IndexByte(w.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]

		colored := colorizePulumiLine(line)
		if _, err := fmt.Fprintf(w.out, "%s%s\n", pulumiPrefix, colored); err != nil {
			return n, err
		}
	}

	return n, nil
}

// Flush writes any remaining buffered content
func (w *PulumiWriter) Flush() {
	if len(w.buf) > 0 {
		colored := colorizePulumiLine(string(w.buf))
		_, _ = fmt.Fprintf(w.out, "%s%s\n", pulumiPrefix, colored)
		w.buf = nil
	}
}

func colorizePulumiLine(line string) string {
	trimmed := strings.TrimSpace(line)

	// Empty lines pass through
	if trimmed == "" {
		return ""
	}

	// Resource operations: " +  type name action"
	if strings.HasPrefix(trimmed, "+") {
		return terminal.Green(line)
	}
	if strings.HasPrefix(trimmed, "~") {
		return terminal.Yellow(line)
	}
	if strings.HasPrefix(trimmed, "-") {
		return terminal.Red(line)
	}
	// Replace operations show as "+-"
	if strings.HasPrefix(trimmed, "++") || strings.HasPrefix(trimmed, "+-") {
		return terminal.Yellow(line)
	}

	// Progress spinner lines: "@ updating...."
	if strings.HasPrefix(trimmed, "@") {
		return terminal.Gray(line)
	}

	// Section headers
	if strings.HasPrefix(trimmed, "Updating") ||
		strings.HasPrefix(trimmed, "Destroying") ||
		strings.HasPrefix(trimmed, "Previewing") ||
		strings.HasPrefix(trimmed, "Refreshing") {
		return terminal.BoldCyan(line)
	}

	// Output/Resource summary headers
	if strings.HasPrefix(trimmed, "Outputs:") || strings.HasPrefix(trimmed, "Resources:") {
		return terminal.Bold(line)
	}

	// Duration line
	if strings.HasPrefix(trimmed, "Duration:") {
		return terminal.Cyan(line)
	}

	// Resource count summary lines like "+ 4 created", "~ 1 updated", "- 2 deleted"
	if (strings.HasPrefix(trimmed, "+ ") || strings.HasPrefix(trimmed, "~ ") || strings.HasPrefix(trimmed, "- ")) &&
		(strings.Contains(trimmed, "created") || strings.Contains(trimmed, "updated") || strings.Contains(trimmed, "deleted") || strings.Contains(trimmed, "unchanged")) {
		switch trimmed[0] {
		case '+':
			return terminal.Green(line)
		case '~':
			return terminal.Yellow(line)
		case '-':
			return terminal.Red(line)
		}
	}

	// Suppress "run pulumi stack rm" hint — we call RemoveStack automatically
	if strings.Contains(trimmed, "pulumi stack rm") ||
		strings.Contains(trimmed, "the history and configuration associated with the stack are still maintained") {
		return ""
	}

	// Diagnostics / errors
	if strings.HasPrefix(trimmed, "error:") || strings.HasPrefix(trimmed, "Error:") {
		return terminal.Red(line)
	}
	if strings.HasPrefix(trimmed, "warning:") || strings.HasPrefix(trimmed, "Warning:") {
		return terminal.Yellow(line)
	}

	// Output key-value pairs (indented under Outputs:)
	// Default: pass through unchanged
	return line
}
