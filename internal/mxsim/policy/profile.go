package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from "5m", "30s", "1h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.D().String(), nil }
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.D().String() + `"`), nil
}
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// RateRule is a sliding-window counter: at most Count events per Window,
// after which OnExceed is returned and the cooldown starts.
type RateRule struct {
	Count    int      `yaml:"count" json:"count"`
	Window   Duration `yaml:"window" json:"window"`
	OnExceed string   `yaml:"on_exceed" json:"on_exceed"`
}

func (r RateRule) enabled() bool { return r.Count > 0 && r.Window > 0 }

type Limits struct {
	MaxConcurrentConns  int      `yaml:"max_concurrent_conns" json:"max_concurrent_conns"`
	ConnRate            RateRule `yaml:"conn_rate" json:"conn_rate"`
	RcptRate            RateRule `yaml:"rcpt_rate" json:"rcpt_rate"`
	RcptPerConn         int      `yaml:"rcpt_per_conn" json:"rcpt_per_conn"`
	CooldownAfterExceed Duration `yaml:"cooldown_after_exceed" json:"cooldown_after_exceed"`
	// TooManyConns is returned when MaxConcurrentConns is exceeded.
	TooManyConns string `yaml:"too_many_conns" json:"too_many_conns"`
}

type Behaviour struct {
	TarpitBanner  Duration `yaml:"tarpit_banner" json:"tarpit_banner"`
	TarpitRcpt    Duration `yaml:"tarpit_rcpt" json:"tarpit_rcpt"`
	Greylist      bool     `yaml:"greylist" json:"greylist"`
	GreylistDelay Duration `yaml:"greylist_delay" json:"greylist_delay"`
	GreylistReply string   `yaml:"greylist_reply" json:"greylist_reply"`
	CatchAll      bool     `yaml:"catch_all" json:"catch_all"`
	RejectUnknown string   `yaml:"reject_unknown" json:"reject_unknown"`
	Accept        string   `yaml:"accept" json:"accept"`
	// TimeoutHold is how long a "timeout" recipient is left hanging before the
	// server gives up on the connection. The client should time out first.
	TimeoutHold Duration `yaml:"timeout_hold" json:"timeout_hold"`
	// IdleTimeout is the per-command read deadline.
	IdleTimeout Duration `yaml:"idle_timeout" json:"idle_timeout"`
}

// Recipients selects behaviour by local part. A trailing "@" in the config is
// tolerated ("valid@" == "valid") because that is how mail people write them.
type Recipients struct {
	Exists  []string `yaml:"exists" json:"exists"`
	Bounce  []string `yaml:"bounce" json:"bounce"`
	Timeout []string `yaml:"timeout" json:"timeout"`
	Drop    []string `yaml:"drop" json:"drop"`
}

type Chaos struct {
	TempErrorRate  float64 `yaml:"temp_error_rate" json:"temp_error_rate"`
	TempErrorReply string  `yaml:"temp_error_reply" json:"temp_error_reply"`
	DropRate       float64 `yaml:"drop_rate" json:"drop_rate"`
	Seed           int64   `yaml:"seed" json:"seed"`
}

type Profile struct {
	Name       string     `yaml:"name" json:"name"`
	Domains    []string   `yaml:"domains" json:"domains"`
	Listen     []string   `yaml:"listen" json:"listen"`
	Banner     string     `yaml:"banner" json:"banner"`
	EhloCaps   []string   `yaml:"ehlo_caps" json:"ehlo_caps"`
	Limits     Limits     `yaml:"limits" json:"limits"`
	Behaviour  Behaviour  `yaml:"behaviour" json:"behaviour"`
	Recipients Recipients `yaml:"recipients" json:"recipients"`
	Chaos      Chaos      `yaml:"chaos" json:"chaos"`
}

func normalizeLocals(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "@"))
		if s != "" {
			out[s] = true
		}
	}
	return out
}

// ApplyDefaults fills in everything the YAML left out, so a three-line profile
// is still a working MX.
func (p *Profile) ApplyDefaults() {
	if p.Banner == "" {
		p.Banner = "220 mx." + p.Name + ".test ESMTP mxsim"
	}
	if len(p.EhloCaps) == 0 {
		p.EhloCaps = []string{"PIPELINING", "SIZE 35882577", "8BITMIME"}
	}
	if p.Behaviour.Accept == "" {
		p.Behaviour.Accept = "250 2.1.5 OK"
	}
	if p.Behaviour.RejectUnknown == "" {
		p.Behaviour.RejectUnknown = "550 5.1.1 <%s>: Recipient address rejected: User unknown"
	}
	if p.Behaviour.GreylistReply == "" {
		p.Behaviour.GreylistReply = "450 4.2.0 Greylisted, try again later"
	}
	if p.Behaviour.TimeoutHold == 0 {
		p.Behaviour.TimeoutHold = Duration(60 * time.Second)
	}
	if p.Behaviour.IdleTimeout == 0 {
		p.Behaviour.IdleTimeout = Duration(120 * time.Second)
	}
	if p.Behaviour.Greylist && p.Behaviour.GreylistDelay == 0 {
		p.Behaviour.GreylistDelay = Duration(5 * time.Minute)
	}
	if p.Limits.TooManyConns == "" {
		p.Limits.TooManyConns = "421 4.7.0 Too many concurrent connections from this IP"
	}
	if p.Limits.ConnRate.enabled() && p.Limits.ConnRate.OnExceed == "" {
		p.Limits.ConnRate.OnExceed = "421 4.7.0 Too many connections, try again later"
	}
	if p.Limits.RcptRate.enabled() && p.Limits.RcptRate.OnExceed == "" {
		p.Limits.RcptRate.OnExceed = "421 4.7.0 Too many requests, try again later"
	}
	if p.Chaos.TempErrorReply == "" {
		p.Chaos.TempErrorReply = "451 4.3.0 Temporary server error, try again later"
	}
	if p.Chaos.Seed == 0 {
		p.Chaos.Seed = 1337
	}
	for i, d := range p.Domains {
		p.Domains[i] = strings.ToLower(strings.TrimSpace(d))
	}
	sort.Strings(p.Domains)
}

func (p *Profile) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("profile has no name")
	}
	if len(p.Listen) == 0 {
		return fmt.Errorf("profile %q has no listen address", p.Name)
	}
	if p.Chaos.TempErrorRate < 0 || p.Chaos.TempErrorRate > 1 {
		return fmt.Errorf("profile %q: temp_error_rate must be within 0..1", p.Name)
	}
	if p.Chaos.DropRate < 0 || p.Chaos.DropRate > 1 {
		return fmt.Errorf("profile %q: drop_rate must be within 0..1", p.Name)
	}
	return nil
}

// ParseProfile decodes a profile from YAML. JSON is valid YAML, so the admin
// API accepts either. Unknown fields are an error: a typo in a limit name
// would otherwise silently disable the limit under test.
func ParseProfile(b []byte, defaultName string) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.Name == "" {
		p.Name = defaultName
	}
	p.ApplyDefaults()
	return &p, nil
}

// LoadProfile reads one YAML profile from disk.
func LoadProfile(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	p, err := ParseProfile(b, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// LoadDir reads every *.yaml in dir, sorted by name for deterministic startup.
func LoadDir(dir string) ([]*Profile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	more, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, more...)
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no profiles found in %s", dir)
	}
	out := make([]*Profile, 0, len(paths))
	seen := map[string]string{}
	for _, p := range paths {
		prof, err := LoadProfile(p)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[prof.Name]; dup {
			return nil, fmt.Errorf("duplicate profile name %q (%s and %s)", prof.Name, prev, p)
		}
		seen[prof.Name] = p
		out = append(out, prof)
	}
	return out, nil
}
