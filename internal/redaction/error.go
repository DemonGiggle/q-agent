package redaction

import "io"

type maskedError struct {
	cause error
	text  string
}

func (e maskedError) Error() string { return e.text }
func (e maskedError) Unwrap() error { return e.cause }
func (p *Policy) Error(s Sink, err error) error {
	if err == nil {
		return nil
	}
	text := p.Text(s, err.Error())
	if text == err.Error() {
		return err
	}
	return maskedError{err, text}
}

// DiagnosticWriter handles complete diagnostic writes; provider streams use Stream.
type DiagnosticWriter struct {
	Policy *Policy
	Sink   Sink
	Out    io.Writer
}

func (w *DiagnosticWriter) Write(data []byte) (int, error) {
	_, err := io.WriteString(w.Out, w.Policy.Text(w.Sink, string(data)))
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
