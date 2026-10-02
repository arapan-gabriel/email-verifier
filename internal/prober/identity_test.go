package prober

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Plan 030: a refusal of us is never a verdict. The table is the plan's own,
// each reply as the warm-up ladder received it.

// prodIdentity is the deployed node's identity (`probe.source_ip`, `probe.helo`,
// the MAIL FROM domain).
var prodIdentity = Identity{
	SourceIP:       "92.222.87.97",
	Helo:           "mail.datascoutmail.com",
	MailFromDomain: "probe.datascoutmail.com",
}

func TestARefusalOfUsIsNeverAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		text string
	}{
		{"tls for our ip (day 16)", 550, "550 TLS encryption required for mails from 92.222.87.97"},
		{"must use tls (day 17)", 550, "550 Must use TLS"},
		{"our ip not allowed", 550, "550 Your IP 92.222.87.97 is not allowed to send mail here"},
		{"t-online dialup, at RCPT", 554, "554 IP=92.222.87.97 - Dialup/transient IP not allowed. Use a mailgateway or contact tobr@rx.t-online.de"},
		{"magicspam", 550, "550 Error: listed as abusive by MagicSpam"},
		{"namemaster access rule", 551, "551 5.7.1 <mail.datascoutmail.com[92.222.87.97]>: Client host rejected: Error Protokoll (DSGVO)"},
		{"invalid rdns", 550, "550 Invalid RDNS entry for 92.222.87.97"},
		{"encryption needed (day 14)", 550, "550 Encryption needed"},
		{"mimecast route (day 14)", 553, "553 This route requires encryption (TLS)"},
		{"our helo named", 550, "550 Host mail.datascoutmail.com is not permitted to relay"},
		{"our sender domain named", 550, "550 Mail from probe.datascoutmail.com refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAs(tc.code, tc.text, prodIdentity); got != ClassPolicy {
				t.Errorf("ClassifyAs(%d, %q) = %s, want policy", tc.code, tc.text, got)
			}
		})
	}
}

// Explicit mailbox wording still wins: IONOS's "mailbox unavailable" carries a
// case link with our IP in it (case r1601 = the recipient does not exist).
func TestMailboxWordingStillWinsOverOurIdentity(t *testing.T) {
	for _, tc := range []struct {
		code int
		text string
	}{
		{550, "550 Requested action not taken: mailbox unavailable\n550 invalid DNS MX or A/AAAA resource record. For explanation visit https://postmaster.1und1.de/en/case?c=r1601&i=ip&v=92.222.87.97&r=1N5TcC"},
		// A recipient code has already said who the reply is about, even when
		// the server appends our address to it.
		{550, "550 5.1.1 <x@y.de>: Recipient address rejected [client 92.222.87.97]"},
	} {
		if got := ClassifyAs(tc.code, tc.text, prodIdentity); got != ClassInvalid {
			t.Errorf("ClassifyAs(%d, %q) = %s, want invalid", tc.code, tc.text, got)
		}
	}
}

// A tenant's directory-based edge blocking is a tenant setting, not us and not
// the mailbox: still policy, by its code.
func TestMicrosoft541StaysPolicy(t *testing.T) {
	text := "550 5.4.1 Recipient address rejected: Access denied. For more information see https://aka.ms/EXOSmtpErrors"
	if got := ClassifyAs(550, text, prodIdentity); got != ClassPolicy {
		t.Errorf("5.4.1 = %s, want policy", got)
	}
}

func TestIdentityMatchesWholeTokensOnly(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"ip=92.222.87.97 - dialup", true},
		{"<mail.datascoutmail.com[92.222.87.97]>", true},
		{"mails from 92.222.87.97", true},
		{"mails from 92.222.87.97.", true}, // end of a sentence
		{"client 192.222.87.97 rejected", false},
		{"client 92.222.87.971 rejected", false},
		{"client 92.222.87.97.5 rejected", false},
		{"host xmail.datascoutmail.com", false},
		{"host mail.datascoutmail.com.evil.net", false},
		{"nothing of ours here", false},
	} {
		if got := prodIdentity.namedIn(tc.text); got != tc.want {
			t.Errorf("namedIn(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	if (Identity{}).namedIn("anything 92.222.87.97") {
		t.Error("an empty identity matched")
	}
}

// Without our identity the same "not allowed" reply has no sender wording and
// stays what it was — which is exactly why the identity is needed.
func TestTheIdentityIsWhatCatchesTheUnlistedRefusal(t *testing.T) {
	text := "550 Your IP 92.222.87.97 is not allowed to send mail here"
	if got := Classify(550, text); got != ClassInvalid {
		t.Fatalf("without identity = %s; the test no longer shows the gap it covers", got)
	}
	if got := ClassifyAs(550, text, prodIdentity); got != ClassPolicy {
		t.Errorf("with identity = %s, want policy", got)
	}
}

// A 421 that demands TLS is a refusal of our session. Read as a throttle it
// halved the pacer's rate for a host that refuses us at any rate.
func TestATLSDemandIsPolicyAndNeverAThrottle(t *testing.T) {
	for _, text := range []string{
		"421 4.7.0 STARTTLS is mandatory",
		"421 4.7.0 TLS minimum version not met",
		"454 4.7.0 TLS not available due to local problem",
	} {
		code, _ := strconv.Atoi(text[:3])
		c := ClassifyAs(code, text, prodIdentity)
		if c != ClassPolicy || c.IsThrottle() {
			t.Errorf("%q = %s (throttle=%v), want policy, not a throttle", text, c, c.IsThrottle())
		}
	}
	// A plain 421 is still the rate signal it always was.
	if c := ClassifyAs(421, "421 4.7.0 Too many connections, slow down", prodIdentity); c != ClassThrottled {
		t.Errorf("plain 421 = %s, want throttled", c)
	}
	// Greylisting still wins over any wording.
	if c := ClassifyAs(451, "451 4.7.1 Greylisted, TLS session noted", prodIdentity); c != ClassDeferred {
		t.Errorf("greylist with tls = %s, want deferred", c)
	}
}

// End to end: the pacer is never told a 421 TLS demand was a throttle.
func TestA421TLSDemandNeverReachesTheThrottleSignal(t *testing.T) {
	pc := &recordingPacer{}
	d := scriptedMX("220 ok", func(cmd string) string {
		if strings.HasPrefix(cmd, "RCPT TO") {
			return "421 4.7.0 STARTTLS is mandatory"
		}
		return "250 ok"
	})
	p := New(Options{Dialer: d, Resolver: stubResolver{}, Pacer: pc, Timeout: 5 * time.Second,
		Helo: prodIdentity.Helo, MailFrom: "verify@" + prodIdentity.MailFromDomain, SourceIP: prodIdentity.SourceIP})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := pc.throttles(); n != 0 {
		t.Errorf("pacer saw %d throttle signals for a TLS demand", n)
	}
	r := resp.Results["a@example.test"]
	if r.Class != ClassPolicy || r.Accepted != nil {
		t.Errorf("result = %+v, want policy with no verdict", r)
	}
}

// The prober builds its identity from its own options.
func TestTheProberRecognisesItsOwnAddressAtRCPT(t *testing.T) {
	d := scriptedMX("220 ok", func(cmd string) string {
		if strings.HasPrefix(cmd, "RCPT TO") {
			return "550 Your IP 92.222.87.97 is not allowed to send mail here"
		}
		return "250 ok"
	})
	p := New(Options{Dialer: d, Resolver: stubResolver{}, Pacer: &recordingPacer{}, Timeout: 5 * time.Second,
		Helo: prodIdentity.Helo, MailFrom: "verify@" + prodIdentity.MailFromDomain, SourceIP: prodIdentity.SourceIP})
	resp, err := p.Probe(t.Context(), Request{MXHost: "mx.test", Domain: "example.test", Emails: []string{"a@example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := resp.Results["a@example.test"]; r.Class != ClassPolicy || r.Accepted != nil {
		t.Errorf("result = %+v, want policy with no verdict", r)
	}
}

// --- the corpus: every permanent reply the ladder received, classified as committed ---

var updateCorpus = flag.Bool("update-corpus", false, "rewrite testdata/corpus.tsv's expected column")

type corpusRow struct {
	code     int
	expected string
	rest     string // recorded, rows, reply — kept verbatim
	reply    string
}

func unescape(s string) string {
	r := strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\t`, "\t")
	return r.Replace(s)
}

func readCorpus(t *testing.T) (header []string, rows []corpusRow) {
	t.Helper()
	f, err := os.Open("testdata/corpus.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			header = append(header, line)
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) != 5 {
			t.Fatalf("malformed corpus line %q", line)
		}
		code, err := strconv.Atoi(parts[0])
		if err != nil {
			t.Fatalf("bad code in %q", line)
		}
		rows = append(rows, corpusRow{code: code, expected: parts[1],
			rest: strings.Join(parts[2:], "\t"), reply: unescape(parts[4])})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return header, rows
}

func TestCorpusClassifiesExactlyAsCommitted(t *testing.T) {
	header, rows := readCorpus(t)
	if len(rows) < 300 {
		t.Fatalf("corpus has %d rows; it should be the ladder's whole record", len(rows))
	}
	if *updateCorpus {
		var b strings.Builder
		for _, h := range header {
			b.WriteString(h + "\n")
		}
		for _, r := range rows {
			fmt.Fprintf(&b, "%d\t%s\t%s\n", r.code, ClassifyAs(r.code, r.reply, prodIdentity), r.rest)
		}
		if err := os.WriteFile("testdata/corpus.tsv", []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, r := range rows {
		got := string(ClassifyAs(r.code, r.reply, prodIdentity))
		if got != r.expected {
			t.Errorf("%d %q: %s, committed %s", r.code, firstLine(r.reply), got, r.expected)
		}
	}
}

// A reply naming our address never comes back invalid unless a recipient code
// or mailbox wording says so — checked over the whole real corpus.
func TestNoCorpusReplyNamingUsIsInvalidWithoutMailboxEvidence(t *testing.T) {
	_, rows := readCorpus(t)
	for _, r := range rows {
		low := strings.ToLower(r.reply)
		ec := EnhancedCode(low)
		if !prodIdentity.namedIn(low) || hasAny(low, mailboxHints) || subject(ec) == 1 || subject(ec) == 2 {
			continue
		}
		if c := ClassifyAs(r.code, r.reply, prodIdentity); c == ClassInvalid {
			t.Errorf("%q names us and is invalid", firstLine(r.reply))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
