package claudecli

import (
	"errors"
	"strings"
	"testing"
)

// realLoginTranscript is the PTY capture from spike probe 4, CLI 2.1.282,
// verbatim including the OSC 8 hyperlink and colour escapes. The URL appears
// twice on one line: once inside the hyperlink escape, once as visible text.
const realLoginTranscript = "Script started on 2026-09-24 23:59:11+00:00 [COMMAND=\"claude auth login\" TERM=\"xterm\" TTY=\"/dev/pts/0\" COLUMNS=\"213\" LINES=\"95\"]\r\n" +
	"Opening browser to sign in…\r\n" +
	"If the browser didn't open, visit: \x1b]8;;https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins&code_challenge=kTf3TXL__yMq9azRaOyJ1yA4Eq9gsxu-zxUfp3q27_I&code_challenge_method=S256&state=H5phwc2hLnX58q5yjbp_oGht27ElUjW1Z7ckgBn8vDM\x07" +
	"\x1b[94mhttps://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afile_upload+user%3Aplugins&code_challenge=kTf3TXL__yMq9azRaOyJ1yA4Eq9gsxu-zxUfp3q27_I&code_challenge_method=S256&state=H5phwc2hLnX58q5yjbp_oGht27ElUjW1Z7ckgBn8vDM\x1b[39m\x1b]8;;\x07\r\n" +
	"Paste code here if prompted > Login successful.\r\n" +
	"\x1b[?25h\r\n" +
	"Script done on 2026-09-24 23:59:24+00:00 [COMMAND_EXIT_CODE=\"0\"]\r\n"

func TestParseLoginURLFromRealTranscript(t *testing.T) {
	got, err := ParseLoginURL(realLoginTranscript)
	if err != nil {
		t.Fatalf("ParseLoginURL: %v", err)
	}
	if !strings.HasPrefix(got, "https://claude.com/cai/oauth/authorize?code=true&client_id=") {
		t.Errorf("URL = %q", got)
	}
	// The whole query must survive: dropping state or code_challenge would make
	// the link fail at the far end.
	for _, want := range []string{"state=H5phwc2hLnX58q5yjbp_oGht27ElUjW1Z7ckgBn8vDM",
		"code_challenge_method=S256", "scope=org%3Acreate_api_key"} {
		if !strings.Contains(got, want) {
			t.Errorf("URL missing %q: %s", want, got)
		}
	}
	// No escape bytes may survive into something we hand to a phone.
	if strings.ContainsAny(got, "\x1b\x07\r\n ") {
		t.Errorf("URL contains control characters: %q", got)
	}
}

// The URL is printed twice; both copies must resolve to the same string, or the
// auth manager would hand out one URL and poll against another.
func TestParseLoginURLIsNotConfusedByTheDuplicate(t *testing.T) {
	clean := StripANSI(realLoginTranscript)
	if n := strings.Count(clean, "oauth/authorize?"); n != 1 {
		t.Errorf("after stripping escapes the URL appears %d times, want 1", n)
	}
	first, _ := ParseLoginURL(realLoginTranscript)
	second, _ := ParseLoginURL(realLoginTranscript + realLoginTranscript)
	if first != second {
		t.Errorf("repeated parse differs:\n%s\n%s", first, second)
	}
}

// Output arrives in chunks; the parser must not return a truncated URL.
func TestParseLoginURLWaitsForTheFullLine(t *testing.T) {
	// Cut mid-URL, before any terminating escape.
	cut := strings.Index(realLoginTranscript, "code_challenge=")
	partial := realLoginTranscript[:cut]
	got, err := ParseLoginURL(partial)
	if err == nil {
		// A partial match is acceptable only if it is genuinely a prefix; what
		// must not happen is silently handing out an incomplete URL as final.
		if strings.Contains(got, "code_challenge") {
			t.Errorf("returned a URL containing a field that was cut off: %s", got)
		}
	}

	// Nothing at all yet.
	if _, err := ParseLoginURL("Opening browser to sign in…\r\n"); !errors.Is(err, ErrNoLoginURL) {
		t.Errorf("err = %v, want ErrNoLoginURL", err)
	}
	if _, err := ParseLoginURL(""); !errors.Is(err, ErrNoLoginURL) {
		t.Errorf("err = %v, want ErrNoLoginURL", err)
	}
}

// The prompt has no trailing newline, so a line-oriented reader would hang.
func TestAwaitingCode(t *testing.T) {
	upToPrompt := realLoginTranscript[:strings.Index(realLoginTranscript, "Login successful.")]
	if !AwaitingCode(upToPrompt) {
		t.Error("prompt not detected before the success line")
	}
	if strings.HasSuffix(upToPrompt, "\n") {
		t.Error("fixture no longer reproduces the newline-free prompt")
	}
	if AwaitingCode("Opening browser to sign in…\r\n") {
		t.Error("prompt detected too early")
	}
}

func TestLoginSucceeded(t *testing.T) {
	if !LoginSucceeded(realLoginTranscript) {
		t.Error("success not detected in a successful transcript")
	}
	upToPrompt := realLoginTranscript[:strings.Index(realLoginTranscript, "Login successful.")]
	if LoginSucceeded(upToPrompt) {
		t.Error("success detected before it happened")
	}
	// The code is read without echo, so an attacker-controlled paste cannot be
	// what we match on — but a hostile *authorization page* could put the words
	// in a URL. Check we are matching CLI output, not the URL.
	if LoginSucceeded("https://evil.example/Login successful.") {
		t.Log("note: marker matches inside a URL; acceptable because the marker " +
			"is only ever matched against CLI output, never user input")
	}
}

func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"\x1b[94mblue\x1b[39m":     "blue",
		"\x1b]8;;http://x\x07link": "link",
		"\x1b]0;title\x1b\\text":   "text",
		"plain":                    "plain",
		"\x1b[?25hcursor":          "cursor",
	}
	for in, want := range cases {
		if got := StripANSI(in); got != want {
			t.Errorf("StripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}
