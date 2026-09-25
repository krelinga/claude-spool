package claudecli

import (
	"errors"
	"regexp"
	"strings"
)

// The re-login flow, as the CLI actually behaves (spike probe 4, CLI 2.1.282).
// `claude auth login` under a PTY prints:
//
//	Opening browser to sign in…
//	If the browser didn't open, visit: <OSC8 link>https://claude.com/cai/oauth/authorize?...<reset>
//	Paste code here if prompted > Login successful.
//
// Three details matter for driving it programmatically:
//
//  1. The URL is emitted twice on one line — once inside an OSC 8 hyperlink
//     escape and once as visible text — so the escapes must be stripped before
//     matching, or every read yields two "different" URLs.
//  2. The prompt has no trailing newline, so a reader waiting for one blocks
//     forever. Detect the prompt as a substring.
//  3. The code is read without echo: it never appears in the transcript. So
//     success can only be confirmed from the "Login successful." line and the
//     exit status, not by reading back what was written.
const (
	// LoginPromptMarker is the (newline-free) prompt that means the CLI is
	// waiting for the authorization code.
	LoginPromptMarker = "Paste code here"
	// LoginSuccessMarker is printed once the code is accepted.
	LoginSuccessMarker = "Login successful."
)

var (
	// oscSequence matches an OSC escape (ESC ] ... terminated by BEL or ST),
	// which is how the CLI wraps the URL as a clickable hyperlink.
	oscSequence = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	// csiSequence matches a CSI escape (ESC [ ... final byte), the colour codes.
	csiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

	// loginURLRe matches the authorization URL. Deliberately anchored on the
	// oauth path rather than "any https URL", so a stray link in other output
	// cannot be mistaken for it.
	loginURLRe = regexp.MustCompile(`https://[a-zA-Z0-9.-]+/[^\s"'<>]*oauth/authorize\?[^\s"'<>\x1b\x07]+`)
)

// ErrNoLoginURL means the authorization URL has not appeared yet.
var ErrNoLoginURL = errors.New("no authorization URL in output")

// StripANSI removes the escape sequences the CLI uses for links and colour.
func StripANSI(s string) string {
	s = oscSequence.ReplaceAllString(s, "")
	return csiSequence.ReplaceAllString(s, "")
}

// ParseLoginURL extracts the authorization URL from accumulated PTY output.
//
// It is safe to call repeatedly as output arrives: it returns ErrNoLoginURL
// until the line has been seen in full.
func ParseLoginURL(output string) (string, error) {
	m := loginURLRe.FindString(StripANSI(output))
	if m == "" {
		return "", ErrNoLoginURL
	}
	// The URL is the last thing on its line; trim anything the terminal glued on.
	return strings.TrimRight(m, ".,);"), nil
}

// AwaitingCode reports whether the CLI is prompting for the code.
func AwaitingCode(output string) bool {
	return strings.Contains(StripANSI(output), LoginPromptMarker)
}

// LoginSucceeded reports whether the CLI confirmed the login.
func LoginSucceeded(output string) bool {
	return strings.Contains(StripANSI(output), LoginSuccessMarker)
}
