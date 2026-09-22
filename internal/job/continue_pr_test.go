package job

import "testing"

func TestParseContinuePRContext(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantRef string
		wantPR  bool
		active  bool
	}{
		{name: "empty", input: "", wantRef: "", wantPR: false, active: false},
		{
			name:    "colon form",
			input:   "continue_pr: true\npush_ref: devbox/uta-82-32\n",
			wantRef: "devbox/uta-82-32",
			wantPR:  true,
			active:  true,
		},
		{
			name:    "equals form case-insensitive",
			input:   "CONTINUE_PR=true\nPUSH_REF=devbox/uta-82-32",
			wantRef: "devbox/uta-82-32",
			wantPR:  true,
			active:  true,
		},
		{
			name:    "push_ref alone activates",
			input:   "Please continue.\npush_ref: feat/existing\n",
			wantRef: "feat/existing",
			wantPR:  false,
			active:  true,
		},
		{
			name:    "continue_pr alone does not activate",
			input:   "continue_pr: true\nreset onto existing tip",
			wantRef: "",
			wantPR:  true,
			active:  false,
		},
		{
			name:    "false continue_pr ignored",
			input:   "continue_pr: false\npush_ref: keep-me",
			wantRef: "keep-me",
			wantPR:  false,
			active:  true,
		},
		{
			name:    "inline prose with keys on own lines",
			input:   "Operator notes:\n\npush_ref = cos/old-branch\ncontinue_pr = yes\n\nDo the thing.",
			wantRef: "cos/old-branch",
			wantPR:  true,
			active:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseContinuePRContext(tt.input)
			if got.PushRef != tt.wantRef {
				t.Fatalf("PushRef=%q want %q", got.PushRef, tt.wantRef)
			}
			if got.ContinuePR != tt.wantPR {
				t.Fatalf("ContinuePR=%v want %v", got.ContinuePR, tt.wantPR)
			}
			if got.Active() != tt.active {
				t.Fatalf("Active()=%v want %v", got.Active(), tt.active)
			}
		})
	}
}
