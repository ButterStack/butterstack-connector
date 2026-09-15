package tools

import (
	"errors"
	"testing"
)

// Under -Mj the p4 client writes its error records to stdout as JSON, not to
// stderr. Reading stderr alone left every tool failure reported as the bare
// wait status, "perforce: exit status 1" (issue #9). These records are captured
// from real p4 output.
func TestFailureDetail(t *testing.T) {
	const (
		expiredTicket = `{"data":"Your session has expired, please login again.\n","generic":13,"severity":3,"code":"error"}` + "\n"
		missingTrust  = `{"data":"The authenticity of '10.0.0.4:1666' can't be established,\nthis may be your first attempt to connect to this P4PORT.\n","generic":38,"severity":3,"code":"error"}` + "\n"
	)

	tests := []struct {
		name   string
		stdout string
		stderr string
		runErr error
		want   string
	}{
		{
			name:   "an error record on stdout is preferred over the wait status",
			stdout: expiredTicket,
			runErr: errors.New("exit status 1"),
			want:   "Your session has expired, please login again.",
		},
		{
			name:   "a multi-line record is reported by its first line",
			stdout: missingTrust,
			runErr: errors.New("exit status 1"),
			want:   "The authenticity of '10.0.0.4:1666' can't be established,",
		},
		{
			// A failing command can emit informational records before the error.
			name: "an error record later in the stream is still found",
			stdout: `{"data":"//depot/art/tex.png#1 - opened for add\n","generic":0,"severity":0,"code":"info"}` + "\n" +
				`{"data":"Submit aborted -- fix problems then use 'p4 submit -c 42'.\n","generic":38,"severity":3,"code":"error"}` + "\n",
			runErr: errors.New("exit status 1"),
			want:   "Submit aborted -- fix problems then use 'p4 submit -c 42'.",
		},
		{
			// An info-severity record is not a failure description; falling back
			// to stderr beats reporting "opened for add" as the cause.
			name:   "informational records alone do not masquerade as the error",
			stdout: `{"data":"//depot/art/tex.png#1 - opened for add\n","generic":0,"severity":0,"code":"info"}` + "\n",
			stderr: "Connect to server failed; check $P4PORT.",
			runErr: errors.New("exit status 1"),
			want:   "Connect to server failed; check $P4PORT.",
		},
		{
			// Some p4 versions render severity as a quoted string.
			name:   "severity is read whether p4 renders it as a number or a string",
			stdout: `{"data":"Access for user 'ryan' has not been enabled by 'p4 protect'.\n","severity":"3","code":"error"}` + "\n",
			runErr: errors.New("exit status 1"),
			want:   "Access for user 'ryan' has not been enabled by 'p4 protect'.",
		},
		{
			// Connection-level failures never get far enough to produce a record.
			name:   "stderr is used when stdout carries no record",
			stderr: "Perforce client error:\n\tConnect to server failed",
			runErr: errors.New("exit status 1"),
			want:   "Perforce client error:",
		},
		{
			name:   "the wait status is the last resort, not the first",
			runErr: errors.New("exit status 1"),
			want:   "exit status 1",
		},
		{
			// This runs on a path that is already failing; it must never turn
			// one failure into two.
			name:   "truncated JSON degrades to stderr rather than panicking",
			stdout: `{"data":"Your session has exp`,
			stderr: "something went wrong",
			runErr: errors.New("exit status 1"),
			want:   "something went wrong",
		},
		{
			name:   "a record with no data field is skipped",
			stdout: `{"generic":38,"severity":3,"code":"error"}` + "\n",
			stderr: "fallback",
			runErr: errors.New("exit status 1"),
			want:   "fallback",
		},
		{
			// p4 is not perfectly consistent about emitting severity; the text
			// is what the caller needs either way.
			name:   "a data record with no severity is treated as the error",
			stdout: `{"data":"Client 'bsg-cp-01' unknown - use 'client' command to create it.\n"}` + "\n",
			runErr: errors.New("exit status 1"),
			want:   "Client 'bsg-cp-01' unknown - use 'client' command to create it.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := failureDetail([]byte(tc.stdout), tc.stderr, tc.runErr)
			if got != tc.want {
				t.Errorf("failureDetail()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
